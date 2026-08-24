// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package objectstore provides an S3-backed port.ObjectStoreWriter used to
// upload B2B org logos to a CDN-fronted public bucket (LFXV2-2016).
package objectstore

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config holds the S3 bucket/region and CDN settings required to upload and
// publicly serve B2B org logos.
type Config struct {
	// Bucket is the S3 bucket name. Required, no code fallback — per the
	// object-storage skill (PR #67), a missing bucket must fail startup with a
	// clear config error rather than a runtime AWS API error.
	Bucket string

	// Region is the AWS region the bucket lives in. Required, no code
	// fallback. Every provisioned bucket is us-west-2; a wrong-region default
	// would produce an opaque PermanentRedirect from S3 instead of a clear
	// config error.
	Region string

	// CDNURLPrefix is the absolute (scheme-included) base URL of the
	// CloudFront distribution fronting Bucket, e.g.
	// "https://org-logos-public.dev.downloads.lfx.community". Required and
	// validated as absolute — a bare hostname would silently produce a
	// relative URL when embedded in a response body.
	CDNURLPrefix string

	// EndpointURL optionally overrides the S3 endpoint (e.g. a local
	// nats-s3 sidecar for dev). Presence is not used to infer "local" —
	// it is passed straight through to the SDK's BaseEndpoint option.
	EndpointURL string
}

// ConfigFromEnv builds a Config from environment variables.
//
// Required:
//
//	S3_BUCKET       — bucket name for B2B org logo objects.
//	AWS_REGION      — AWS region the bucket lives in (e.g. "us-west-2").
//	CDN_URL_PREFIX  — absolute CDN base URL fronting the bucket.
//
// Optional:
//
//	S3_ENDPOINT_URL — overrides the S3 endpoint (local dev sidecar).
func ConfigFromEnv() (Config, error) {
	bucket := os.Getenv("S3_BUCKET")
	if bucket == "" {
		return Config{}, fmt.Errorf("S3_BUCKET environment variable is required")
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		return Config{}, fmt.Errorf("AWS_REGION environment variable is required")
	}

	cdnPrefix := os.Getenv("CDN_URL_PREFIX")
	if cdnPrefix == "" {
		return Config{}, fmt.Errorf("CDN_URL_PREFIX environment variable is required")
	}
	parsed, err := url.Parse(cdnPrefix)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Config{}, fmt.Errorf("CDN_URL_PREFIX must be an absolute URL (got %q)", cdnPrefix)
	}

	return Config{
		Bucket:       bucket,
		Region:       region,
		CDNURLPrefix: strings.TrimRight(cdnPrefix, "/"),
		EndpointURL:  os.Getenv("S3_ENDPOINT_URL"),
	}, nil
}
