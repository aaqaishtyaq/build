// Copyright 2022 Go Authors All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package task

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"golang.org/x/build/buildenv"
	"golang.org/x/build/buildlet"
	"golang.org/x/build/dashboard"
	"golang.org/x/build/internal/releasetargets"
	"golang.org/x/build/internal/workflow"
)

// WriteSourceArchive writes a source archive to out, based on revision with version written in as VERSION.
func WriteSourceArchive(ctx *workflow.TaskContext, gerritURL, revision, version string, out io.Writer) error {
	ctx.Printf("Create source archive.")
	tarURL := gerritURL + "/go/+archive/" + revision + ".tar.gz"
	resp, err := http.Get(tarURL)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch %q: %v", tarURL, resp.Status)
	}
	defer resp.Body.Close()
	gzReader, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	reader := tar.NewReader(gzReader)

	gzWriter := gzip.NewWriter(out)
	writer := tar.NewWriter(gzWriter)

	// Add go/VERSION to the archive, and fix up the existing contents.
	if err := writer.WriteHeader(&tar.Header{
		Name:       "go/VERSION",
		Size:       int64(len(version)),
		Typeflag:   tar.TypeReg,
		Mode:       0644,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	}); err != nil {
		return err
	}
	if _, err := writer.Write([]byte(version)); err != nil {
		return err
	}
	if err := adjustTar(reader, writer, "go/", []adjustFunc{
		dropRegexpMatches([]string{`VERSION`}), // Don't overwrite our VERSION file from above.
		dropRegexpMatches(dropPatterns),
		fixPermissions(),
	}); err != nil {
		return err
	}

	if err := writer.Close(); err != nil {
		return err
	}
	return gzWriter.Close()
}

// An adjustFunc updates a tar file header in some way.
// The input is safe to write to. A nil return means to drop the file.
type adjustFunc func(*tar.Header) *tar.Header

// adjustTar copies the files from reader to writer, putting them in prefixDir
// and adjusting them with adjusts along the way. Prefix must have a trailing /.
func adjustTar(reader *tar.Reader, writer *tar.Writer, prefixDir string, adjusts []adjustFunc) error {
	if !strings.HasSuffix(prefixDir, "/") {
		return fmt.Errorf("prefix dir %q must have a trailing /", prefixDir)
	}
	writer.WriteHeader(&tar.Header{
		Name:       prefixDir,
		Typeflag:   tar.TypeDir,
		Mode:       0755,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	})
file:
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		headerCopy := *header
		newHeader := &headerCopy
		for _, adjust := range adjusts {
			newHeader = adjust(newHeader)
			if newHeader == nil {
				continue file
			}
		}
		newHeader.Name = prefixDir + newHeader.Name
		writer.WriteHeader(newHeader)
		if _, err := io.Copy(writer, reader); err != nil {
			return err
		}
	}
	return nil
}

var dropPatterns = []string{
	// .gitattributes, .github, etc.
	`\..*`,
	// This shouldn't exist, since we create a VERSION file.
	`VERSION.cache`,
	// Remove the build cache that the toolchain build process creates.
	// According to go.dev/cl/82095, it shouldn't exist at all.
	`pkg/obj/.*`,
	// Users don't need the api checker binary pre-built. It's
	// used by tests, but all.bash builds it first.
	`pkg/tool/[^/]+/api.*`,
	// Users also don't need the metadata command, which is run dynamically
	// by cmd/dist. As of writing we don't know why it's showing up at all.
	`pkg/tool/[^/]+/metadata.*`,
	// Remove pkg/${GOOS}_${GOARCH}/cmd. This saves a bunch of
	// space, and users don't typically rebuild cmd/compile,
	// cmd/link, etc. If they want to, they still can, but they'll
	// have to pay the cost of rebuilding dependent libaries. No
	// need to ship them just in case.
	`pkg/[^/]+/cmd/.*`,
	// Clean up .exe~ files; see go.dev/issue/23894.
	`.*\.exe~`,
}

// dropRegexpMatches drops files whose name matches any of patterns.
func dropRegexpMatches(patterns []string) adjustFunc {
	var rejectRegexps []*regexp.Regexp
	for _, pattern := range patterns {
		rejectRegexps = append(rejectRegexps, regexp.MustCompile("^"+pattern+"$"))
	}
	return func(h *tar.Header) *tar.Header {
		for _, regexp := range rejectRegexps {
			if regexp.MatchString(h.Name) {
				return nil
			}
		}
		return h
	}
}

// dropUnwantedSysos drops race detector sysos for other architectures.
func dropUnwantedSysos(target *releasetargets.Target) adjustFunc {
	raceSysoRegexp := regexp.MustCompile(`^src/runtime/race/race_(.*?).syso$`)
	osarch := target.GOOS + "_" + target.GOARCH
	return func(h *tar.Header) *tar.Header {
		matches := raceSysoRegexp.FindStringSubmatch(h.Name)
		if matches != nil && matches[1] != osarch {
			return nil
		}
		return h
	}
}

// fixPermissions sets files' permissions to user-writeable, world-readable.
func fixPermissions() adjustFunc {
	return func(h *tar.Header) *tar.Header {
		if h.Typeflag == tar.TypeDir || h.Mode&0111 != 0 {
			h.Mode = 0755
		} else {
			h.Mode = 0644
		}
		return h
	}
}

// fixupCrossCompile moves cross-compiled tools to their final location and
// drops unnecessary host architecture files.
func fixupCrossCompile(target *releasetargets.Target) adjustFunc {
	if !strings.HasSuffix(target.Builder, "-crosscompile") {
		return func(h *tar.Header) *tar.Header { return h }
	}
	osarch := target.GOOS + "_" + target.GOARCH
	return func(h *tar.Header) *tar.Header {
		// Move cross-compiled tools up to bin/, and drop the existing contents.
		if strings.HasPrefix(h.Name, "bin/") {
			if strings.HasPrefix(h.Name, "bin/"+osarch) {
				h.Name = strings.ReplaceAll(h.Name, "bin/"+osarch, "bin")
			} else {
				return nil
			}
		}
		// Drop host architecture files.
		if strings.HasPrefix(h.Name, "pkg/linux_amd64") ||
			strings.HasPrefix(h.Name, "pkg/tool/linux_amd64") {
			return nil
		}
		return h
	}
}

const (
	goDir = "go"
	go14  = "go1.4"
)

type BuildletStep struct {
	Target      *releasetargets.Target
	Buildlet    buildlet.Client
	BuildConfig *dashboard.BuildConfig
	Watch       bool
}

// BuildBinary builds a binary distribution from sourceArchive and writes it to out.
func (b *BuildletStep) BuildBinary(ctx *workflow.TaskContext, sourceArchive io.Reader, out io.Writer) error {
	buildEnv := buildenv.Production
	// Push source to buildlet.
	ctx.Printf("Pushing source to buildlet.")
	if err := b.Buildlet.PutTar(ctx, sourceArchive, ""); err != nil {
		return fmt.Errorf("failed to put generated source tarball: %v", err)
	}

	if u := b.BuildConfig.GoBootstrapURL(buildEnv); u != "" {
		ctx.Printf("Installing go1.4.")
		if err := b.Buildlet.PutTarFromURL(ctx, u, go14); err != nil {
			return err
		}
	}

	// Execute build (make.bash only first).
	ctx.Printf("Building (make.bash only).")
	if err := b.exec(ctx, goDir+"/"+b.BuildConfig.MakeScript(), b.BuildConfig.MakeScriptArgs(), buildlet.ExecOpts{
		ExtraEnv: b.makeEnv(),
	}); err != nil {
		return err
	}

	if b.Target.Race {
		ctx.Printf("Building race detector.")
		if err := b.runGo(ctx, []string{"install", "-race", "std"}, buildlet.ExecOpts{
			ExtraEnv: b.makeEnv(),
		}); err != nil {
			return err
		}
	}

	ctx.Printf("Building release tarball.")
	input, err := b.Buildlet.GetTar(ctx, "go")
	if err != nil {
		return err
	}
	defer input.Close()

	gzReader, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer gzReader.Close()
	reader := tar.NewReader(gzReader)
	gzWriter := gzip.NewWriter(out)
	writer := tar.NewWriter(gzWriter)
	if err := adjustTar(reader, writer, "go/", []adjustFunc{
		dropRegexpMatches(dropPatterns),
		dropUnwantedSysos(b.Target),
		fixupCrossCompile(b.Target),
		fixPermissions(),
	}); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return gzWriter.Close()
}

func (b *BuildletStep) makeEnv() []string {
	// We need GOROOT_FINAL both during the binary build and test runs. See go.dev/issue/52236.
	makeEnv := []string{"GOROOT_FINAL=" + b.BuildConfig.GorootFinal()}
	// Add extra vars from the target's configuration.
	makeEnv = append(makeEnv, b.Target.ExtraEnv...)
	return makeEnv
}

//go:embed releaselet/releaselet.go
var releaselet string

func (b *BuildletStep) BuildMSI(ctx *workflow.TaskContext, binaryArchive io.Reader, msi io.Writer) error {
	if err := b.Buildlet.PutTar(ctx, binaryArchive, ""); err != nil {
		return err
	}
	ctx.Printf("Pushing and running releaselet.")
	if err := b.Buildlet.Put(ctx, strings.NewReader(releaselet), "releaselet.go", 0666); err != nil {
		return err
	}
	if err := b.runGo(ctx, []string{"run", "releaselet.go"}, buildlet.ExecOpts{
		Dir: ".", // root of buildlet work directory
	}); err != nil {
		ctx.Printf("releaselet failed: %v", err)
		b.Buildlet.ListDir(ctx, ".", buildlet.ListDirOpts{Recursive: true}, func(ent buildlet.DirEntry) {
			ctx.Printf("remote: %v", ent)
		})
		return err
	}
	return b.fetchFile(ctx, msi, "msi")
}

// fetchFile fetches the specified directory from the given buildlet, and
// writes the first file it finds in that directory to dest.
func (b *BuildletStep) fetchFile(ctx *workflow.TaskContext, dest io.Writer, dir string) error {
	ctx.Printf("Downloading file from %q.", dir)
	tgz, err := b.Buildlet.GetTar(context.Background(), dir)
	if err != nil {
		return err
	}
	defer tgz.Close()
	zr, err := gzip.NewReader(tgz)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		} else if err != nil {
			return err
		}
		if !h.FileInfo().IsDir() {
			break
		}
	}
	_, err = io.Copy(dest, tr)
	return err
}

func (b *BuildletStep) TestTarget(ctx *workflow.TaskContext, binaryArchive io.Reader) error {
	buildEnv := buildenv.Production
	if err := b.Buildlet.PutTar(ctx, binaryArchive, ""); err != nil {
		return err
	}
	if u := b.BuildConfig.GoBootstrapURL(buildEnv); u != "" {
		ctx.Printf("Installing go1.4 (second time, for all.bash).")
		if err := b.Buildlet.PutTarFromURL(ctx, u, go14); err != nil {
			return err
		}
	}
	ctx.Printf("Building (all.bash to ensure tests pass).")
	return b.exec(ctx, goDir+"/"+b.BuildConfig.AllScript(), b.BuildConfig.AllScriptArgs(), buildlet.ExecOpts{
		ExtraEnv: b.makeEnv(),
	})
}

// exec runs cmd with args. Its working dir is opts.Dir, or the directory of cmd.
// Its environment is the buildlet's environment, plus a GOPATH setting, plus opts.ExtraEnv.
// If the command fails, its output is included in the returned error.
func (b *BuildletStep) exec(ctx context.Context, cmd string, args []string, opts buildlet.ExecOpts) error {
	work, err := b.Buildlet.WorkDir(ctx)
	if err != nil {
		return err
	}

	// Set up build environment. The caller's environment wins if there's a conflict.
	env := append(b.BuildConfig.Env(), "GOPATH="+work+"/gopath")
	env = append(env, opts.ExtraEnv...)
	out := &bytes.Buffer{}
	opts.Output = out
	opts.ExtraEnv = env
	opts.Args = args
	if b.Watch {
		opts.Output = io.MultiWriter(opts.Output, os.Stdout)
	}
	remoteErr, execErr := b.Buildlet.Exec(ctx, cmd, opts)
	if execErr != nil {
		return execErr
	}
	if remoteErr != nil {
		return fmt.Errorf("Command %v %s failed: %v\nOutput:\n%v", cmd, args, remoteErr, out)
	}

	return nil
}

func (b *BuildletStep) runGo(ctx context.Context, args []string, execOpts buildlet.ExecOpts) error {
	goCmd := goDir + "/bin/go"
	if b.Target.GOOS == "windows" {
		goCmd += ".exe"
	}
	execOpts.Args = args
	return b.exec(ctx, goCmd, args, execOpts)
}

func ConvertTGZToZIP(r io.Reader, w io.Writer) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)

	zw := zip.NewWriter(w)
	for {
		th, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		fi := th.FileInfo()
		zh, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		zh.Name = th.Name // for the full path
		switch strings.ToLower(path.Ext(zh.Name)) {
		case ".jpg", ".jpeg", ".png", ".gif":
			// Don't re-compress already compressed files.
			zh.Method = zip.Store
		default:
			zh.Method = zip.Deflate
		}
		if fi.IsDir() {
			zh.Method = zip.Store
		}
		w, err := zw.CreateHeader(zh)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			continue
		}
		if _, err := io.Copy(w, tr); err != nil {
			return err
		}
	}
	return zw.Close()
}
