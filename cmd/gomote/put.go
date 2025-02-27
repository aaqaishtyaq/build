// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/build/internal/gomote/protos"
	"golang.org/x/build/tarutil"
)

// legacyPutTar a .tar.gz
func legacyPutTar(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "puttar usage: gomote puttar [put-opts] <buildlet-name> [tar.gz file or '-' for stdin]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var rev string
	fs.StringVar(&rev, "gorev", "", "If non-empty, git hash to download from gerrit and put to the buildlet. e.g. 886b02d705ff for Go 1.4.1. This just maps to the --URL flag, so the two options are mutually exclusive.")
	var dir string
	fs.StringVar(&dir, "dir", "", "relative directory from buildlet's work dir to extra tarball into")
	var tarURL string
	fs.StringVar(&tarURL, "url", "", "URL of tarball, instead of provided file.")

	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
	}
	if rev != "" {
		if tarURL != "" {
			fmt.Fprintln(os.Stderr, "--gorev and --url are mutually exclusive")
			fs.Usage()
		}
		tarURL = "https://go.googlesource.com/go/+archive/" + rev + ".tar.gz"
	}

	name := fs.Arg(0)
	bc, err := remoteClient(name)
	if err != nil {
		return err
	}

	ctx := context.Background()

	if tarURL != "" {
		if fs.NArg() != 1 {
			fs.Usage()
		}
		if err := bc.PutTarFromURL(ctx, tarURL, dir); err != nil {
			return err
		}
		if rev != "" {
			// Put a VERSION file there too, to avoid git usage.
			version := strings.NewReader("devel " + rev)
			var vtar tarutil.FileList
			vtar.AddRegular(&tar.Header{
				Name: "VERSION",
				Mode: 0644,
				Size: int64(version.Len()),
			}, int64(version.Len()), version)
			tgz := vtar.TarGz()
			defer tgz.Close()
			return bc.PutTar(ctx, tgz, dir)
		}
		return nil
	}

	var tgz io.Reader = os.Stdin
	if fs.NArg() == 2 && fs.Arg(1) != "-" {
		f, err := os.Open(fs.Arg(1))
		if err != nil {
			return err
		}
		defer f.Close()
		tgz = f
	}
	return bc.PutTar(ctx, tgz, dir)
}

// putTar a .tar.gz
func putTar(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "puttar usage: gomote puttar [put-opts] <buildlet-name> [tar.gz file or '-' for stdin]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	var rev string
	fs.StringVar(&rev, "gorev", "", "If non-empty, git hash to download from gerrit and put to the buildlet. e.g. 886b02d705ff for Go 1.4.1. This just maps to the --URL flag, so the two options are mutually exclusive.")
	var dir string
	fs.StringVar(&dir, "dir", "", "relative directory from buildlet's work dir to extra tarball into")
	var tarURL string
	fs.StringVar(&tarURL, "url", "", "URL of tarball, instead of provided file.")

	fs.Parse(args)
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
	}
	if rev != "" && tarURL != "" {
		fmt.Fprintln(os.Stderr, "--gorev and --url are mutually exclusive")
		fs.Usage()
	}
	name := fs.Arg(0)
	ctx := context.Background()
	client := gomoteServerClient(ctx)

	if rev != "" {
		tarURL = "https://go.googlesource.com/go/+archive/" + rev + ".tar.gz"
	}
	if tarURL != "" {
		if fs.NArg() != 1 {
			fs.Usage()
		}
		_, err := client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
			GomoteId:  name,
			Directory: dir,
			Url:       tarURL,
		})
		if err != nil {
			return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
		}
		if rev != "" {
			// Put a VERSION file there too, to avoid git usage.
			version := strings.NewReader("devel " + rev)
			var vtar tarutil.FileList
			vtar.AddRegular(&tar.Header{
				Name: "VERSION",
				Mode: 0644,
				Size: int64(version.Len()),
			}, int64(version.Len()), version)
			tgz := vtar.TarGz()
			defer tgz.Close()

			resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
			if err != nil {
				return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
			}
			if err := uploadToGCS(ctx, resp.GetFields(), tgz, resp.GetObjectName(), resp.GetUrl()); err != nil {
				return fmt.Errorf("unable to upload version file to GCS: %s", err)
			}
			if _, err = client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
				GomoteId:  name,
				Directory: dir,
				Url:       fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
			}); err != nil {
				return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
			}
		}
		return nil
	}
	var tgz io.Reader = os.Stdin
	if fs.NArg() != 2 {
		fs.Usage()
	}
	if fs.Arg(1) != "-" {
		f, err := os.Open(fs.Arg(1))
		if err != nil {
			return err
		}
		defer f.Close()
		tgz = f
	}
	resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
	if err != nil {
		return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
	}
	if err := uploadToGCS(ctx, resp.GetFields(), tgz, resp.GetObjectName(), resp.GetUrl()); err != nil {
		return fmt.Errorf("unable to upload file to GCS: %s", err)
	}
	if _, err := client.WriteTGZFromURL(ctx, &protos.WriteTGZFromURLRequest{
		GomoteId:  name,
		Directory: dir,
		Url:       fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
	}); err != nil {
		return fmt.Errorf("unable to write tar to instance: %s", statusFromError(err))
	}
	return nil
}

// put go1.4 in the workdir
func put14(args []string) error {
	fs := flag.NewFlagSet("put14", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put14 usage: gomote put14 <buildlet-name>")
		fs.PrintDefaults()
		os.Exit(1)
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
	}
	name := fs.Arg(0)
	bc, conf, err := clientAndConf(name)
	if err != nil {
		return err
	}
	u := conf.GoBootstrapURL(buildEnv)
	if u == "" {
		fmt.Printf("No GoBootstrapURL defined for %q; ignoring. (may be baked into image)\n", name)
		return nil
	}
	ctx := context.Background()
	return bc.PutTarFromURL(ctx, u, "go1.4")
}

// putBootstrap places the bootstrap version of go in the workdir
func putBootstrap(args []string) error {
	fs := flag.NewFlagSet("putbootstrap", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "putbootstrap usage: gomote putbootstrap <buildlet-name>")
		fs.PrintDefaults()
		os.Exit(1)
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
	}
	name := fs.Arg(0)
	ctx := context.Background()
	client := gomoteServerClient(ctx)
	resp, err := client.AddBootstrap(ctx, &protos.AddBootstrapRequest{
		GomoteId: name,
	})
	if err != nil {
		return fmt.Errorf("unable to add bootstrap version of Go to instance: %s", statusFromError(err))
	}
	if resp.GetBootstrapGoUrl() == "" {
		fmt.Printf("No GoBootstrapURL defined for %q; ignoring. (may be baked into image)\n", name)
	}
	return nil
}

// legacyPut single file
func legacyPut(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put usage: gomote put [put-opts] <buildlet-name> <source or '-' for stdin> [destination]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	modeStr := fs.String("mode", "", "Unix file mode (octal); default to source file mode")
	fs.Parse(args)
	if n := fs.NArg(); n < 2 || n > 3 {
		fs.Usage()
	}

	bc, err := remoteClient(fs.Arg(0))
	if err != nil {
		return err
	}

	var r io.Reader = os.Stdin
	var mode os.FileMode = 0666

	src := fs.Arg(1)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f

		if *modeStr == "" {
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			mode = fi.Mode()
		}
	}
	if *modeStr != "" {
		modeInt, err := strconv.ParseInt(*modeStr, 8, 64)
		if err != nil {
			return err
		}
		mode = os.FileMode(modeInt)
		if !mode.IsRegular() {
			return fmt.Errorf("bad mode: %v", mode)
		}
	}

	dest := fs.Arg(2)
	if dest == "" {
		if src == "-" {
			return errors.New("must specify destination file name when source is standard input")
		}
		dest = filepath.Base(src)
	}

	ctx := context.Background()
	return bc.Put(ctx, r, dest, mode)
}

// put single file
func put(args []string) error {
	fs := flag.NewFlagSet("put", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "put usage: gomote put [put-opts] <buildlet-name> <source or '-' for stdin> [destination]")
		fs.PrintDefaults()
		os.Exit(1)
	}
	modeStr := fs.String("mode", "", "Unix file mode (octal); default to source file mode")
	fs.Parse(args)
	if n := fs.NArg(); n < 2 || n > 3 {
		fs.Usage()
	}

	var r io.Reader = os.Stdin
	var mode os.FileMode = 0666

	src := fs.Arg(1)
	if src != "-" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f

		if *modeStr == "" {
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			mode = fi.Mode()
		}
	}
	if *modeStr != "" {
		modeInt, err := strconv.ParseInt(*modeStr, 8, 64)
		if err != nil {
			return err
		}
		mode = os.FileMode(modeInt)
		if !mode.IsRegular() {
			return fmt.Errorf("bad mode: %v", mode)
		}
	}
	dest := fs.Arg(2)
	if dest == "" {
		if src == "-" {
			return errors.New("must specify destination file name when source is standard input")
		}
		dest = filepath.Base(src)
	}
	ctx := context.Background()
	client := gomoteServerClient(ctx)
	resp, err := client.UploadFile(ctx, &protos.UploadFileRequest{})
	if err != nil {
		return fmt.Errorf("unable to request credentials for a file upload: %s", statusFromError(err))
	}
	err = uploadToGCS(ctx, resp.GetFields(), r, dest, resp.GetUrl())
	if err != nil {
		return fmt.Errorf("unable to upload file to GCS: %s", err)
	}
	name := fs.Arg(0)
	_, err = client.WriteFileFromURL(ctx, &protos.WriteFileFromURLRequest{
		GomoteId: name,
		Url:      fmt.Sprintf("%s%s", resp.GetUrl(), resp.GetObjectName()),
		Filename: dest,
		Mode:     uint32(mode),
	})
	if err != nil {
		return fmt.Errorf("unable to write the file from URL: %s", statusFromError(err))
	}
	return nil
}

func uploadToGCS(ctx context.Context, fields map[string]string, file io.Reader, filename, url string) error {
	buf := new(bytes.Buffer)
	mw := multipart.NewWriter(buf)

	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("unable to write field: %s", err)
		}
	}
	_, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("unable to create form file: %s", err)
	}
	// Write our own boundary to avoid buffering entire file into the multipart Writer
	bound := fmt.Sprintf("\r\n--%s--\r\n", mw.Boundary())
	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(io.MultiReader(buf, file, strings.NewReader(bound))))
	if err != nil {
		return fmt.Errorf("unable to create request: %s", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %s", err)
	}
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("http post failed: status code=%d", res.StatusCode)
	}
	return nil
}
