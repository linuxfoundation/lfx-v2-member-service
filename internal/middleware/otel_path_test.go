// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactTelemetryPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    string
		changed bool
	}{
		{
			name:    "member-tiers username is redacted",
			path:    "/b2b_orgs/member-tiers/johndoe123",
			want:    "/b2b_orgs/member-tiers/joh****",
			changed: true,
		},
		{
			name:    "encoded slash stays one redacted segment",
			path:    "/b2b_orgs/member-tiers/john%2Fdoe",
			want:    "/b2b_orgs/member-tiers/joh****",
			changed: true,
		},
		{
			// A trailing slash 404s without matching the route, but the raw
			// value must still never reach the tracing backend.
			name:    "trailing slash variant is redacted",
			path:    "/b2b_orgs/member-tiers/johndoe123/",
			want:    "/b2b_orgs/member-tiers/joh****/",
			changed: true,
		},
		{
			name:    "extra segments are each redacted",
			path:    "/b2b_orgs/member-tiers/johndoe123/somethingelse",
			want:    "/b2b_orgs/member-tiers/joh****/som****",
			changed: true,
		},
		{
			name: "other routes pass through",
			path: "/b2b_orgs/some-uid",
			want: "/b2b_orgs/some-uid",
		},
		{
			name: "bare collection path passes through",
			path: "/b2b_orgs/member-tiers",
			want: "/b2b_orgs/member-tiers",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := redactTelemetryPath(tt.path)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.changed, changed)
		})
	}
}

// The redactor/restorer pair sandwiches otelhttp: whatever runs between them
// (otelhttp recording span attributes) must see the redacted target, while
// everything inside the restorer (muxer, handlers) must see the original.
func TestOTelPathRedactorRestorerSandwich(t *testing.T) {
	var betweenPath, betweenURI, innerPath, innerURI string

	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		innerPath = r.URL.EscapedPath()
		innerURI = r.RequestURI
	})
	between := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			betweenPath = r.URL.EscapedPath()
			betweenURI = r.RequestURI
			next.ServeHTTP(w, r)
		})
	}
	handler := OTelPathRedactor()(between(OTelPathRestorer()(inner)))

	req := httptest.NewRequest(http.MethodGet, "/b2b_orgs/member-tiers/johndoe123?x=1", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "/b2b_orgs/member-tiers/joh****", betweenPath,
		"the layer standing in for otelhttp must see the redacted path")
	assert.Equal(t, "/b2b_orgs/member-tiers/joh****?x=1", betweenURI)
	assert.Equal(t, "/b2b_orgs/member-tiers/johndoe123", innerPath,
		"handlers inside the restorer must see the original path")
	assert.Equal(t, "/b2b_orgs/member-tiers/johndoe123?x=1", innerURI)
}

func TestOTelPathRedactorPassThrough(t *testing.T) {
	var seen *http.Request
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r })
	handler := OTelPathRedactor()(OTelPathRestorer()(inner))

	req := httptest.NewRequest(http.MethodGet, "/b2b_orgs/some-uid", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	require.NotNil(t, seen)
	assert.Same(t, req, seen, "non-PII paths must pass through without cloning the request")
}
