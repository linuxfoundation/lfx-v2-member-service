// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

// promotionGenerationMetadataKey stores each CopyIfNewer promotion's
// caller-supplied generation as S3 object metadata (surfaced by AWS as the
// x-amz-meta-promoted-at header), so a later HeadObject can tell whether a
// newer promotion has already won without needing any state outside S3
// itself.
const promotionGenerationMetadataKey = "promoted-at"

const (
	ensureBucketAttempts = 10
	ensureBucketDelay    = 3 * time.Second
)

// copyIfNewerCASAttempts bounds CopyIfNewer's internal compare-and-swap retry
// loop (see the method doc comment) — a conflicting writer that turns out to
// be older than the caller is not fatal, just a reason to retry against the
// freshest ETag, but that must not spin forever.
const copyIfNewerCASAttempts = 5

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

// CopyIfNewer implements port.ObjectStoreWriter. It HeadObjects dstKey to
// decide which conditional CopyObject to issue: IfNoneMatch: "*" (create-only)
// if dstKey does not exist yet, or IfMatch pinned to the ETag just read if it
// does — either way S3 itself rejects the write with 412/409 if dstKey
// changed between the HeadObject and the CopyObject. That conflict only
// proves *someone* wrote first, not that they were newer: two attempts can
// both HeadObject the same starting ETag, and whichever's CopyObject reaches
// S3 first wins the write regardless of generation. So a conflict re-reads
// dstKey's freshest stamped generation and only surfaces
// port.ErrStalePromotion once that generation is at least as new as ours;
// otherwise the writer that beat us here was itself older, and this retries
// against the fresh ETag instead of wrongly abandoning a promotion it should
// still win (LFXV2-2016 lfx-reviewer finding on PR #87).
//
// An equal existing generation is treated as stale (>=, not >), even though
// generation is derived from Salesforce's millisecond-precision
// LastModifiedDate and two genuinely concurrent commits to the same org can
// land in the same millisecond and produce an identical value: there is no
// signal available here that can order such a tie correctly across
// replicas, so this does not try to. An earlier revision loosened this to a
// strict >, on the reasoning that an equal generation is an unbreakable tie
// and whichever CopyObject physically reaches S3 first should just win. That
// is wrong: it lets a delayed, older-in-real-time attempt that happens to
// share a generation with an already-promoted newer one overwrite it later,
// which is a live regression (a shown logo reverting to older bytes), not
// merely a missed optimization (copilot-pull-request-reviewer finding on PR
// #87, 2026-08-18). Treating equality as stale instead means the first
// attempt to actually promote for a given generation wins and every other
// same-generation attempt — regardless of which is chronologically newer in
// Salesforce terms — is safely dropped rather than risking an overwrite; the
// known cost is that a genuinely newer commit can lose that race and be left
// pointed at its own scratch object until its next upload, which is the
// safer failure mode of the two. The promotion directive replaces metadata
// rather than copying it, so dstKey's stored generation always reflects this
// attempt, not whatever srcKey happened to carry.
func (c *Client) CopyIfNewer(ctx context.Context, srcKey, dstKey string, generation int64) error {
	source := fmt.Sprintf("%s/%s", c.bucket, srcKey)

	// MetadataDirectiveReplace (needed below to stamp the generation) makes S3
	// discard every system/user metadata field not explicitly restated in this
	// request — it does not selectively merge with srcKey's own metadata. Read
	// srcKey's Content-Type and Cache-Control here so the promoted object keeps
	// them; without this the shared key would silently become
	// application/octet-stream with no cache policy (LFXV2-2016 lfx-reviewer
	// finding on PR #87).
	srcHead, srcHeadErr := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &srcKey})
	if srcHeadErr != nil {
		return fmt.Errorf("reading source object %s in bucket %s before promotion: %w", srcKey, c.bucket, srcHeadErr)
	}

	for attempt := 1; attempt <= copyIfNewerCASAttempts; attempt++ {
		head, headErr := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &dstKey})
		var ifMatch, ifNoneMatch *string
		var notFound *types.NotFound
		switch {
		case headErr == nil:
			if existing, ok := head.Metadata[promotionGenerationMetadataKey]; ok {
				if existingGen, parseErr := strconv.ParseInt(existing, 10, 64); parseErr == nil && existingGen >= generation {
					return port.ErrStalePromotion
				}
			}
			ifMatch = head.ETag
		case errors.As(headErr, &notFound):
			ifNoneMatch = aws.String("*")
		default:
			return fmt.Errorf("checking existing object %s in bucket %s before promotion: %w", dstKey, c.bucket, headErr)
		}

		_, copyErr := c.s3.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket:            &c.bucket,
			Key:               &dstKey,
			CopySource:        &source,
			IfMatch:           ifMatch,
			IfNoneMatch:       ifNoneMatch,
			ContentType:       srcHead.ContentType,
			CacheControl:      srcHead.CacheControl,
			Metadata:          map[string]string{promotionGenerationMetadataKey: strconv.FormatInt(generation, 10)},
			MetadataDirective: types.MetadataDirectiveReplace,
		})
		if copyErr == nil {
			return nil
		}
		if !isPreconditionConflict(copyErr) {
			return fmt.Errorf("copying object %s to %s in bucket %s: %w", srcKey, dstKey, c.bucket, copyErr)
		}
		// Conflict: loop back and re-HeadObject dstKey. If it's now stamped
		// with a generation >= ours, the top of the loop returns
		// ErrStalePromotion; otherwise we retry the copy against the fresh
		// ETag.
	}
	return fmt.Errorf("promoting object %s to %s in bucket %s: exhausted %d attempts against conflicting writers", srcKey, dstKey, c.bucket, copyIfNewerCASAttempts)
}

// isPreconditionConflict reports whether err is a conditional CopyObject
// rejection (412 Precondition Failed for IfMatch, 409 Conflict for
// IfNoneMatch) — either means a concurrent writer changed dstKey between the
// HeadObject and this Copy, distinct from a genuine transient/infra error.
func isPreconditionConflict(err error) bool {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case http.StatusPreconditionFailed, http.StatusConflict:
			return true
		}
	}
	return false
}

// nowUnixNano is a var so tests can override it deterministically. Nanosecond
// resolution (not Unix seconds) is required: two successful logo promotions
// for the same org within the same second would otherwise mint identical
// ?v= cache-busting tokens, letting a CDN edge that already cached the first
// response serve it back for the second upload's URL too (LFXV2-2016
// lfx-reviewer finding on PR #87).
var nowUnixNano = func() int64 { return time.Now().UnixNano() }
