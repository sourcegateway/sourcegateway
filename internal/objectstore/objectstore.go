// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
)

// ErrPreconditionFailed reports an unsatisfied conditional object operation.
var ErrPreconditionFailed = errors.New("precondition failed")

// ErrMutationOutcomeUnknown reports that an upstream mutation may have committed.
var ErrMutationOutcomeUnknown = errors.New("mutation outcome unknown")

// ObjectInfo describes an object returned by a provider.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentRange string
}

// ListResult contains one page of objects and common prefixes.
type ListResult struct {
	Objects   []ObjectInfo
	Prefixes  []string
	NextToken string
}

// PutOptions carries content metadata and HTTP preconditions.
type PutOptions struct {
	ContentType           string
	IfMatch               string
	IfNoneMatch           string
	CopySourceIfMatch     string
	CopySourceIfNoneMatch string
}

// ObjectProvider defines the storage operations used by the gateway.
type ObjectProvider interface {
	HeadBucket(context.Context) error
	List(context.Context, string, string, string, int32) (ListResult, error)
	Head(context.Context, string) (ObjectInfo, error)
	Get(context.Context, string, string) (io.ReadCloser, ObjectInfo, error)
	Put(context.Context, string, io.Reader, int64, PutOptions) (ObjectInfo, error)
	Delete(context.Context, string) error
	Copy(context.Context, string, string, PutOptions) (ObjectInfo, error)
}

// ResolveOrigin returns the exact network origin permitted for a backend.
func ResolveOrigin(ctx context.Context, backend v1alpha1.ObjectBackend, credential config.Credential) (string, error) {
	switch backend.Provider {
	case "s3":
		if credential.Kind != "awsAccessKey" && credential.Kind != "awsDefault" {
			return "", errors.New("S3 backend requires an AWS credential")
		}
		if credential.Origin != "" {
			origin, err := config.NormalizeOrigin(credential.Origin)
			if err != nil {
				return "", fmt.Errorf("validate S3 origin: %w", err)
			}
			return origin, nil
		}
		if backend.Region == "" {
			return "", errors.New("S3 region is required")
		}
		endpoint, err := s3.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, s3.EndpointParameters{
			Region:         aws.String(backend.Region),
			UseDualStack:   aws.Bool(true),
			ForcePathStyle: aws.Bool(true),
		})
		if err != nil {
			return "", fmt.Errorf("resolve S3 origin: %w", err)
		}
		origin, err := config.NormalizeOrigin(endpoint.URI.String())
		if err != nil {
			return "", fmt.Errorf("validate resolved S3 origin: %w", err)
		}
		return origin, nil
	case "gcs":
		if credential.Kind != "gcsDefault" && credential.Kind != "gcsServiceAccount" {
			return "", errors.New("GCS backend requires a GCS credential")
		}
		origin, err := config.NormalizeOrigin(credential.Origin)
		if err != nil {
			return "", fmt.Errorf("validate GCS origin: %w", err)
		}
		return origin, nil
	default:
		return "", fmt.Errorf("unsupported object provider %q", backend.Provider)
	}
}

// CredentialRevision hashes the credential material used by a backend.
func CredentialRevision(cfg *config.Config, namespace string, backend v1alpha1.ObjectBackend) (string, error) {
	credential, err := cfg.Credential("objectStorageBucket", backend.Credential, namespace)
	if err != nil {
		return "", err
	}
	files := map[string][]string{
		"awsAccessKey":      {"access-key-id", "secret-access-key", "session-token"},
		"gcsServiceAccount": {"service-account.json"},
	}[credential.Kind]
	digest := sha256.New()
	_, _ = digest.Write([]byte(credential.Kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(backend.Endpoint))
	contents, err := cfg.ReadCredentialFiles(backend.Credential, files...)
	if err != nil && credential.Kind == "awsAccessKey" && errors.Is(err, os.ErrNotExist) {
		files = files[:2]
		contents, err = cfg.ReadCredentialFiles(backend.Credential, files...)
	}
	if err != nil {
		return "", err
	}
	for _, name := range files {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(name))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(contents[name])
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// sameOriginRedirect limits redirects to three hops within the original origin.
func sameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) != 0 && !strings.EqualFold(request.URL.Scheme+"://"+request.URL.Host, via[0].URL.Scheme+"://"+via[0].URL.Host) {
		return errors.New("cross-origin redirect rejected")
	}
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	return nil
}
