// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

const (
	ensureBucketAttempts = 10
	ensureBucketDelay    = 3 * time.Second
)

// Client is an S3-backed port.ObjectStoreWriter.
type Client struct {
	s3     *s3.Client
	bucket string
	cdn    string
}

// Ensure Client satisfies the port at compile time.
var _ port.ObjectStoreWriter = (*Client)(nil)

// NewClient builds a Client from cfg using the default AWS credential chain
// (no branching on EndpointURL presence — it is always passed through,
// nil/empty is a no-op for the SDK). Path-style addressing is forced so a
// local S3-compatible endpoint (e.g. a dev sidecar) works without DNS-based
// virtual-hosted routing.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
		if cfg.EndpointURL != "" {
			o.BaseEndpoint = &cfg.EndpointURL
		}
	})

	return &Client{s3: s3Client, bucket: cfg.Bucket, cdn: cfg.CDNURLPrefix}, nil
}

// EnsureBucket confirms the bucket is reachable before the service accepts
// traffic, retrying up to ensureBucketAttempts times, ensureBucketDelay apart.
// It never creates the bucket — provisioning is owned by infra (Antonia),
// per the Decided Architecture in ORG-LOGO-UPLOAD-PLAN-LFXV2-2016.md.
func (c *Client) EnsureBucket(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= ensureBucketAttempts; attempt++ {
		if err := c.headBucket(ctx); err != nil {
			lastErr = err
			slog.WarnContext(ctx, "logo bucket not yet reachable, retrying",
				"bucket", c.bucket, "attempt", attempt, "max_attempts", ensureBucketAttempts, "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ensureBucketDelay):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("logo bucket %s not reachable after %d attempts: %w", c.bucket, ensureBucketAttempts, lastErr)
}

// Readyz pings the bucket for use by the /readyz handler. Unlike EnsureBucket,
// it does not retry — a single failed HeadBucket is a real signal that the
// service should not be marked ready.
func (c *Client) Readyz(ctx context.Context) error {
	return c.headBucket(ctx)
}

func (c *Client) headBucket(ctx context.Context) error {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.bucket})
	if err != nil {
		var respErr *smithyhttp.ResponseError
		if errors.As(err, &respErr) {
			return fmt.Errorf("head bucket %s: HTTP %d: %w", c.bucket, respErr.HTTPStatusCode(), err)
		}
		return fmt.Errorf("head bucket %s: %w", c.bucket, err)
	}
	return nil
}

// Put uploads data to key with the given content type, sets the mandatory
// short-TTL Cache-Control, and returns a versioned, absolute CDN URL for the
// object. The "?v=" cache-busting hint is a Unix timestamp, not an S3
// VersionId — both the object-storage skill and this endpoint's design treat
// VersionId as unsuitable for public cache-busting.
func (c *Client) Put(ctx context.Context, key string, contentType string, data []byte) (string, error) {
	cacheControl := constants.LogoCacheControl
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       &c.bucket,
		Key:          &key,
		Body:         bytes.NewReader(data),
		ContentType:  &contentType,
		CacheControl: &cacheControl,
	})
	if err != nil {
		return "", fmt.Errorf("uploading object %s to bucket %s: %w", key, c.bucket, err)
	}

	return c.VersionedURL(key), nil
}

// VersionedURL implements port.ObjectStoreWriter.
func (c *Client) VersionedURL(key string) string {
	return fmt.Sprintf("%s/%s?v=%d", c.cdn, key, nowUnixNano())
}

// Delete implements port.ObjectStoreWriter.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	if err != nil {
		return fmt.Errorf("deleting object %s from bucket %s: %w", key, c.bucket, err)
	}
	return nil
}

// Copy implements port.ObjectStoreWriter. It relies on S3's default COPY
// metadata directive, so dstKey ends up with the same Content-Type and
// Cache-Control srcKey was uploaded with.
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string) error {
	source := fmt.Sprintf("%s/%s", c.bucket, srcKey)
	_, err := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     &c.bucket,
		Key:        &dstKey,
		CopySource: &source,
	})
	if err != nil {
		return fmt.Errorf("copying object %s to %s in bucket %s: %w", srcKey, dstKey, c.bucket, err)
	}
	return nil
}

// nowUnixNano is a var so tests can override it deterministically. Nanosecond
// resolution (not Unix seconds) is required: two successful logo promotions
// for the same org within the same second would otherwise mint identical
// ?v= cache-busting tokens, letting a CDN edge that already cached the first
// response serve it back for the second upload's URL too (LFXV2-2016
// lfx-reviewer finding on PR #87).
var nowUnixNano = func() int64 { return time.Now().UnixNano() }
