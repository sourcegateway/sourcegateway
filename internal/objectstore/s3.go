// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
)

// s3Provider implements object storage against an S3-compatible service.
type s3Provider struct {
	client *s3.Client
	bucket string
}

// NewS3 creates an S3-compatible provider for backend.
func NewS3(ctx context.Context, cfg *config.Config, namespace string, backend v1alpha1.ObjectBackend) (ObjectProvider, error) {
	credential, err := cfg.Credential("objectStorageBucket", backend.Credential, namespace)
	if err != nil {
		return nil, err
	}
	origin, err := ResolveOrigin(ctx, backend, credential)
	endpoint, endpointOrigin, endpointErr := config.NormalizeEndpoint(backend.Endpoint)
	if err != nil || endpointErr != nil || endpoint != backend.Endpoint || endpointOrigin != origin {
		return nil, fmt.Errorf("S3 endpoint is not permitted by the selected credential")
	}
	if backend.Region == "" {
		return nil, fmt.Errorf("S3 region is required")
	}
	httpClient := &http.Client{Transport: http.DefaultTransport, CheckRedirect: sameOriginRedirect}
	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(backend.Region), awsconfig.WithHTTPClient(httpClient)}
	if credential.Kind == "awsAccessKey" {
		files, err := cfg.ReadCredentialFiles(backend.Credential, "access-key-id", "secret-access-key", "session-token")
		if errors.Is(err, os.ErrNotExist) {
			files, err = cfg.ReadCredentialFiles(backend.Credential, "access-key-id", "secret-access-key")
		}
		if err != nil {
			return nil, err
		}
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			strings.TrimSpace(string(files["access-key-id"])), strings.TrimSpace(string(files["secret-access-key"])), strings.TrimSpace(string(files["session-token"])),
		)))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = true
		if backend.Endpoint != "" {
			options.BaseEndpoint = aws.String(backend.Endpoint)
		}
	})
	return &s3Provider{client: client, bucket: backend.Bucket}, nil
}

// HeadBucket checks that the configured bucket is accessible.
func (p *s3Provider) HeadBucket(ctx context.Context) error {
	_, err := p.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(p.bucket)})
	return err
}

// List returns one page of objects and common prefixes.
func (p *s3Provider) List(ctx context.Context, prefix, delimiter, token string, max int32) (ListResult, error) {
	output, err := p.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(p.bucket), Prefix: aws.String(prefix), Delimiter: aws.String(delimiter),
		ContinuationToken: optionalString(token), MaxKeys: aws.Int32(max),
	})
	if err != nil {
		return ListResult{}, err
	}
	result := ListResult{NextToken: aws.ToString(output.NextContinuationToken)}
	for _, object := range output.Contents {
		result.Objects = append(result.Objects, ObjectInfo{
			Key: aws.ToString(object.Key), Size: aws.ToInt64(object.Size), ETag: aws.ToString(object.ETag), LastModified: aws.ToTime(object.LastModified),
		})
	}
	for _, prefix := range output.CommonPrefixes {
		result.Prefixes = append(result.Prefixes, aws.ToString(prefix.Prefix))
	}
	return result, nil
}

// Head returns metadata for key.
func (p *s3Provider) Head(ctx context.Context, key string) (ObjectInfo, error) {
	output, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(key)})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(output.ContentLength), ETag: aws.ToString(output.ETag), LastModified: aws.ToTime(output.LastModified)}, nil
}

// Get opens all or a byte range of key.
func (p *s3Provider) Get(ctx context.Context, key, byteRange string) (io.ReadCloser, ObjectInfo, error) {
	output, err := p.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(key), Range: optionalString(byteRange)})
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	return output.Body, ObjectInfo{Key: key, Size: aws.ToInt64(output.ContentLength), ETag: aws.ToString(output.ETag), LastModified: aws.ToTime(output.LastModified), ContentRange: aws.ToString(output.ContentRange)}, nil
}

// Put writes an object subject to options.
func (p *s3Provider) Put(ctx context.Context, key string, body io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	input := &s3.PutObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(key), Body: body,
		ContentType: optionalString(options.ContentType), IfMatch: optionalString(options.IfMatch), IfNoneMatch: optionalString(options.IfNoneMatch),
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	output, err := p.client.PutObject(ctx, input, func(options *s3.Options) {
		// Do not buffer an unseekable stream merely to add optional S3 checksums or a SigV4 payload hash.
		options.Retryer = aws.NopRetryer{}
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.APIOptions = append(options.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	})
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: size, ETag: aws.ToString(output.ETag), LastModified: time.Now()}, nil
}

// Delete removes key.
func (p *s3Provider) Delete(ctx context.Context, key string) error {
	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(p.bucket), Key: aws.String(key)}, func(options *s3.Options) {
		options.Retryer = aws.NopRetryer{}
	})
	return err
}

// Copy copies an object within the bucket subject to options.
func (p *s3Provider) Copy(ctx context.Context, source, destination string, options PutOptions) (ObjectInfo, error) {
	copySource := url.PathEscape(p.bucket + "/" + source)
	output, err := p.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String(p.bucket), Key: aws.String(destination), CopySource: aws.String(copySource),
		IfMatch: optionalString(options.IfMatch), IfNoneMatch: optionalString(options.IfNoneMatch),
		CopySourceIfMatch: optionalString(options.CopySourceIfMatch), CopySourceIfNoneMatch: optionalString(options.CopySourceIfNoneMatch),
	}, func(options *s3.Options) { options.Retryer = aws.NopRetryer{} })
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: destination, ETag: aws.ToString(output.CopyObjectResult.ETag), LastModified: aws.ToTime(output.CopyObjectResult.LastModified)}, nil
}

// optionalString converts an empty value to nil for AWS request fields.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}
