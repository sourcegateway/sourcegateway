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
	"os"
	"os/signal"
	"syscall"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// main starts a local Kubernetes control plane for development.
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "dev-envtest:", err)
		os.Exit(1)
	}
}

// run starts envtest and serves it until the process is interrupted.
func run(parent context.Context, arguments []string) error {
	flags := flag.NewFlagSet("dev-envtest", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	assets := flags.String("assets", "", "directory containing kube-apiserver and etcd")
	crds := flags.String("crds", "deploy/crds", "CRD directory")
	kubeconfig := flags.String("output-kubeconfig", "", "output kubeconfig path")
	serviceAccountKey := flags.String("service-account-key", "", "RSA private key for ServiceAccount tokens")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if *assets == "" || *kubeconfig == "" || *serviceAccountKey == "" {
		return errors.New("assets, output-kubeconfig, and service-account-key are required")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: *assets, CRDDirectoryPaths: []string{*crds}, ErrorIfCRDPathMissing: true,
		AttachControlPlaneOutput: true,
	}
	apiArguments := environment.ControlPlane.GetAPIServer().Configure()
	apiArguments.Set("service-account-signing-key-file", *serviceAccountKey)
	apiArguments.Set("service-account-key-file", *serviceAccountKey)
	apiArguments.Set("service-account-issuer", "https://sourcegateway.dev/envtest")
	apiArguments.Set("api-audiences", "sourcegateway.dev")
	_, err := environment.Start()
	if err != nil {
		return fmt.Errorf("start envtest: %w", err)
	}
	if err := os.WriteFile(*kubeconfig, environment.KubeConfig, 0o600); err != nil {
		if stopErr := environment.Stop(); stopErr != nil {
			return errors.Join(fmt.Errorf("write kubeconfig: %w", err), fmt.Errorf("stop envtest: %w", stopErr))
		}
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	if err := environment.Stop(); err != nil {
		return fmt.Errorf("stop envtest: %w", err)
	}
	return nil
}
