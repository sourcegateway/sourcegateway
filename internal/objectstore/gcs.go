// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
)

// gcsProvider implements object storage against Google Cloud Storage.
type gcsProvider struct {
	client *storage.Client
	bucket *storage.BucketHandle
}

// NewGCS creates a Google Cloud Storage provider for backend.
func NewGCS(ctx context.Context, cfg *config.Config, namespace string, backend v1alpha1.ObjectBackend) (ObjectProvider, error) {
	credential, err := cfg.Credential("objectStorageBucket", backend.Credential, namespace)
	if err != nil {
		return nil, err
	}
	origin, err := ResolveOrigin(ctx, backend, credential)
	endpoint, endpointOrigin, endpointErr := config.NormalizeEndpoint(backend.Endpoint)
	if err != nil || endpointErr != nil || endpoint != backend.Endpoint || endpointOrigin != origin {
		return nil, fmt.Errorf("GCS endpoint is not permitted by the selected credential")
	}
	var options []option.ClientOption
	if credential.Kind == "gcsServiceAccount" {
		files, err := cfg.ReadCredentialFiles(backend.Credential, "service-account.json")
		if err != nil {
			return nil, err
		}
		options = append(options, option.WithAuthCredentialsJSON(option.ServiceAccount, files["service-account.json"]))
	}
	options = append(options, option.WithScopes(storage.ScopeReadWrite))
	httpClient, _, err := transport.NewHTTPClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	httpClient.CheckRedirect = sameOriginRedirect
	storageOptions := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if backend.Endpoint != "" {
		storageOptions = append(storageOptions, option.WithEndpoint(backend.Endpoint))
	}
	client, err := storage.NewClient(ctx, storageOptions...)
	if err != nil {
		return nil, err
	}
	return &gcsProvider{client: client, bucket: client.Bucket(backend.Bucket)}, nil
}

// HeadBucket checks that the configured bucket is accessible.
func (p *gcsProvider) HeadBucket(ctx context.Context) error {
	_, err := p.bucket.Attrs(ctx)
	return err
}

// List returns one page of objects and common prefixes.
func (p *gcsProvider) List(ctx context.Context, prefix, delimiter, token string, max int32) (ListResult, error) {
	query := &storage.Query{Prefix: prefix, Delimiter: delimiter}
	objectIterator := p.bucket.Objects(ctx, query)
	objectIterator.PageInfo().Token = token
	objectIterator.PageInfo().MaxSize = int(max)
	result := ListResult{}
	for len(result.Objects)+len(result.Prefixes) < int(max) {
		attrs, err := objectIterator.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return ListResult{}, err
		}
		if attrs.Prefix != "" {
			result.Prefixes = append(result.Prefixes, attrs.Prefix)
		} else {
			result.Objects = append(result.Objects, objectInfo(attrs))
		}
	}
	result.NextToken = objectIterator.PageInfo().Token
	return result, nil
}

// Head returns metadata for key.
func (p *gcsProvider) Head(ctx context.Context, key string) (ObjectInfo, error) {
	attrs, err := p.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	return objectInfo(attrs), nil
}

// Get opens all or a byte range of key.
func (p *gcsProvider) Get(ctx context.Context, key, byteRange string) (io.ReadCloser, ObjectInfo, error) {
	object := p.bucket.Object(key)
	var reader *storage.Reader
	var err error
	if byteRange == "" {
		reader, err = object.NewReader(ctx)
	} else {
		rawStart, rawEnd, found := strings.Cut(strings.TrimPrefix(byteRange, "bytes="), "-")
		offset, startErr := strconv.ParseInt(rawStart, 10, 64)
		end, endErr := strconv.ParseInt(rawEnd, 10, 64)
		if !found || startErr != nil || endErr != nil || end < offset {
			return nil, ObjectInfo{}, fmt.Errorf("invalid range")
		}
		reader, err = object.NewRangeReader(ctx, offset, end-offset+1)
	}
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	size := reader.Attrs.Size
	if byteRange != "" {
		rawStart, rawEnd, _ := strings.Cut(strings.TrimPrefix(byteRange, "bytes="), "-")
		start, _ := strconv.ParseInt(rawStart, 10, 64)
		end, _ := strconv.ParseInt(rawEnd, 10, 64)
		size = end - start + 1
	}
	return reader, ObjectInfo{Key: key, Size: size, ETag: strconv.FormatInt(reader.Attrs.Generation, 10), LastModified: reader.Attrs.LastModified}, nil
}

// Put writes exactly size bytes to key subject to options.
func (p *gcsProvider) Put(ctx context.Context, key string, body io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	object, err := conditionedObject(p.bucket.Object(key).Retryer(storage.WithPolicy(storage.RetryNever)), options)
	if err != nil {
		return ObjectInfo{}, err
	}
	writeContext, cancel := context.WithCancel(ctx)
	defer cancel()
	writer := object.NewWriter(writeContext)
	writer.ContentType = options.ContentType
	writer.ChunkSize = 0
	var copyErr error
	if size < 0 {
		_, copyErr = io.Copy(writer, body)
	} else {
		_, copyErr = io.CopyN(writer, body, size)
	}
	if copyErr != nil {
		cancel()
		_ = writer.Close()
		return ObjectInfo{}, copyErr
	}
	if err := writer.Close(); err != nil {
		return ObjectInfo{}, err
	}
	return objectInfo(writer.Attrs()), nil
}

// Delete removes key.
func (p *gcsProvider) Delete(ctx context.Context, key string) error {
	return p.bucket.Object(key).Retryer(storage.WithPolicy(storage.RetryNever)).Delete(ctx)
}

// Copy copies an object within the bucket subject to options.
func (p *gcsProvider) Copy(ctx context.Context, source, destination string, options PutOptions) (ObjectInfo, error) {
	destinationObject, err := conditionedObject(p.bucket.Object(destination).Retryer(storage.WithPolicy(storage.RetryNever)), options)
	if err != nil {
		return ObjectInfo{}, err
	}
	sourceObject, err := conditionedCopySource(p.bucket.Object(source).Retryer(storage.WithPolicy(storage.RetryNever)), options)
	if err != nil {
		return ObjectInfo{}, err
	}
	attrs, err := destinationObject.CopierFrom(sourceObject).Run(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	return objectInfo(attrs), nil
}

// conditionedObject applies destination preconditions to an object handle.
func conditionedObject(object *storage.ObjectHandle, options PutOptions) (*storage.ObjectHandle, error) {
	if options.IfMatch != "" && options.IfNoneMatch != "" {
		return nil, ErrPreconditionFailed
	}
	if options.IfMatch != "" {
		if options.IfMatch == "*" {
			object = object.If(storage.Conditions{GenerationNotMatch: 0})
		} else {
			generation, err := strconv.ParseInt(strings.Trim(options.IfMatch, `"`), 10, 64)
			if err != nil {
				return nil, ErrPreconditionFailed
			}
			object = object.If(storage.Conditions{GenerationMatch: generation})
		}
	} else if options.IfNoneMatch != "" {
		if options.IfNoneMatch == "*" {
			object = object.If(storage.Conditions{DoesNotExist: true})
		} else {
			generation, err := strconv.ParseInt(strings.Trim(options.IfNoneMatch, `"`), 10, 64)
			if err != nil {
				return nil, ErrPreconditionFailed
			}
			object = object.If(storage.Conditions{GenerationNotMatch: generation})
		}
	}
	return object, nil
}

// conditionedCopySource applies copy-source preconditions to an object handle.
func conditionedCopySource(object *storage.ObjectHandle, options PutOptions) (*storage.ObjectHandle, error) {
	if options.CopySourceIfMatch != "" && options.CopySourceIfNoneMatch != "" {
		return nil, ErrPreconditionFailed
	}
	if options.CopySourceIfMatch != "" {
		if options.CopySourceIfMatch != "*" {
			generation, err := strconv.ParseInt(strings.Trim(options.CopySourceIfMatch, `"`), 10, 64)
			if err != nil {
				return nil, ErrPreconditionFailed
			}
			object = object.If(storage.Conditions{GenerationMatch: generation})
		}
	} else if options.CopySourceIfNoneMatch == "*" {
		return nil, ErrPreconditionFailed
	} else if options.CopySourceIfNoneMatch != "" {
		generation, err := strconv.ParseInt(strings.Trim(options.CopySourceIfNoneMatch, `"`), 10, 64)
		if err != nil {
			return nil, ErrPreconditionFailed
		}
		object = object.If(storage.Conditions{GenerationNotMatch: generation})
	}
	return object, nil
}

// objectInfo translates GCS attributes into provider-neutral metadata.
func objectInfo(attrs *storage.ObjectAttrs) ObjectInfo {
	modified := attrs.Updated
	if modified.IsZero() {
		modified = time.Now()
	}
	return ObjectInfo{Key: attrs.Name, Size: attrs.Size, ETag: strconv.FormatInt(attrs.Generation, 10), LastModified: modified}
}
