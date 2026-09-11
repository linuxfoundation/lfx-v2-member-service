// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// otelPIIPathPrefix is the escaped-path prefix of the one route that carries a
// username (PII) as a path parameter: GET /b2b_orgs/member-tiers/{username}.
// Matching by shape, not by matched route, because the redaction has to happen
// before the muxer runs (otelhttp records the path when the server span
// starts); redacting everything after the prefix also covers trailing-slash
// and encoded-slash variants that would 404 without ever matching the route.
const otelPIIPathPrefix = "/b2b_orgs/member-tiers/"

// otelOriginalURL carries the pre-redaction request target through the context
// so OTelPathRestorer can put it back after otelhttp has recorded its span
// attributes.
type otelOriginalURL struct {
	url        *url.URL
	requestURI string
}

type otelOriginalURLKey struct{}

// redactTelemetryPath returns the escaped path with every segment after
// otelPIIPathPrefix redacted, and whether anything changed.
func redactTelemetryPath(escapedPath string) (string, bool) {
	if !strings.HasPrefix(escapedPath, otelPIIPathPrefix) {
		return escapedPath, false
	}
	rest := escapedPath[len(otelPIIPathPrefix):]
	segments := strings.Split(rest, "/")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		segments[i] = redaction.Redact(segment)
	}
	return otelPIIPathPrefix + strings.Join(segments, "/"), true
}

// OTelPathRedactor swaps the request URL and RequestURI for a PII-redacted
// copy on username-bearing routes before otelhttp starts the server span, so
// the url.path (and legacy http.target) span attributes never carry a raw
// LFID. It must wrap otelhttp.NewHandler from the outside, paired with
// OTelPathRestorer on the inside; requests on other paths pass through
// untouched.
func OTelPathRedactor() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			redacted, changed := redactTelemetryPath(r.URL.EscapedPath())
			if !changed {
				next.ServeHTTP(w, r)
				return
			}

			orig := &otelOriginalURL{url: r.URL, requestURI: r.RequestURI}
			r2 := r.Clone(context.WithValue(r.Context(), otelOriginalURLKey{}, orig))
			redactedURL := *r.URL
			// Set RawPath too: the redacted form decodes to itself, and
			// without it EscapedPath would re-escape the asterisks as %2A.
			redactedURL.Path = redacted
			redactedURL.RawPath = redacted
			r2.URL = &redactedURL
			r2.RequestURI = redactedURL.RequestURI()

			next.ServeHTTP(w, r2)
		})
	}
}

// OTelPathRestorer restores the original request URL and RequestURI stashed by
// OTelPathRedactor, so the muxer, handlers, and the access log (which applies
// its own redaction) all see the path the client actually sent. It must sit
// between otelhttp.NewHandler and the muxer.
func OTelPathRestorer() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if orig, ok := r.Context().Value(otelOriginalURLKey{}).(*otelOriginalURL); ok {
				r.URL = orig.url
				r.RequestURI = orig.requestURI
			}
			next.ServeHTTP(w, r)
		})
	}
}
