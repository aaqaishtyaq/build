// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/build/buildlet"
	"golang.org/x/build/internal/gomote/protos"
)

func legacyLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "ls usage: gomote ls <instance> [-R] [dir]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var recursive bool
	fs.BoolVar(&recursive, "R", false, "recursive")
	var digest bool
	fs.BoolVar(&digest, "d", false, "get file digests")
	var skip string
	fs.StringVar(&skip, "skip", "", "comma-separated list of relative directories to skip (use forward slashes)")
	fs.Parse(args)

	dir := "."
	if n := fs.NArg(); n < 1 || n > 2 {
		fs.Usage()
	} else if n == 2 {
		dir = fs.Arg(1)
	}
	name := fs.Arg(0)
	bc, err := remoteClient(name)
	if err != nil {
		return err
	}
	opts := buildlet.ListDirOpts{
		Recursive: recursive,
		Digest:    digest,
		Skip:      strings.Split(skip, ","),
	}
	return bc.ListDir(context.Background(), dir, opts, func(bi buildlet.DirEntry) {
		fmt.Fprintf(os.Stdout, "%s\n", bi)
	})
}

func ls(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "ls usage: gomote v2 ls <instance> [-R] [dir]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var recursive bool
	fs.BoolVar(&recursive, "R", false, "recursive")
	var digest bool
	fs.BoolVar(&digest, "d", false, "get file digests")
	var skip string
	fs.StringVar(&skip, "skip", "", "comma-separated list of relative directories to skip (use forward slashes)")
	fs.Parse(args)

	dir := "."
	if n := fs.NArg(); n < 1 || n > 2 {
		fs.Usage()
	} else if n == 2 {
		dir = fs.Arg(1)
	}
	name := fs.Arg(0)
	ctx := context.Background()
	client := gomoteServerClient(ctx)
	resp, err := client.ListDirectory(ctx, &protos.ListDirectoryRequest{
		GomoteId:  name,
		Directory: dir,
		Recursive: recursive,
		SkipFiles: strings.Split(skip, ","),
		Digest:    digest,
	})
	if err != nil {
		return fmt.Errorf("unable to ls: %s", statusFromError(err))
	}
	for _, entry := range resp.GetEntries() {
		fmt.Fprintf(os.Stdout, "%s\n", entry)
	}
	return nil
}
