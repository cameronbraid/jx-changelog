package changelog

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/jenkins-x-plugins/jx-changelog/pkg/gits"
	"github.com/jenkins-x-plugins/jx-changelog/pkg/helmhelpers"
	"github.com/jenkins-x-plugins/jx-changelog/pkg/issues"
	"github.com/jenkins-x-plugins/jx-changelog/pkg/users"
	"github.com/jenkins-x/go-scm/scm"
	jxc "github.com/jenkins-x/jx-api/v3/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx-helpers/v3/pkg/builds"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/cli"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/giturl"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/activities"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/jxclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"

	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube"
	"github.com/jenkins-x/jx-helpers/v3/pkg/kube/naming"

	"github.com/pkg/errors"

	"github.com/ghodss/yaml"

	jenkinsio "github.com/jenkins-x/jx-api/v3/pkg/apis/jenkins.io"
	v1 "github.com/jenkins-x/jx-api/v3/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/spf13/cobra"
	"gopkg.in/src-d/go-git.v4/plumbing/object"

	chgit "github.com/antham/chyle/chyle/git"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Options contains the command line flags
type Options struct {
	options.BaseOptions

	ScmFactory    scmhelpers.Options
	GitClient     gitclient.Interface
	CommandRunner cmdrunner.CommandRunner
	JXClient      jxc.Interface

	Namespace           string
	BuildNumber         string
	PreviousRevision    string
	PreviousDate        string
	CurrentRevision     string
	TemplatesDir        string
	ReleaseYamlFile     string
	CrdYamlFile         string
	Version             string
	Build               string
	Header              string
	HeaderFile          string
	Footer              string
	FooterFile          string
	OutputMarkdownFile  string
	OverwriteCRD        bool
	GenerateCRD         bool
	GenerateReleaseYaml bool
	UpdateRelease       bool
	NoReleaseInDev      bool
	IncludeMergeCommits bool
	FailIfFindCommits   bool
	State               State
}

type State struct {
	Tracker         issues.IssueProvider
	FoundIssueNames map[string]bool
	LoggedIssueKind bool
	Release         *v1.Release
}

const (
	ReleaseName = `{{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}`

	SpecName    = `{{ .Chart.Name }}`
	SpecVersion = `{{ .Chart.Version }}`

	ReleaseCrdYaml = `apiVersion: apiextensions.k8s.io/v1beta1
kind: CustomResourceDefinition
metadata:
  creationTimestamp: 2018-02-24T14:56:33Z
  name: releases.jenkins.io
  resourceVersion: "557150"
  selfLink: /apis/apiextensions.k8s.io/v1beta1/customresourcedefinitions/releases.jenkins.io
  uid: e77f4e08-1972-11e8-988e-42010a8401df
spec:
  group: jenkins.io
  names:
    kind: Release
    listKind: ReleaseList
    plural: releases
    shortNames:
    - rel
    singular: release
    categories:
    - all
  scope: Namespaced
  version: v1`
)

var (
	info = termcolor.ColorInfo

	GitAccessDescription = `

By default jx commands look for a file '~/.jx/gitAuth.yaml' to find the API tokens for Git servers. You can use 'jx create git token' to create a Git token.

Alternatively if you are running this command inside a CI server you can use environment variables to specify the username and API token.
e.g. define environment variables GIT_USERNAME and GIT_API_TOKEN
`

	cmdLong = templates.LongDesc(`
		Generates a Changelog for the latest tag

		This command will generate a Changelog as markdown for the git commit range given. 
		If you are using GitHub it will also update the GitHub Release with the changelog. You can disable that by passing'--update-release=false'

		If you have just created a git tag this command will try default to the changes between the last tag and the previous one. You can always specify the exact Git references (tag/sha) directly via '--previous-rev' and '--rev'

		The changelog is generated by parsing the git commits. It will also detect any text like 'fixes #123' to link to issue fixes. You can also use Conventional Commits notation: https://conventionalcommits.org/ to get a nicer formatted changelog. e.g. using commits like 'fix:(my feature) this my fix' or 'feat:(cheese) something'

		This command also generates a Release Custom Resource Definition you can include in your helm chart to give metadata about the changelog of the application along with metadata about the release (git tag, url, commits, issues fixed etc). Including this metadata in a helm charts means we can do things like automatically comment on issues when they hit Staging or Production; or give detailed descriptions of what things have changed when using GitOps to update versions in an environment by referencing the fixed issues in the Pull Request.

		You can opt out of the release YAML generation via the '--generate-yaml=false' option
		
		To update the release notes on GitHub / Gitea this command needs a git API token.

`) + GitAccessDescription

	cmdExample = templates.Examples(`
		# generate a changelog on the current source
		jx step changelog

		# specify the version to use
		jx step changelog --version 1.2.3

		# specify the version and a header template
		jx step changelog --header-file docs/dev/changelog-header.md --version 1.2.3

`)

	GitHubIssueRegex = regexp.MustCompile(`(\#\d+)`)
	JIRAIssueRegex   = regexp.MustCompile(`[A-Z][A-Z]+-(\d+)`)
)

// NewCmdChangelogCreate creates the command and options
func NewCmdChangelogCreate() (*cobra.Command, *Options) {
	o := &Options{}
	cmd := &cobra.Command{
		Use:     "changelog",
		Short:   "Creates a changelog for a git tag",
		Aliases: []string{"changes"},
		Long:    cmdLong,
		Example: cmdExample,
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}

	cmd.Flags().StringVarP(&o.PreviousRevision, "previous-rev", "p", "", "the previous tag revision")
	cmd.Flags().StringVarP(&o.PreviousDate, "previous-date", "", "", "the previous date to find a revision in format 'MonthName dayNumber year'")
	cmd.Flags().StringVarP(&o.CurrentRevision, "rev", "", "", "the current tag revision")
	cmd.Flags().StringVarP(&o.TemplatesDir, "templates-dir", "t", "", "the directory containing the helm chart templates to generate the resources")
	cmd.Flags().StringVarP(&o.ReleaseYamlFile, "release-yaml-file", "", "release.yaml", "the name of the file to generate the Release YAML")
	cmd.Flags().StringVarP(&o.CrdYamlFile, "crd-yaml-file", "", "release-crd.yaml", "the name of the file to generate the Release CustomResourceDefinition YAML")
	cmd.Flags().StringVarP(&o.Version, "version", "v", "", "The version to release")
	cmd.Flags().StringVarP(&o.Build, "build", "", "", "The Build number which is used to update the PipelineActivity. If not specified its defaulted from  the '$BUILD_NUMBER' environment variable")
	cmd.Flags().StringVarP(&o.OutputMarkdownFile, "output-markdown", "", "", "The file to generate for the changelog output if not updating a Git provider release")
	cmd.Flags().BoolVarP(&o.OverwriteCRD, "overwrite", "o", false, "overwrites the Release CRD YAML file if it exists")
	cmd.Flags().BoolVarP(&o.GenerateCRD, "crd", "c", false, "Generate the CRD in the chart")
	cmd.Flags().BoolVarP(&o.GenerateReleaseYaml, "generate-yaml", "y", true, "Generate the Release YAML in the local helm chart")
	cmd.Flags().BoolVarP(&o.UpdateRelease, "update-release", "", true, "Should we update the release on the Git repository with the changelog")
	cmd.Flags().BoolVarP(&o.NoReleaseInDev, "no-dev-release", "", false, "Disables the generation of Release CRDs in the development namespace to track releases being performed")
	cmd.Flags().BoolVarP(&o.IncludeMergeCommits, "include-merge-commits", "", false, "Include merge commits when generating the changelog")
	cmd.Flags().BoolVarP(&o.FailIfFindCommits, "fail-if-no-commits", "", false, "Do we want to fail the build if we don't find any commits to generate the changelog")

	cmd.Flags().StringVarP(&o.Header, "header", "", "", "The changelog header in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&o.HeaderFile, "header-file", "", "", "The file name of the changelog header in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&o.Footer, "footer", "", "", "The changelog footer in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")
	cmd.Flags().StringVarP(&o.FooterFile, "footer-file", "", "", "The file name of the changelog footer in markdown for the changelog. Can use go template expressions on the ReleaseSpec object: https://golang.org/pkg/text/template/")

	//cmd.Flags().StringVarP(&o.Dir, "dir", "", "", "The directory of the Git repository. Defaults to the current working directory")
	o.ScmFactory.AddFlags(cmd)
	o.BaseOptions.AddBaseFlags(cmd)
	return cmd, o
}

func (o *Options) Validate() error {
	err := o.BaseOptions.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate base options")
	}

	err = o.ScmFactory.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to discover git repository")
	}

	o.JXClient, o.Namespace, err = jxclient.LazyCreateJXClientAndNamespace(o.JXClient, o.Namespace)
	if err != nil {
		return errors.Wrapf(err, "failed to create jx client")
	}

	return nil
}

func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate")
	}

	// lets enable batch mode if we detect we are inside a pipeline
	if !o.BatchMode && builds.GetBuildNumber() != "" {
		log.Logger().Info("Using batch mode as inside a pipeline")
		o.BatchMode = true
	}

	dir := o.ScmFactory.Dir

	previousRev := o.PreviousRevision
	if previousRev == "" {
		previousDate := o.PreviousDate
		if previousDate != "" {
			previousRev, err = gits.GetRevisionBeforeDateText(o.Git(), dir, previousDate)
			if err != nil {
				return fmt.Errorf("Failed to find commits before date %s: %s", previousDate, err)
			}
		}
	}
	if previousRev == "" {
		previousRev, _, err = gits.GetCommitPointedToByPreviousTag(o.Git(), dir)
		if err != nil {
			return err
		}
		if previousRev == "" {
			// lets assume we are the first release
			previousRev, err = gits.GetFirstCommitSha(o.Git(), dir)
			if err != nil {
				return errors.Wrap(err, "failed to find first commit after we found no previous releaes")
			}
			if previousRev == "" {
				log.Logger().Info("no previous commit version found so change diff unavailable")
				return nil
			}
		}
	}
	currentRev := o.CurrentRevision
	if currentRev == "" {
		currentRev, _, err = gits.GetCommitPointedToByLatestTag(o.Git(), dir)
		if err != nil {
			return err
		}
	}

	templatesDir := o.TemplatesDir
	dir = o.ScmFactory.Dir
	if templatesDir == "" {
		chartFile, err := helmhelpers.FindChart(dir)
		if err != nil {
			return fmt.Errorf("Could not find helm chart %s", err)
		}
		path, _ := filepath.Split(chartFile)
		templatesDir = filepath.Join(path, "templates")
	}
	err = os.MkdirAll(templatesDir, files.DefaultDirWritePermissions)
	if err != nil {
		return fmt.Errorf("Failed to create the templates directory %s due to %s", templatesDir, err)
	}

	log.Logger().Infof("Generating change log from git ref %s => %s", info(previousRev), info(currentRev))

	gitDir, gitConfDir, err := gitclient.FindGitConfigDir(dir)
	if err != nil {
		return err
	}
	if gitDir == "" || gitConfDir == "" {
		log.Logger().Warnf("No git directory could be found from dir %s", dir)
		return nil
	}

	gitInfo := o.ScmFactory.GitURL
	if gitInfo == nil {
		gitInfo, err = giturl.ParseGitURL(o.ScmFactory.SourceURL)
		if err != nil {
			return errors.Wrapf(err, "failed to parse git URL %s", o.ScmFactory.SourceURL)
		}
	}

	tracker, err := o.CreateIssueProvider()
	if err != nil {
		return err
	}
	o.State.Tracker = tracker

	o.State.FoundIssueNames = map[string]bool{}

	commits, err := chgit.FetchCommits(gitDir, previousRev, currentRev)
	if err != nil {
		if o.FailIfFindCommits {
			return err
		}
		log.Logger().Warnf("failed to find git commits between revision %s and %s due to: %s", previousRev, currentRev, err.Error())
	}
	if commits != nil {
		commits1 := *commits
		if len(commits1) > 0 {
			if strings.HasPrefix(commits1[0].Message, "release ") {
				// remove the release commit from the log
				tmp := commits1[1:]
				commits = &tmp
			}
		}
		log.Logger().Debugf("Found commits:")
		for _, commit := range *commits {
			log.Logger().Debugf("  commit %s", commit.Hash)
			log.Logger().Debugf("  Author: %s <%s>", commit.Author.Name, commit.Author.Email)
			log.Logger().Debugf("  Date: %s", commit.Committer.When.Format(time.ANSIC))
			log.Logger().Debugf("      %s\n\n\n", commit.Message)
		}
	}
	version := o.Version
	if version == "" {
		version = SpecVersion
	}

	release := &v1.Release{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Release",
			APIVersion: jenkinsio.GroupAndVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: ReleaseName,
			CreationTimestamp: metav1.Time{
				Time: time.Now(),
			},
			//ResourceVersion:   "1",
			DeletionTimestamp: &metav1.Time{},
		},
		Spec: v1.ReleaseSpec{
			Name:          SpecName,
			Version:       version,
			GitOwner:      gitInfo.Organisation,
			GitRepository: gitInfo.Name,
			GitHTTPURL:    gitInfo.HttpsURL(),
			GitCloneURL:   gitInfo.CloneURL,
			Commits:       []v1.CommitSummary{},
			Issues:        []v1.IssueSummary{},
			PullRequests:  []v1.IssueSummary{},
		},
	}

	scmClient := o.ScmFactory.ScmClient
	resolver := users.GitUserResolver{
		GitProvider: scmClient,
	}
	if commits != nil {
		for _, commit := range *commits {
			c := commit
			if o.IncludeMergeCommits || len(commit.ParentHashes) <= 1 {
				o.addCommit(&release.Spec, &c, &resolver)
			}
		}
	}

	release.Spec.DependencyUpdates = CollapseDependencyUpdates(release.Spec.DependencyUpdates)

	// lets try to update the release
	markdown, err := gits.GenerateMarkdown(&release.Spec, gitInfo)
	if err != nil {
		return err
	}
	header, err := o.getTemplateResult(&release.Spec, "header", o.Header, o.HeaderFile)
	if err != nil {
		return err
	}
	footer, err := o.getTemplateResult(&release.Spec, "footer", o.Footer, o.FooterFile)
	if err != nil {
		return err
	}
	markdown = header + markdown + footer

	log.Logger().Debugf("Generated release notes:\n\n%s\n", markdown)

	if version != "" && o.UpdateRelease {
		filterTags, err := gits.FilterTags(o.Git(), dir, version)
		tags, err := filterTags, err
		if err != nil {
			return errors.Wrapf(err, "listing tags with pattern %s in %s", version, dir)
		}
		vVersion := fmt.Sprintf("v%s", version)
		vtags, err := gits.FilterTags(o.Git(), dir, vVersion)
		if err != nil {
			return errors.Wrapf(err, "listing tags with pattern %s in %s", vVersion, dir)
		}
		foundTag := false
		foundVTag := false

		for _, t := range tags {
			if t == version {
				foundTag = true
				break
			}
		}
		for _, t := range vtags {
			if t == vVersion {
				foundVTag = true
				break
			}
		}
		tagName := version
		if foundVTag && !foundTag {
			tagName = vVersion
		}
		releaseInfo := &scm.ReleaseInput{
			Title:       version,
			Tag:         tagName,
			Description: markdown,
		}

		ctx := context.Background()
		fullName := scm.Join(o.ScmFactory.Owner, o.ScmFactory.Repository)

		// lets try find a release for the tag
		rel, _, err := scmClient.Releases.FindByTag(ctx, fullName, tagName)

		if scmhelpers.IsScmNotFound(err) {
			err = nil
			rel = nil
		}
		if err != nil {
			return errors.Wrapf(err, "failed to query release on repo %s for tag %s", fullName, tagName)
		}

		if rel == nil {
			rel, _, err = scmClient.Releases.Create(ctx, fullName, releaseInfo)
			if err != nil {
				log.Logger().Warnf("Failed to create the release for %s: %s", fullName, err)
				return nil
			}
		} else {
			rel, _, err = scmClient.Releases.Update(ctx, fullName, rel.ID, releaseInfo)
			if err != nil {
				log.Logger().Warnf("Failed to update the release for %s number: %d: %s", fullName, rel.ID, err)
				return nil
			}
		}

		url := ""
		if rel != nil {
			url = rel.Link
		}
		if url == "" {
			url = stringhelpers.UrlJoin(gitInfo.HttpsURL(), "releases/tag", tagName)
		}
		release.Spec.ReleaseNotesURL = url
		log.Logger().Infof("Updated the release information at %s", info(url))
		log.Logger().Infof("added description: %s", markdown)
	} else if o.OutputMarkdownFile != "" {
		err := ioutil.WriteFile(o.OutputMarkdownFile, []byte(markdown), files.DefaultFileWritePermissions)
		if err != nil {
			return err
		}
		log.Logger().Infof("\nGenerated Changelog: %s", info(o.OutputMarkdownFile))
	} else {
		log.Logger().Infof("\nGenerated Changelog:")
		log.Logger().Infof("%s\n", markdown)
	}

	o.State.Release = release
	// now lets marshal the release YAML
	data, err := yaml.Marshal(release)

	if err != nil {
		return err
	}
	if data == nil {
		return fmt.Errorf("Could not marshal release to yaml")
	}
	releaseFile := filepath.Join(templatesDir, o.ReleaseYamlFile)
	crdFile := filepath.Join(templatesDir, o.CrdYamlFile)
	if o.GenerateReleaseYaml {
		err = ioutil.WriteFile(releaseFile, data, files.DefaultFileWritePermissions)
		if err != nil {
			return fmt.Errorf("Failed to save Release YAML file %s: %s", releaseFile, err)
		}
		log.Logger().Infof("generated: %s", info(releaseFile))
	}
	cleanVersion := strings.TrimPrefix(version, "v")
	release.Spec.Version = cleanVersion
	if o.GenerateCRD {
		exists, err := files.FileExists(crdFile)
		if err != nil {
			return fmt.Errorf("Failed to check for CRD YAML file %s: %s", crdFile, err)
		}
		if o.OverwriteCRD || !exists {
			err = ioutil.WriteFile(crdFile, []byte(ReleaseCrdYaml), files.DefaultFileWritePermissions)
			if err != nil {
				return fmt.Errorf("Failed to save Release CRD YAML file %s: %s", crdFile, err)
			}
			log.Logger().Infof("generated: %s", info(crdFile))

			err = gitclient.Add(o.Git(), templatesDir)
			if err != nil {
				return errors.Wrapf(err, "failed to git add in dir %s", templatesDir)
			}
		}
	}
	appName := ""
	if gitInfo != nil {
		appName = gitInfo.Name
	}
	if appName == "" {
		appName = release.Spec.Name
	}
	if appName == "" {
		appName = release.Spec.GitRepository
	}
	releaseNotesURL := release.Spec.ReleaseNotesURL

	// lets modify the PipelineActivity
	err = o.updatePipelineActivity(func(pa *v1.PipelineActivity) (bool, error) {
		updated := false
		ps := &pa.Spec

		doUpdate := func(oldValue, newValue string) string {
			if newValue == "" || newValue == oldValue {
				return oldValue
			}
			updated = true
			return newValue
		}

		commits := release.Spec.Commits
		if len(commits) > 0 {
			lastCommit := commits[len(commits)-1]
			ps.LastCommitSHA = doUpdate(ps.LastCommitSHA, lastCommit.SHA)
			ps.LastCommitMessage = doUpdate(ps.LastCommitMessage, lastCommit.Message)
			ps.LastCommitURL = doUpdate(ps.LastCommitURL, lastCommit.URL)
		}
		ps.ReleaseNotesURL = doUpdate(ps.ReleaseNotesURL, releaseNotesURL)
		ps.Version = doUpdate(ps.Version, cleanVersion)
		return updated, nil
	})
	if err != nil {
		return errors.Wrapf(err, "failed to update PipelineActivity")
	}
	return nil
}

func (o *Options) updatePipelineActivity(fn func(activity *v1.PipelineActivity) (bool, error)) error {
	if o.BuildNumber == "" {
		o.BuildNumber = os.Getenv("BUILD_NUMBER")
		if o.BuildNumber == "" {
			o.BuildNumber = os.Getenv("BUILD_ID")
		}
	}
	pipeline := fmt.Sprintf("%s/%s/%s", o.ScmFactory.Owner, o.ScmFactory.Repository, o.ScmFactory.Branch)

	ctx := context.Background()
	build := o.BuildNumber
	if pipeline != "" && build != "" {
		ns := o.Namespace
		name := naming.ToValidName(pipeline + "-" + build)

		jxClient := o.JXClient

		// lets see if we can update the pipeline
		acts := jxClient.JenkinsV1().PipelineActivities(ns)
		key := &activities.PromoteStepActivityKey{
			PipelineActivityKey: activities.PipelineActivityKey{
				Name:     name,
				Pipeline: pipeline,
				Build:    build,
				GitInfo: &giturl.GitRepository{
					Name:         o.ScmFactory.Repository,
					Organisation: o.ScmFactory.Owner,
				},
			},
		}
		a, _, err := key.GetOrCreate(o.JXClient, o.Namespace)
		if err != nil {
			return errors.Wrapf(err, "failed to get PipelineActivity")
		}

		updated, err := fn(a)
		if err != nil {
			return errors.Wrapf(err, "failed to update PipelineActivit %s", name)
		}
		if updated {
			a, err = acts.Update(ctx, a, metav1.UpdateOptions{})
			if err != nil {
				return errors.Wrapf(err, "failed to update PipelineActivity %s", name)
			}
			log.Logger().Infof("Updated PipelineActivity %s which has status %s", name, string(a.Spec.Status))
		}
	} else {
		log.Logger().Warnf("No $BUILD_NUMBER so cannot update PipelineActivities with the details from the changelog")
	}
	return nil
}

// CreateIssueProvider creates the issue provider
func (o *Options) CreateIssueProvider() (issues.IssueProvider, error) {
	return issues.CreateGitIssueProvider(o.ScmFactory.ScmClient, o.ScmFactory.Owner, o.ScmFactory.Repository)
	/*
		// TODO find kind from a configuration file inside the repository....
		kind := ""
		return issues.CreateIssueProvider(kind, serverURL, username, apiToken, project, o.BatchMode)
	*/
}

func (o *Options) Git() gitclient.Interface {
	if o.GitClient == nil {
		o.GitClient = cli.NewCLIClient("", o.CommandRunner)
	}
	return o.GitClient
}

func (o *Options) addCommit(spec *v1.ReleaseSpec, commit *object.Commit, resolver *users.GitUserResolver) {
	// TODO
	url := ""
	branch := "master"

	var author, committer *v1.UserDetails
	var err error
	sha := commit.Hash.String()
	if commit.Author.Email != "" && commit.Author.Name != "" {
		author, err = resolver.GitSignatureAsUser(&commit.Author)
		if err != nil {
			log.Logger().Warnf("failed to enrich commit with issues, error getting git signature for git author %s: %v", commit.Author, err)
		}
	}
	if commit.Committer.Email != "" && commit.Committer.Name != "" {
		committer, err = resolver.GitSignatureAsUser(&commit.Committer)
		if err != nil {
			log.Logger().Warnf("failed to enrich commit with issues, error getting git signature for git committer %s: %v", commit.Committer, err)
		}
	}
	commitSummary := v1.CommitSummary{
		Message:   commit.Message,
		URL:       url,
		SHA:       sha,
		Author:    author,
		Branch:    branch,
		Committer: committer,
	}

	err = o.addIssuesAndPullRequests(spec, &commitSummary, commit)
	if err != nil {
		log.Logger().Warnf("Failed to enrich commit %s with issues: %s", sha, err)
	}
	spec.Commits = append(spec.Commits, commitSummary)

}

func (o *Options) addIssuesAndPullRequests(spec *v1.ReleaseSpec, commit *v1.CommitSummary, rawCommit *object.Commit) error {
	tracker := o.State.Tracker

	regex := GitHubIssueRegex
	issueKind := issues.GetIssueProvider(tracker)
	if !o.State.LoggedIssueKind {
		o.State.LoggedIssueKind = true
		log.Logger().Infof("Finding issues in commit messages using %s format", issueKind)
	}
	if issueKind == issues.Jira {
		regex = JIRAIssueRegex
	}
	message := fullCommitMessageText(rawCommit)

	matches := regex.FindAllStringSubmatch(message, -1)

	resolver := users.GitUserResolver{
		GitProvider: o.ScmFactory.ScmClient,
	}
	for _, match := range matches {
		for _, result := range match {
			result = strings.TrimPrefix(result, "#")
			if _, ok := o.State.FoundIssueNames[result]; !ok {
				o.State.FoundIssueNames[result] = true
				issue, err := tracker.GetIssue(result)
				if err != nil {
					log.Logger().Warnf("Failed to lookup issue %s in issue tracker %s due to %s", result, tracker.HomeURL(), err)
					continue
				}
				if issue == nil {
					log.Logger().Warnf("Failed to find issue %s for repository %s", result, tracker.HomeURL())
					continue
				}

				user, err := resolver.Resolve(&issue.Author)
				if err != nil {
					log.Logger().Warnf("Failed to resolve user %v for issue %s repository %s", issue.Author, result, tracker.HomeURL())
				}

				var closedBy *v1.UserDetails
				if issue.ClosedBy == nil {
					log.Logger().Warnf("Failed to find closedBy user for issue %s repository %s", result, tracker.HomeURL())
				} else {
					u, err := resolver.Resolve(issue.ClosedBy)
					if err != nil {
						log.Logger().Warnf("Failed to resolve closedBy user %v for issue %s repository %s", issue.Author, result, tracker.HomeURL())
					} else if u != nil {
						closedBy = u
					}
				}

				var assignees []v1.UserDetails
				if issue.Assignees == nil {
					log.Logger().Warnf("Failed to find assignees for issue %s repository %s", result, tracker.HomeURL())
				} else {
					u, err := resolver.GitUserSliceAsUserDetailsSlice(issue.Assignees)
					if err != nil {
						log.Logger().Warnf("Failed to resolve Assignees %v for issue %s repository %s", issue.Assignees, result, tracker.HomeURL())
					}
					assignees = u
				}

				labels := toV1Labels(issue.Labels)
				commit.IssueIDs = append(commit.IssueIDs, result)
				issueSummary := v1.IssueSummary{
					ID:                result,
					URL:               issue.Link,
					Title:             issue.Title,
					Body:              issue.Body,
					User:              user,
					CreationTimestamp: kube.ToMetaTime(&issue.Created),
					ClosedBy:          closedBy,
					Assignees:         assignees,
					Labels:            labels,
				}
				state := issue.State
				if state != "" {
					issueSummary.State = state
				}
				if issue.PullRequest {
					spec.PullRequests = append(spec.PullRequests, issueSummary)
				} else {
					spec.Issues = append(spec.Issues, issueSummary)
				}
			}
		}
	}
	return nil
}

// toV1Labels converts git labels to IssueLabel
func toV1Labels(labels []string) []v1.IssueLabel {
	answer := []v1.IssueLabel{}
	for _, label := range labels {
		answer = append(answer, v1.IssueLabel{
			Name: label,
		})
	}
	return answer
}

// fullCommitMessageText returns the commit message
func fullCommitMessageText(commit *object.Commit) string {
	answer := commit.Message
	fn := func(parent *object.Commit) error {
		text := parent.Message
		if text != "" {
			sep := "\n"
			if strings.HasSuffix(answer, "\n") {
				sep = ""
			}
			answer += sep + text
		}
		return nil
	}
	fn(commit) //nolint:errcheck
	return answer

}

func (o *Options) getTemplateResult(releaseSpec *v1.ReleaseSpec, templateName string, templateText string, templateFile string) (string, error) {
	if templateText == "" {
		if templateFile == "" {
			return "", nil
		}
		data, err := ioutil.ReadFile(templateFile)
		if err != nil {
			return "", err
		}
		templateText = string(data)
	}
	if templateText == "" {
		return "", nil
	}
	tmpl, err := template.New(templateName).Parse(templateText)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	writer := bufio.NewWriter(&buffer)
	err = tmpl.Execute(writer, releaseSpec)
	writer.Flush()
	return buffer.String(), err
}

//CollapseDependencyUpdates takes a raw set of dependencyUpdates, removes duplicates and collapses multiple updates to
// the same org/repo:components into a sungle update
func CollapseDependencyUpdates(dependencyUpdates []v1.DependencyUpdate) []v1.DependencyUpdate {
	// Sort the dependency updates. This makes the outputs more readable, and it also allows us to more easily do duplicate removal and collapsing

	sort.Slice(dependencyUpdates, func(i, j int) bool {
		if dependencyUpdates[i].Owner == dependencyUpdates[j].Owner {
			if dependencyUpdates[i].Repo == dependencyUpdates[j].Repo {
				if dependencyUpdates[i].Component == dependencyUpdates[j].Component {
					if dependencyUpdates[i].FromVersion == dependencyUpdates[j].FromVersion {
						return dependencyUpdates[i].ToVersion < dependencyUpdates[j].ToVersion
					}
					return dependencyUpdates[i].FromVersion < dependencyUpdates[j].FromVersion
				}
				return dependencyUpdates[i].Component < dependencyUpdates[j].Component
			}
			return dependencyUpdates[i].Repo < dependencyUpdates[j].Repo
		}
		return dependencyUpdates[i].Owner < dependencyUpdates[j].Owner
	})

	// Collapse  entries
	collapsed := make([]v1.DependencyUpdate, 0)

	if len(dependencyUpdates) > 0 {
		start := 0
		for i := 1; i <= len(dependencyUpdates); i++ {
			if i == len(dependencyUpdates) || dependencyUpdates[i-1].Owner != dependencyUpdates[i].Owner || dependencyUpdates[i-1].Repo != dependencyUpdates[i].Repo || dependencyUpdates[i-1].Component != dependencyUpdates[i].Component {
				end := i - 1
				collapsed = append(collapsed, v1.DependencyUpdate{
					DependencyUpdateDetails: v1.DependencyUpdateDetails{
						Owner:              dependencyUpdates[start].Owner,
						Repo:               dependencyUpdates[start].Repo,
						Component:          dependencyUpdates[start].Component,
						URL:                dependencyUpdates[start].URL,
						Host:               dependencyUpdates[start].Host,
						FromVersion:        dependencyUpdates[start].FromVersion,
						FromReleaseHTMLURL: dependencyUpdates[start].FromReleaseHTMLURL,
						FromReleaseName:    dependencyUpdates[start].FromReleaseName,
						ToVersion:          dependencyUpdates[end].ToVersion,
						ToReleaseName:      dependencyUpdates[end].ToReleaseName,
						ToReleaseHTMLURL:   dependencyUpdates[end].ToReleaseHTMLURL,
					},
				})
				start = i
			}
		}
	}
	return collapsed
}
