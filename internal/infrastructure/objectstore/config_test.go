// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package objectstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigFromEnv_Happy(t *testing.T) {
	t.Setenv("S3_BUCKET", "lfx-v2-org-logos-public-dev")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("CDN_URL_PREFIX", "https://org-logos-public.dev.downloads.lfx.community/")
	t.Setenv("S3_ENDPOINT_URL", "")

	cfg, err := ConfigFromEnv()

	require.NoError(t, err)
	assert.Equal(t, "lfx-v2-org-logos-public-dev", cfg.Bucket)
	assert.Equal(t, "us-west-2", cfg.Region)
	assert.Equal(t, "https://org-logos-public.dev.downloads.lfx.community", cfg.CDNURLPrefix, "trailing slash must be trimmed")
	assert.Empty(t, cfg.EndpointURL)
}

func TestConfigFromEnv_MissingBucket(t *testing.T) {
	t.Setenv("S3_BUCKET", "")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("CDN_URL_PREFIX", "https://cdn.example.com")

	_, err := ConfigFromEnv()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "S3_BUCKET")
}

func TestConfigFromEnv_MissingRegion(t *testing.T) {
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("AWS_REGION", "")
	t.Setenv("CDN_URL_PREFIX", "https://cdn.example.com")

	_, err := ConfigFromEnv()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "AWS_REGION")
}

func TestConfigFromEnv_MissingCDNPrefix(t *testing.T) {
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("CDN_URL_PREFIX", "")

	_, err := ConfigFromEnv()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CDN_URL_PREFIX")
}

func TestConfigFromEnv_BareHostnameCDNPrefixRejected(t *testing.T) {
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("CDN_URL_PREFIX", "org-logos-public.dev.downloads.lfx.community")

	_, err := ConfigFromEnv()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "absolute URL")
}

func TestConfigFromEnv_EndpointURLOptional(t *testing.T) {
	t.Setenv("S3_BUCKET", "bucket")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("CDN_URL_PREFIX", "https://cdn.example.com")
	t.Setenv("S3_ENDPOINT_URL", "http://localhost:4566")

	cfg, err := ConfigFromEnv()

	require.NoError(t, err)
	assert.Equal(t, "http://localhost:4566", cfg.EndpointURL)
}
