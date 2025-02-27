// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package relui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/jackc/pgx/v4/pgxpool"
	"golang.org/x/build/buildlet"
	"golang.org/x/build/dashboard"
	"golang.org/x/build/internal/gcsfs"
	"golang.org/x/build/internal/releasetargets"
	"golang.org/x/build/internal/relui/db"
	"golang.org/x/build/internal/task"
	"golang.org/x/build/internal/workflow"
	"golang.org/x/net/context/ctxhttp"
)

// DefinitionHolder holds workflow definitions.
type DefinitionHolder struct {
	mu          sync.Mutex
	definitions map[string]*workflow.Definition
}

// NewDefinitionHolder creates a new DefinitionHolder,
// initialized with a sample "echo" workflow.
func NewDefinitionHolder() *DefinitionHolder {
	return &DefinitionHolder{definitions: map[string]*workflow.Definition{
		"echo": newEchoWorkflow(),
	}}
}

// Definition returns the initialized workflow.Definition registered
// for a given name.
func (h *DefinitionHolder) Definition(name string) *workflow.Definition {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.definitions[name]
}

// RegisterDefinition registers a definition with a name.
// If a definition with the same name already exists, RegisterDefinition panics.
func (h *DefinitionHolder) RegisterDefinition(name string, d *workflow.Definition) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exist := h.definitions[name]; exist {
		panic("relui: multiple registrations for " + name)
	}
	h.definitions[name] = d
}

// Definitions returns the names of all registered definitions.
func (h *DefinitionHolder) Definitions() map[string]*workflow.Definition {
	h.mu.Lock()
	defer h.mu.Unlock()
	defs := make(map[string]*workflow.Definition)
	for k, v := range h.definitions {
		defs[k] = v
	}
	return defs
}

// RegisterMailDLCLDefinition registers a workflow definition for mailing a golang.org/dl CL
// onto h.
func RegisterMailDLCLDefinition(h *DefinitionHolder, tasks *task.VersionTasks) {
	versions := workflow.Parameter{
		Name:          "Versions",
		ParameterType: workflow.SliceShort,
		Doc: `Versions are the Go versions that have been released.

The versions must use the same format as Go tags,
and the list must contain one or two versions.

For example:
• "go1.18.2" and "go1.17.10" for a minor Go release
• "go1.19" for a major Go release
• "go1.19beta1" or "go1.19rc1" for a pre-release`,
	}

	wd := workflow.New()
	wd.Output("ChangeURL", wd.Task("mail-dl-cl", func(ctx *workflow.TaskContext, versions []string) (string, error) {
		id, err := tasks.MailDLCL(ctx, versions, false)
		if err != nil {
			return "", err
		}
		return task.ChangeLink(id), nil
	}, wd.Parameter(versions)))
	h.RegisterDefinition("mail-dl-cl", wd)
}

// RegisterAnnounceDefinitions registers workflow definitions involving announcing
// onto h.
func RegisterAnnounceDefinitions(h *DefinitionHolder, tasks task.AnnounceMailTasks) {
	version := workflow.Parameter{
		Name: "Version",
		Doc: `Version is the Go version that has been released.

The version string must use the same format as Go tags.`,
	}
	security := workflow.Parameter{
		Name:          "Security (optional)",
		ParameterType: workflow.SliceLong,
		Doc: `Security is a list of descriptions, one for each distinct security fix included in this release, in Markdown format.

The empty list means there are no security fixes included.

This field applies only to minor releases.

Past examples:
• "encoding/pem: fix stack overflow in Decode

   A large (more than 5 MB) PEM input can cause a stack overflow in Decode,
   leading the program to crash.

   Thanks to Juho Nurminen of Mattermost who reported the error.

   This is CVE-2022-24675 and Go issue https://go.dev/issue/51853."
• "crypto/elliptic: tolerate all oversized scalars in generic P-256

   A crafted scalar input longer than 32 bytes can cause P256().ScalarMult
   or P256().ScalarBaseMult to panic. Indirect uses through crypto/ecdsa and
   crypto/tls are unaffected. amd64, arm64, ppc64le, and s390x are unaffected.

   This was discovered thanks to a Project Wycheproof test vector.

   This is CVE-2022-28327 and Go issue https://go.dev/issue/52075."`,
		Example: `encoding/pem: fix stack overflow in Decode

A large (more than 5 MB) PEM input can cause a stack overflow in Decode,
leading the program to crash.

Thanks to Juho Nurminen of Mattermost who reported the error.

This is CVE-2022-24675 and Go issue https://go.dev/issue/51853.`,
	}
	names := workflow.Parameter{
		Name:          "Names (optional)",
		ParameterType: workflow.SliceShort,
		Doc:           `Names is an optional list of release coordinator names to include in the sign-off message.`,
	}

	{
		minorVersion := version
		minorVersion.Example = "go1.18.2"
		secondaryVersion := workflow.Parameter{
			Name:    "SecondaryVersion",
			Doc:     `SecondaryVersion is an older Go version that was also released.`,
			Example: "go1.17.10",
		}

		wd := workflow.New()
		sentMail := wd.Task("mail-announcement", func(ctx *workflow.TaskContext, v1, v2 string, sec, names []string) (task.SentMail, error) {
			return tasks.AnnounceMinorRelease(ctx, task.ReleaseAnnouncement{Version: v1, SecondaryVersion: v2, Security: sec, Names: names})
		}, wd.Parameter(minorVersion), wd.Parameter(secondaryVersion), wd.Parameter(security), wd.Parameter(names))
		wd.Output("AnnouncementURL", wd.Task("await-announcement", tasks.AwaitAnnounceMail, sentMail))
		h.RegisterDefinition("announce-minor", wd)
	}
	{
		betaVersion := version
		betaVersion.Example = "go1.19beta1"

		wd := workflow.New()
		sentMail := wd.Task("mail-announcement", func(ctx *workflow.TaskContext, v string, names []string) (task.SentMail, error) {
			return tasks.AnnounceBetaRelease(ctx, task.ReleaseAnnouncement{Version: v, Names: names})
		}, wd.Parameter(betaVersion), wd.Parameter(names))
		wd.Output("AnnouncementURL", wd.Task("await-announcement", tasks.AwaitAnnounceMail, sentMail))
		h.RegisterDefinition("announce-beta", wd)
	}
	{
		rcVersion := version
		rcVersion.Example = "go1.19rc1"

		wd := workflow.New()
		sentMail := wd.Task("mail-announcement", func(ctx *workflow.TaskContext, v string, names []string) (task.SentMail, error) {
			return tasks.AnnounceRCRelease(ctx, task.ReleaseAnnouncement{Version: v, Names: names})
		}, wd.Parameter(rcVersion), wd.Parameter(names))
		wd.Output("AnnouncementURL", wd.Task("await-announcement", tasks.AwaitAnnounceMail, sentMail))
		h.RegisterDefinition("announce-rc", wd)
	}
	{
		majorVersion := version
		majorVersion.Example = "go1.19"

		wd := workflow.New()
		sentMail := wd.Task("mail-announcement", func(ctx *workflow.TaskContext, v string, names []string) (task.SentMail, error) {
			return tasks.AnnounceMajorRelease(ctx, task.ReleaseAnnouncement{Version: v, Names: names})
		}, wd.Parameter(majorVersion), wd.Parameter(names))
		wd.Output("AnnouncementURL", wd.Task("await-announcement", tasks.AwaitAnnounceMail, sentMail))
		h.RegisterDefinition("announce-major", wd)
	}
}

// RegisterTweetDefinitions registers workflow definitions involving tweeting
// onto h, using e for the external service configuration.
func RegisterTweetDefinitions(h *DefinitionHolder, e task.ExternalConfig) {
	version := workflow.Parameter{
		Name: "Version",
		Doc: `Version is the Go version that has been released.

The version string must use the same format as Go tags.`,
	}
	security := workflow.Parameter{
		Name: "Security (optional)",
		Doc: `Security is an optional sentence describing security fixes included in this release.

The empty string means there are no security fixes to highlight.

Past examples:
• "Includes a security fix for crypto/tls (CVE-2021-34558)."
• "Includes a security fix for the Wasm port (CVE-2021-38297)."
• "Includes security fixes for encoding/pem (CVE-2022-24675), crypto/elliptic (CVE-2022-28327), crypto/x509 (CVE-2022-27536)."`,
	}
	announcement := workflow.Parameter{
		Name:          "Announcement",
		ParameterType: workflow.URL,
		Doc: `Announcement is the announcement URL.

It's applicable to all release types other than major
(since major releases point to release notes instead).`,
		Example: "https://groups.google.com/g/golang-announce/c/wB1fph5RpsE/m/ZGwOsStwAwAJ",
	}

	{
		minorVersion := version
		minorVersion.Example = "go1.18.2"
		secondaryVersion := workflow.Parameter{
			Name:    "SecondaryVersion",
			Doc:     `SecondaryVersion is an older Go version that was also released.`,
			Example: "go1.17.10",
		}

		wd := workflow.New()
		wd.Output("TweetURL", wd.Task("tweet-minor", func(ctx *workflow.TaskContext, v1, v2, sec, ann string) (string, error) {
			return task.TweetMinorRelease(ctx, task.ReleaseTweet{Version: v1, SecondaryVersion: v2, Security: sec, Announcement: ann}, e)
		}, wd.Parameter(minorVersion), wd.Parameter(secondaryVersion), wd.Parameter(security), wd.Parameter(announcement)))
		h.RegisterDefinition("tweet-minor", wd)
	}
	{
		betaVersion := version
		betaVersion.Example = "go1.19beta1"

		wd := workflow.New()
		wd.Output("TweetURL", wd.Task("tweet-beta", func(ctx *workflow.TaskContext, v, sec, ann string) (string, error) {
			return task.TweetBetaRelease(ctx, task.ReleaseTweet{Version: v, Security: sec, Announcement: ann}, e)
		}, wd.Parameter(betaVersion), wd.Parameter(security), wd.Parameter(announcement)))
		h.RegisterDefinition("tweet-beta", wd)
	}
	{
		rcVersion := version
		rcVersion.Example = "go1.19rc1"

		wd := workflow.New()
		wd.Output("TweetURL", wd.Task("tweet-rc", func(ctx *workflow.TaskContext, v, sec, ann string) (string, error) {
			return task.TweetRCRelease(ctx, task.ReleaseTweet{Version: v, Security: sec, Announcement: ann}, e)
		}, wd.Parameter(rcVersion), wd.Parameter(security), wd.Parameter(announcement)))
		h.RegisterDefinition("tweet-rc", wd)
	}
	{
		majorVersion := version
		majorVersion.Example = "go1.19"

		wd := workflow.New()
		wd.Output("TweetURL", wd.Task("tweet-major", func(ctx *workflow.TaskContext, v, sec string) (string, error) {
			return task.TweetMajorRelease(ctx, task.ReleaseTweet{Version: v, Security: sec}, e)
		}, wd.Parameter(majorVersion), wd.Parameter(security)))
		h.RegisterDefinition("tweet-major", wd)
	}
}

// newEchoWorkflow returns a runnable workflow.Definition for
// development.
func newEchoWorkflow() *workflow.Definition {
	wd := workflow.New()
	wd.Output("greeting", wd.Task("greeting", echo, wd.Parameter(workflow.Parameter{Name: "greeting"})))
	wd.Output("farewell", wd.Task("farewell", echo, wd.Parameter(workflow.Parameter{Name: "farewell"})))
	return wd
}

func echo(ctx *workflow.TaskContext, arg string) (string, error) {
	ctx.Printf("echo(%v, %q)", ctx, arg)
	return arg, nil
}

type AwaitConditionFunc func(ctx *workflow.TaskContext) (done bool, err error)

// AwaitFunc is a workflow.Task that polls the provided awaitCondition
// every period until it either returns true or returns an error.
func AwaitFunc(ctx *workflow.TaskContext, period time.Duration, awaitCondition AwaitConditionFunc) (bool, error) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
			ok, err := awaitCondition(ctx)
			if ok || err != nil {
				return ok, err
			}
		}
	}
}

// AwaitAction is a workflow.Action that is a convenience wrapper
// around AwaitFunc.
func AwaitAction(ctx *workflow.TaskContext, period time.Duration, awaitCondition AwaitConditionFunc) error {
	_, err := AwaitFunc(ctx, period, awaitCondition)
	return err
}

func checkTaskApproved(ctx *workflow.TaskContext, p *pgxpool.Pool, taskName string) (bool, error) {
	q := db.New(p)
	logs, err := q.TaskLogsForTask(ctx, db.TaskLogsForTaskParams{
		WorkflowID: ctx.WorkflowID,
		TaskName:   taskName,
	})
	if err != nil {
		return false, err
	}
	for _, l := range logs {
		if strings.Contains(l.Body, "USER-APPROVED") {
			return true, nil
		}
	}
	return false, nil
}

func approveActionDep(p *pgxpool.Pool, taskName string) func(*workflow.TaskContext, interface{}) error {
	return func(ctx *workflow.TaskContext, _ interface{}) error {
		return AwaitAction(ctx, 10*time.Second, func(ctx *workflow.TaskContext) (done bool, err error) {
			return checkTaskApproved(ctx, p, taskName)
		})
	}
}

func ApproveActionDep(p *pgxpool.Pool) func(taskName string) func(*workflow.TaskContext, interface{}) error {
	return func(taskName string) func(*workflow.TaskContext, interface{}) error {
		return approveActionDep(p, taskName)
	}
}

func RegisterReleaseWorkflows(h *DefinitionHolder, build *BuildReleaseTasks, milestone *task.MilestoneTasks, version *task.VersionTasks) error {
	createSingle := func(name, major string, kind task.ReleaseKind) error {
		wd := workflow.New()
		err := addSingleReleaseWorkflow(build, milestone, version, wd, major, kind)
		if err != nil {
			return err
		}
		h.RegisterDefinition(name, wd)
		return nil
	}
	if err := createSingle("Go 1.19 final", "go1.19", task.KindMajor); err != nil {
		return err
	}
	if err := createSingle("Go 1.19 next RC", "go1.19", task.KindRC); err != nil {
		return err
	}
	if err := createSingle("Go 1.19 next beta", "go1.19", task.KindBeta); err != nil {
		return err
	}
	wd, err := createMinorReleaseWorkflow(build, milestone, version, "go1.17", "go1.18")
	if err != nil {
		return err
	}
	h.RegisterDefinition("Minor releases for Go 1.17 and 1.18", wd)
	return nil
}

func createMinorReleaseWorkflow(build *BuildReleaseTasks, milestone *task.MilestoneTasks, version *task.VersionTasks, prev, current string) (*workflow.Definition, error) {
	wd := workflow.New()
	if err := addSingleReleaseWorkflow(build, milestone, version, wd.Sub(current), current, task.KindCurrentMinor); err != nil {
		return nil, err
	}
	if err := addSingleReleaseWorkflow(build, milestone, version, wd.Sub(prev), prev, task.KindPrevMinor); err != nil {
		return nil, err
	}
	return wd, nil
}

func addSingleReleaseWorkflow(build *BuildReleaseTasks, milestone *task.MilestoneTasks, version *task.VersionTasks, wd *workflow.Definition, major string, kind task.ReleaseKind) error {
	skipTests := wd.Parameter(workflow.Parameter{Name: "Targets to skip testing (or 'all') (optional)", ParameterType: workflow.SliceShort})

	kindVal := wd.Constant(kind)
	branch := fmt.Sprintf("release-branch.%v", major)
	if kind == task.KindBeta {
		branch = "master"
	}
	branchVal := wd.Constant(branch)
	releaseBase := wd.Task("Pick release base commit", version.ReadBranchHead, branchVal)

	// Select version, check milestones.
	nextVersion := wd.Task("Get next version", version.GetNextVersion, kindVal)
	milestones := wd.Task("Pick milestones", milestone.FetchMilestones, nextVersion, kindVal)
	checked := wd.Action("Check blocking issues", milestone.CheckBlockers, milestones, nextVersion, kindVal)
	dlcl := wd.Task("Mail DL CL", version.MailDLCL, wd.Slice([]workflow.Value{nextVersion}), wd.Constant(false))
	dlclCommit := wd.Task("Wait for DL CL", version.AwaitCL, dlcl, wd.Constant(""))
	wd.Output("Download CL submitted", dlclCommit)

	startSigner := wd.Task("Start signing command", build.startSigningCommand, nextVersion)
	wd.Output("Signing command", startSigner)

	// Build, test, and sign release.
	signedAndTestedArtifacts, err := build.addBuildTasks(wd, "go1.19", nextVersion, releaseBase, skipTests, checked)
	if err != nil {
		return err
	}

	verifiedName := "APPROVE-Wait for Release Coordinator Approval"
	verified := wd.Action(verifiedName, build.ApproveActionFunc(verifiedName), signedAndTestedArtifacts)

	// Tag version and upload to CDN/website.
	uploaded := wd.Action("Upload artifacts to CDN", build.uploadArtifacts, signedAndTestedArtifacts, verified)

	tagCommit := releaseBase
	if branch != "master" {
		branchHeadChecked := wd.Action("Check for modified branch head", version.CheckBranchHead, branchVal, releaseBase, uploaded)
		versionCL := wd.Task("Mail version CL", version.CreateAutoSubmitVersionCL, branchVal, nextVersion, branchHeadChecked)
		tagCommit = wd.Task("Wait for version CL submission", version.AwaitCL, versionCL, releaseBase)
	}
	tagged := wd.Action("Tag version", version.TagRelease, nextVersion, tagCommit, uploaded)

	pushed := wd.Action("Push issues", milestone.PushIssues, milestones, nextVersion, kindVal, tagged)
	published := wd.Task("Publish to website", build.publishArtifacts, nextVersion, signedAndTestedArtifacts, pushed)
	wd.Output("Publish results", published)
	return nil
}

func (tasks *BuildReleaseTasks) addBuildTasks(wd *workflow.Definition, majorVersion string, version, revision, skipTests workflow.Value, dependency workflow.Dependency) (workflow.Value, error) {
	targets, ok := releasetargets.TargetsForVersion(majorVersion)
	if !ok {
		return nil, fmt.Errorf("malformed/unknown version %q", majorVersion)
	}

	source := wd.Task("Build source archive", tasks.buildSource, revision, version, dependency)
	// Artifact file paths.
	artifacts := []workflow.Value{source}
	var darwinTargets []*releasetargets.Target
	var testsPassed []workflow.TaskInput
	for _, target := range targets {
		targetVal := wd.Constant(target)
		wd := wd.Sub(target.Name)

		// Build release artifacts for the platform.
		bin := wd.Task("Build binary archive", tasks.buildBinary, targetVal, source)
		switch target.GOOS {
		case "windows":
			zip := wd.Task("Convert to .zip", tasks.convertToZip, targetVal, bin)
			msi := wd.Task("Build MSI", tasks.buildMSI, targetVal, bin)
			artifacts = append(artifacts, msi, zip)
		case "darwin":
			artifacts = append(artifacts, bin)
			darwinTargets = append(darwinTargets, target)
		default:
			artifacts = append(artifacts, bin)
		}

		if target.BuildOnly {
			continue
		}
		short := wd.Action("Run short tests", tasks.runTests, targetVal, wd.Constant(target.Builder), skipTests, bin)
		testsPassed = append(testsPassed, short)
		if target.LongTestBuilder != "" {
			long := wd.Action("Run long tests", tasks.runTests, targetVal, wd.Constant(target.LongTestBuilder), skipTests, bin)
			testsPassed = append(testsPassed, long)
		}
	}
	stagedArtifacts := wd.Task("Stage artifacts for signing", tasks.copyToStaging, version, wd.Slice(artifacts))
	signedArtifacts := wd.Task("Wait for signed artifacts", tasks.awaitSigned, version, wd.Constant(darwinTargets), stagedArtifacts)
	signedAndTested := wd.Task("Wait for signing and tests", func(ctx *workflow.TaskContext, artifacts []artifact) ([]artifact, error) {
		return artifacts, nil
	}, append([]workflow.TaskInput{signedArtifacts}, testsPassed...)...)
	return signedAndTested, nil
}

// BuildReleaseTasks serves as an adapter to the various build tasks in the task package.
type BuildReleaseTasks struct {
	GerritURL              string
	GCSClient              *storage.Client
	ScratchURL, ServingURL string
	DownloadURL            string
	PublishFile            func(*WebsiteFile) error
	CreateBuildlet         func(string) (buildlet.Client, error)
	ApproveActionFunc      func(taskName string) func(*workflow.TaskContext, interface{}) error
}

func (b *BuildReleaseTasks) buildSource(ctx *workflow.TaskContext, revision, version string) (artifact, error) {
	return b.runBuildStep(ctx, nil, "", artifact{}, "src.tar.gz", func(_ *task.BuildletStep, _ io.Reader, w io.Writer) error {
		return task.WriteSourceArchive(ctx, b.GerritURL, revision, version, w)
	})
}

func (b *BuildReleaseTasks) buildBinary(ctx *workflow.TaskContext, target *releasetargets.Target, source artifact) (artifact, error) {
	return b.runBuildStep(ctx, target, target.Builder, source, "tar.gz", func(bs *task.BuildletStep, r io.Reader, w io.Writer) error {
		return bs.BuildBinary(ctx, r, w)
	})
}

func (b *BuildReleaseTasks) buildMSI(ctx *workflow.TaskContext, target *releasetargets.Target, binary artifact) (artifact, error) {
	return b.runBuildStep(ctx, target, target.Builder, binary, "msi", func(bs *task.BuildletStep, r io.Reader, w io.Writer) error {
		return bs.BuildMSI(ctx, r, w)
	})
}

func (b *BuildReleaseTasks) convertToZip(ctx *workflow.TaskContext, target *releasetargets.Target, binary artifact) (artifact, error) {
	return b.runBuildStep(ctx, target, "", binary, "zip", func(_ *task.BuildletStep, r io.Reader, w io.Writer) error {
		return task.ConvertTGZToZIP(r, w)
	})
}

func (b *BuildReleaseTasks) runTests(ctx *workflow.TaskContext, target *releasetargets.Target, buildlet string, skipTests []string, binary artifact) error {
	skipped := false
	for _, skip := range skipTests {
		if skip == "all" || target.Name == skip {
			skipped = true
			break
		}
	}
	if skipped {
		ctx.Printf("Skipping test")
		return nil
	}
	_, err := b.runBuildStep(ctx, target, buildlet, binary, "", func(bs *task.BuildletStep, r io.Reader, _ io.Writer) error {
		return bs.TestTarget(ctx, r)
	})
	return err
}

// runBuildStep is a convenience function that manages resources a build step might need.
// If target and buildlet name are specified, a BuildletStep will be passed to f.
// If inputName is specified, it will be opened and passed as a Reader to f.
// If outputSuffix is specified, a unique filename will be generated based off
// it (and the target name, if any), the file will be opened and passed as a
// Writer to f, and an artifact representing it will be returned as the result.
func (b *BuildReleaseTasks) runBuildStep(
	ctx *workflow.TaskContext,
	target *releasetargets.Target,
	buildletName string,
	input artifact,
	outputSuffix string,
	f func(*task.BuildletStep, io.Reader, io.Writer) error,
) (artifact, error) {
	var step *task.BuildletStep
	if buildletName != "" {
		if target == nil {
			return artifact{}, fmt.Errorf("target must be specified to use a buildlet")
		}
		ctx.Printf("Creating buildlet %v.", buildletName)
		client, err := b.CreateBuildlet(buildletName)
		if err != nil {
			return artifact{}, err
		}
		defer client.Close()
		buildConfig, ok := dashboard.Builders[buildletName]
		if !ok {
			return artifact{}, fmt.Errorf("unknown builder: %v", buildConfig)
		}
		step = &task.BuildletStep{
			Target:      target,
			Buildlet:    client,
			BuildConfig: buildConfig,
			Watch:       true,
		}
		ctx.Printf("Buildlet ready.")
	}

	scratchFS, err := gcsfs.FromURL(ctx, b.GCSClient, b.ScratchURL)
	if err != nil {
		return artifact{}, err
	}
	var in io.ReadCloser
	if input.ScratchPath != "" {
		in, err = scratchFS.Open(input.ScratchPath)
		if err != nil {
			return artifact{}, err
		}
		defer in.Close()
	}
	var out io.WriteCloser
	var scratchPath string
	hash := sha256.New()
	size := &sizeWriter{}
	var multiOut io.Writer
	if outputSuffix != "" {
		scratchName := outputSuffix
		if target != nil {
			scratchName = target.Name + "." + outputSuffix
		}
		scratchPath = fmt.Sprintf("%v/%v-%v", ctx.WorkflowID.String(), scratchName, rand.Int63())
		out, err = gcsfs.Create(scratchFS, scratchPath)
		if err != nil {
			return artifact{}, err
		}
		defer out.Close()
		multiOut = io.MultiWriter(out, hash, size)
	}
	// Hide in's Close method from the task, which may assert it to Closer.
	nopIn := io.NopCloser(in)
	if err := f(step, nopIn, multiOut); err != nil {
		return artifact{}, err
	}
	if step != nil {
		if err := step.Buildlet.Close(); err != nil {
			return artifact{}, err
		}
	}
	if in != nil {
		if err := in.Close(); err != nil {
			return artifact{}, err
		}
	}
	if out != nil {
		if err := out.Close(); err != nil {
			return artifact{}, err
		}
	}
	return artifact{
		Target:      target,
		ScratchPath: scratchPath,
		Suffix:      outputSuffix,
		SHA256:      fmt.Sprintf("%x", string(hash.Sum([]byte(nil)))),
		Size:        size.size,
	}, nil
}

// An artifact represents a file as it moves through the release process. Most
// files will appear on go.dev/dl eventually.
type artifact struct {
	// The target platform of this artifact, or nil for source.
	Target *releasetargets.Target
	// The scratch path of this artifact within the scratch directory.
	// <workflow-id>/<filename>-<random-number>
	ScratchPath string
	// The path within the scratch directory the artifact was staged to for the
	// signing process.
	// <workflow-id>/signing/<go version>/<filename>
	StagingPath string
	// The path within the scratch directory the artifact can be found at
	// after the signing process. For files not modified by the signing
	// process, the staging path, or for those that are
	// <workflow-id>/signing/<go version>/signed/<filename>
	SignedPath string
	// The contents of the GPG signature for this artifact (.asc file).
	GPGSignature string
	// The filename suffix of the artifact, e.g. "tar.gz" or "src.tar.gz",
	// combined with the version and Target name to produce Filename.
	Suffix string
	// The final Filename of this artifact as it will be downloaded.
	Filename string
	SHA256   string
	Size     int
}

type sizeWriter struct {
	size int
}

func (w *sizeWriter) Write(p []byte) (n int, err error) {
	w.size += len(p)
	return len(p), nil
}

func (tasks *BuildReleaseTasks) startSigningCommand(ctx *workflow.TaskContext, version string) (string, error) {
	args := fmt.Sprintf("--relui_staging=%q", tasks.ScratchURL+"/"+signingStagingDir(ctx, version))
	ctx.Printf("run signer with " + args)
	return args, nil
}

func (tasks *BuildReleaseTasks) copyToStaging(ctx *workflow.TaskContext, version string, artifacts []artifact) ([]artifact, error) {
	scratchFS, err := gcsfs.FromURL(ctx, tasks.GCSClient, tasks.ScratchURL)
	if err != nil {
		return nil, err
	}
	var stagedArtifacts []artifact
	for _, a := range artifacts {
		staged := a
		if a.Target != nil {
			staged.Filename = version + "." + a.Target.Name + "." + a.Suffix
		} else {
			staged.Filename = version + "." + a.Suffix
		}
		staged.StagingPath = path.Join(signingStagingDir(ctx, version), staged.Filename)
		stagedArtifacts = append(stagedArtifacts, staged)

		in, err := scratchFS.Open(a.ScratchPath)
		if err != nil {
			return nil, err
		}
		out, err := gcsfs.Create(scratchFS, staged.StagingPath)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(out, in); err != nil {
			return nil, err
		}
		if err := in.Close(); err != nil {
			return nil, err
		}
		if err := out.Close(); err != nil {
			return nil, err
		}
	}
	out, err := gcsfs.Create(scratchFS, path.Join(signingStagingDir(ctx, version), "ready"))
	if err != nil {
		return nil, err
	}
	if _, err := out.Write([]byte("ready")); err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	return stagedArtifacts, nil
}

func signingStagingDir(ctx *workflow.TaskContext, version string) string {
	return path.Join(ctx.WorkflowID.String(), "signing", version)
}

var signingPollDuration = 30 * time.Second

// awaitSigned waits for all of artifacts to be signed, plus the pkgs for
// darwinTargets.
func (tasks *BuildReleaseTasks) awaitSigned(ctx *workflow.TaskContext, version string, darwinTargets []*releasetargets.Target, artifacts []artifact) ([]artifact, error) {
	// .pkg artifacts are created by the signing process. Create placeholders,
	// to be filled out once the files exist.
	for _, t := range darwinTargets {
		artifacts = append(artifacts, artifact{
			Target:   t,
			Suffix:   "pkg",
			Filename: version + "." + t.Name + ".pkg",
			Size:     -1,
		})
	}

	scratchFS, err := gcsfs.FromURL(ctx, tasks.GCSClient, tasks.ScratchURL)
	if err != nil {
		return nil, err
	}

	todo := map[artifact]bool{}
	for _, a := range artifacts {
		todo[a] = true
	}
	var signedArtifacts []artifact
	for {
		for a := range todo {
			signed, ok, err := readSignedArtifact(ctx, scratchFS, version, a)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}

			signedArtifacts = append(signedArtifacts, signed)
			delete(todo, a)
		}

		if len(todo) == 0 {
			return signedArtifacts, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(signingPollDuration):
			ctx.Printf("Still waiting for %v artifacts to be signed", len(todo))
		}
	}
}

func readSignedArtifact(ctx *workflow.TaskContext, scratchFS fs.FS, version string, a artifact) (_ artifact, ok bool, _ error) {
	// Our signing process has somewhat uneven behavior. In general, for things
	// that contain their own signature, such as MSIs and .pkgs, we don't
	// produce a GPG signature, just the new file. On macOS, tars can be signed
	// too, but we GPG sign them anyway.
	modifiedBySigning := false
	hasGPG := false
	suffix := func(suffix string) bool { return a.Suffix == suffix }
	switch {
	case suffix("src.tar.gz"):
		hasGPG = true
	case a.Target.GOOS == "darwin" && suffix("tar.gz"):
		modifiedBySigning = true
		hasGPG = true
	case a.Target.GOOS == "darwin" && suffix("pkg"):
		modifiedBySigning = true
	case suffix("tar.gz"):
		hasGPG = true
	case suffix("msi"):
		modifiedBySigning = true
	case suffix("zip"):
		// For reasons unclear, we don't sign zip files.
	default:
		return artifact{}, false, fmt.Errorf("unhandled file type %q", a.Suffix)
	}

	signed := artifact{
		Target:   a.Target,
		Filename: a.Filename,
		Suffix:   a.Suffix,
	}
	stagingDir := signingStagingDir(ctx, version)
	if modifiedBySigning {
		signed.SignedPath = stagingDir + "/signed/" + a.Filename
	} else {
		signed.SignedPath = stagingDir + "/" + a.Filename
	}

	fi, err := fs.Stat(scratchFS, signed.SignedPath)
	if err != nil {
		return artifact{}, false, nil
	}
	if modifiedBySigning {
		hash, err := fs.ReadFile(scratchFS, stagingDir+"/signed/"+a.Filename+".sha256")
		if err != nil {
			return artifact{}, false, nil
		}
		signed.Size = int(fi.Size())
		signed.SHA256 = string(hash)
	} else {
		signed.SHA256 = a.SHA256
		signed.Size = a.Size
	}
	if hasGPG {
		sig, err := fs.ReadFile(scratchFS, stagingDir+"/signed/"+a.Filename+".asc")
		if err != nil {
			return artifact{}, false, nil
		}
		signed.GPGSignature = string(sig)
	}
	return signed, true, nil
}

var uploadPollDuration = 30 * time.Second

func (tasks *BuildReleaseTasks) uploadArtifacts(ctx *workflow.TaskContext, artifacts []artifact) error {
	scratchFS, err := gcsfs.FromURL(ctx, tasks.GCSClient, tasks.ScratchURL)
	if err != nil {
		return err
	}
	servingFS, err := gcsfs.FromURL(ctx, tasks.GCSClient, tasks.ServingURL)
	if err != nil {
		return err
	}

	todo := map[artifact]bool{}
	for _, a := range artifacts {
		if err := uploadArtifact(scratchFS, servingFS, a); err != nil {
			return err
		}
		todo[a] = true
	}

	for {
		for _, a := range artifacts {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			resp, err := ctxhttp.Head(ctx, http.DefaultClient, tasks.DownloadURL+"/"+a.Filename)
			if err != nil && err != context.DeadlineExceeded {
				return err
			}
			resp.Body.Close()
			cancel()
			if resp.StatusCode == http.StatusOK {
				delete(todo, a)
			}
		}

		if len(todo) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(uploadPollDuration):
			ctx.Printf("Still waiting for %v artifacts to be published", len(todo))
		}
	}
}

func uploadArtifact(scratchFS, servingFS fs.FS, a artifact) error {
	in, err := scratchFS.Open(a.SignedPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := gcsfs.Create(servingFS, a.Filename)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	sha256, err := gcsfs.Create(servingFS, a.Filename+".sha256")
	if err != nil {
		return err
	}
	defer sha256.Close()
	if _, err := sha256.Write([]byte(a.SHA256)); err != nil {
		return err
	}
	if err := sha256.Close(); err != nil {
		return err
	}

	if a.GPGSignature != "" {
		asc, err := gcsfs.Create(servingFS, a.Filename+".asc")
		if err != nil {
			return err
		}
		defer asc.Close()
		if _, err := asc.Write([]byte(a.GPGSignature)); err != nil {
			return err
		}
		if err := asc.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (tasks *BuildReleaseTasks) publishArtifacts(ctx *workflow.TaskContext, version string, artifacts []artifact) (string, error) {
	for _, a := range artifacts {
		f := &WebsiteFile{
			Filename:       a.Filename,
			Version:        version,
			ChecksumSHA256: a.SHA256,
			Size:           int64(a.Size),
		}
		if a.Target != nil {
			f.OS = a.Target.GOOS
			f.Arch = a.Target.GOARCH
			if a.Target.GOARCH == "arm" {
				f.Arch = "armv6l"
			}
		}
		switch a.Suffix {
		case "src.tar.gz":
			f.Kind = "source"
		case "tar.gz", "zip":
			f.Kind = "archive"
		case "msi", "pkg":
			f.Kind = "installer"
		}
		if err := tasks.PublishFile(f); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Uploaded %v artifacts for %v", len(artifacts), version), nil
}

// WebsiteFile represents a file on the go.dev downloads page.
// It should be kept in sync with the download code in x/website/internal/dl.
type WebsiteFile struct {
	Filename       string `json:"filename"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	Version        string `json:"version"`
	ChecksumSHA256 string `json:"sha256"`
	Size           int64  `json:"size"`
	Kind           string `json:"kind"` // "archive", "installer", "source"
}
