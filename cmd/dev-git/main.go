// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi" // #nosec G504 -- this development-only server runs a fixed local git-http-backend CGI.
	"os"
	"os/exec"
	"time"
)

// main serves local bare repositories over HTTPS for development.
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "dev-git:", err)
		os.Exit(1)
	}
}

// run serves local bare repositories over HTTPS.
func run(_ context.Context, arguments []string) error {
	flags := flag.NewFlagSet("dev-git", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	listen := flags.String("listen", "127.0.0.1:9443", "HTTPS listen address")
	root := flags.String("root", "", "directory containing bare repositories")
	certificate := flags.String("certificate", "", "TLS certificate")
	key := flags.String("key", "", "TLS private key")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if *root == "" || *certificate == "" || *key == "" {
		return errors.New("root, certificate, and key are required")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("find git executable: %w", err)
	}
	handler := &cgi.Handler{
		Path: git, Args: []string{"http-backend"}, Dir: *root,
		Env:    []string{"GIT_PROJECT_ROOT=" + *root, "GIT_HTTP_EXPORT_ALL=1"},
		Stderr: os.Stderr,
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServeTLS(*certificate, *key); err != nil {
		return fmt.Errorf("serve Git over HTTPS: %w", err)
	}
	return nil
}
