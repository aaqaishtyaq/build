// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitCheckout sets w.Dir to the work directory
// at $HOME/go-releasebot-work/<release> (where <release> is a string like go1.8.5),
// creating it if it doesn't exist,
// and sets up a fresh git checkout in which to work,
// in $HOME/go-releasebot-work/<release>/gitwork.
//
// The first time it is run for a particular release,
// gitCheckout also creates a clean checkout in
// $HOME/go-releasebot-work/<release>/gitmirror,
// to use as an object cache to speed future checkouts.
//
// For beta releases in -mode=release, it sets w.Dir but doesn't
// fetch the latest master branch. This way the exact commit that
// was tested in prepare mode is also used in release mode.
func (w *Work) gitCheckout() {
	w.Dir = filepath.Join(os.Getenv("HOME"), "go-releasebot-work/"+strings.ToLower(w.Version))
	w.log.Printf("working in %s\n", w.Dir)
	if err := os.MkdirAll(w.Dir, 0777); err != nil {
		w.log.Panic(err)
	}

	if w.BetaRelease && !w.Prepare {
		// We don't want to fetch the latest "master" branch for
		// a beta release. Instead, reuse the same commit that was
		// already fetched from origin and tested in prepare mode.
		return
	}

	const origin = "https://go.googlesource.com/go"

	// Check out a local mirror to work-mirror, to speed future checkouts for this point release.
	mirror := filepath.Join(w.Dir, "gitmirror")
	r := w.runner(mirror)
	if _, err := os.Stat(mirror); err != nil {
		w.runner(w.Dir).run("git", "clone", origin, mirror)
		r.run("git", "config", "gc.auto", "0") // don't throw away refs we fetch
	} else {
		r.run("git", "fetch", "origin", "master")
	}
	r.run("git", "fetch", "origin", w.ReleaseBranch)

	// Clone real Gerrit, but using local mirror for most objects.
	gitDir := filepath.Join(w.Dir, "gitwork")
	if err := os.RemoveAll(gitDir); err != nil {
		w.log.Panic(err)
	}
	w.runner(w.Dir).run("git", "clone", "--reference", mirror, "-b", w.ReleaseBranch, origin, gitDir)

	r = w.runner(gitDir)
	r.run("git", "codereview", "change", "relwork")
	r.run("git", "config", "gc.auto", "0") // don't throw away refs we fetch
}

// gitTagExists returns whether a git tag is already present in the repository.
func (w *Work) gitTagExists() bool {
	_, err := w.runner(filepath.Join(w.Dir, "gitwork")).runErr("git", "rev-parse", w.Version)
	return err == nil
}

// gitTagVersion tags the release candidate or release in Git.
func (w *Work) gitTagVersion() {
	r := w.runner(filepath.Join(w.Dir, "gitwork"))
	if w.gitTagExists() {
		out := r.runOut("git", "rev-parse", w.Version)
		w.VersionCommit = strings.TrimSpace(string(out))
		w.log.Printf("Git tag already exists (%s), resuming release.", w.VersionCommit)
		return
	}
	out := r.runOut("git", "rev-parse", "HEAD")
	w.VersionCommit = strings.TrimSpace(string(out))
	out = r.runOut("git", "show", w.VersionCommit)
	fmt.Printf("About to tag the following commit as %s:\n\n%s\n\nOk? (Y/n) ", w.Version, out)
	if dryRun {
		fmt.Println("dry-run")
		return
	}
	var resp string
	if _, err := fmt.Scanln(&resp); err != nil {
		w.log.Panic(err)
	} else if resp != "Y" && resp != "y" {
		w.log.Fatal("stopped as requested")
	}
	out, err := r.runErr("git", "tag", w.Version, w.VersionCommit)
	if err != nil {
		w.logError("git tag failed: %s\n%s", err, out)
		return
	}
	r.run("git", "push", "origin", w.Version)
}

// gitHeadCommit returns the hash of the HEAD commit.
func (w *Work) gitHeadCommit() string {
	r := w.runner(filepath.Join(w.Dir, "gitwork"))
	out := r.runOut("git", "rev-parse", "HEAD")
	return strings.TrimSpace(string(out))
}
