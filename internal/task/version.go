package task

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/build/gerrit"
	"golang.org/x/build/internal/workflow"
)

// VersionTasks contains tasks related to versioning the release.
type VersionTasks struct {
	Gerrit    GerritClient
	GoProject string
}

// GetNextVersion returns the next for the given type of release.
func (t *VersionTasks) GetNextVersion(ctx *workflow.TaskContext, kind ReleaseKind) (string, error) {
	tags, err := t.Gerrit.ListTags(ctx, t.GoProject)
	if err != nil {
		return "", err
	}
	tagSet := map[string]bool{}
	for _, tag := range tags {
		tagSet[tag] = true
	}
	// Find the most recently released major version.
	// Going down from a high number is convenient for testing.
	currentMajor := 100
	for ; ; currentMajor-- {
		if tagSet[fmt.Sprintf("go1.%d", currentMajor)] {
			break
		}
	}
	findUnused := func(v string) (string, error) {
		for {
			if !tagSet[v] {
				return v, nil
			}
			v, err = nextVersion(v)
			if err != nil {
				return "", err
			}
		}
	}
	switch kind {
	case KindCurrentMinor:
		return findUnused(fmt.Sprintf("go1.%d.1", currentMajor))
	case KindPrevMinor:
		return findUnused(fmt.Sprintf("go1.%d.1", currentMajor-1))
	case KindBeta:
		return findUnused(fmt.Sprintf("go1.%dbeta1", currentMajor+1))
	case KindRC:
		return findUnused(fmt.Sprintf("go1.%drc1", currentMajor+1))
	case KindMajor:
		return fmt.Sprintf("go1.%d", currentMajor+1), nil
	}
	return "", fmt.Errorf("unknown release kind %v", kind)
}

func nextVersion(version string) (string, error) {
	lastNonDigit := strings.LastIndexFunc(version, func(r rune) bool {
		return r < '0' || r > '9'
	})
	if lastNonDigit == -1 || len(version) == lastNonDigit {
		return "", fmt.Errorf("malformatted Go version %q", version)
	}
	n, err := strconv.Atoi(version[lastNonDigit+1:])
	if err != nil {
		return "", fmt.Errorf("malformatted Go version %q (%v)", version, err)
	}
	return fmt.Sprintf("%s%d", version[:lastNonDigit+1], n+1), nil
}

// CreateAutoSubmitVersionCL mails an auto-submit change to update VERSION on branch.
func (t *VersionTasks) CreateAutoSubmitVersionCL(ctx *workflow.TaskContext, branch, version string) (string, error) {
	return t.Gerrit.CreateAutoSubmitChange(ctx, gerrit.ChangeInput{
		Project: t.GoProject,
		Branch:  branch,
		Subject: fmt.Sprintf("[%v] %v", branch, version),
	}, map[string]string{
		"VERSION": version,
	})
}

// AwaitCL waits for the specified CL to be submitted, and returns the new
// branch head. Callers can pass baseCommit, the current branch head, to verify
// that no CLs were submitted between when the CL was created and when it was
// merged. If changeID is blank because the intended CL was a no-op, baseCommit
// is returned immediately.
func (t *VersionTasks) AwaitCL(ctx *workflow.TaskContext, changeID, baseCommit string) (string, error) {
	if changeID == "" {
		ctx.Printf("No CL was necessary")
		return baseCommit, nil
	}

	ctx.Printf("Awaiting review/submit of %v", ChangeLink(changeID))
	return t.Gerrit.AwaitSubmit(ctx, changeID, baseCommit)
}

// ReadBranchHead returns the current HEAD revision of branch.
func (t *VersionTasks) ReadBranchHead(ctx *workflow.TaskContext, branch string) (string, error) {
	return t.Gerrit.ReadBranchHead(ctx, t.GoProject, branch)
}

func (t *VersionTasks) CheckBranchHead(ctx *workflow.TaskContext, branch, expectedCommit string) error {
	head, err := t.ReadBranchHead(ctx, branch)
	if err != nil {
		return err
	}
	if head != expectedCommit {
		return fmt.Errorf("head of branch %q is %q, wanted %q", branch, head, expectedCommit)
	}
	return nil
}

// TagRelease tags commit as version.
func (t *VersionTasks) TagRelease(ctx *workflow.TaskContext, version, commit string) error {
	return t.Gerrit.Tag(ctx, t.GoProject, version, commit)
}
