// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// healthPaths are polled by Kubernetes probes several times a minute, so
// successful probes are downgraded to debug to keep them out of the access
// log. Failed probes stay at info.
var healthPaths = map[string]bool{
	"/livez":  true,
	"/readyz": true,
}

// statusRecorder captures the response status code and byte count written by
// downstream handlers so the access log can report them.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer for
// capabilities this recorder does not implement, such as Hijacker.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// piiPathParams names route path parameters whose values are PII and must be
// redacted from the access-logged path, matched by name in the route template
// (e.g. "{username}") since a username has no detectable shape like an email.
var piiPathParams = map[string]bool{
	"username": true,
}

// redactPath strips sensitive values from the access-logged path. It removes
// two classes: the value of any path parameter named in piiPathParams (matched
// by position against the matched route template), and email addresses embedded
// in any segment by content (several routes carry a member email as a path
// parameter).
//
// It works on the escaped path: chi routes on the escaped form, so a value
// containing an encoded slash (john%2Fdoe) still reaches its route as one
// segment. Redacting the whole escaped segment, rather than splitting the
// decoded path, keeps such a value from being reshaped into extra segments and
// only partly redacted.
func redactPath(routeTemplate, escapedPath string) string {
	segments := strings.Split(escapedPath, "/")
	redacted := false

	// Redact named PII parameters by position. Goa path parameters never span a
	// slash, so a template like "/b2b_orgs/member-tiers/{username}" aligns
	// segment-for-segment with the concrete path.
	if templateSegs := strings.Split(routeTemplate, "/"); len(templateSegs) == len(segments) {
		for i, ts := range templateSegs {
			if name, ok := pathParamName(ts); ok && piiPathParams[name] {
				segments[i] = redaction.Redact(segments[i])
				redacted = true
			}
		}
	}

	// Redact email addresses in any remaining segment by content.
	if strings.ContainsAny(escapedPath, "@%") {
		for i, segment := range segments {
			decoded, err := url.PathUnescape(segment)
			if err != nil || !strings.Contains(decoded, "@") {
				// Leave non-email segments exactly as received, so an encoded
				// value is never silently reshaped into extra path segments.
				continue
			}
			segments[i] = redaction.RedactEmail(decoded)
			redacted = true
		}
	}

	if !redacted {
		return escapedPath
	}
	return strings.Join(segments, "/")
}

// pathParamName returns the parameter name of a route-template segment written
// as "{name}", and whether the segment is such a parameter.
func pathParamName(segment string) (string, bool) {
	if len(segment) >= 2 && segment[0] == '{' && segment[len(segment)-1] == '}' {
		return segment[1 : len(segment)-1], true
	}
	return "", false
}

// unmatchedRoute is reported instead of the concrete path when no route
// matched, so scanning for arbitrary URLs cannot inflate route cardinality.
const unmatchedRoute = "<unmatched>"

// routePattern returns the matched route template, e.g. "/b2b_orgs/{uid}".
// The Goa muxer sets r.Pattern to "METHOD /template" before dispatching, but
// only for requests it routed.
func routePattern(r *http.Request) string {
	if r.Pattern == "" {
		return unmatchedRoute
	}
	if _, template, found := strings.Cut(r.Pattern, " "); found {
		return template
	}
	return r.Pattern
}

// AccessLogMiddleware logs one structured record per completed HTTP
// transaction to stdout at info level, so Datadog gets access metrics without
// treating normal traffic as errors.
//
// Register it on the Goa muxer with Use rather than wrapping the muxer, so
// r.Pattern is populated by the time it runs.
func AccessLogMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			completed := false

			// Deferred so a panicking handler still produces an access record.
			defer func() {
				status := rec.status
				panicked := !completed
				switch {
				case panicked:
					// net/http abandons the response, so whatever status was
					// already written never completed.
					status = http.StatusInternalServerError
				case status == 0:
					status = http.StatusOK
				}

				level := slog.LevelInfo
				switch {
				case panicked:
					level = slog.LevelError
				case healthPaths[r.URL.Path] && status < http.StatusBadRequest:
					// Only successful probes are noise. A failing probe is a
					// readiness incident and must stay visible at info.
					level = slog.LevelDebug
				}

				attrs := []any{
					"verb", r.Method,
					"pattern", routePattern(r),
					"path", redactPath(routePattern(r), r.URL.EscapedPath()),
					"status", status,
					"duration_ms", float64(time.Since(start).Microseconds()) / 1000.0,
					"user_agent", r.UserAgent(),
					"bytes_written", rec.written,
					"request_id", rec.Header().Get(string(constants.RequestIDHeader)),
				}
				if panicked {
					attrs = append(attrs, "panic", true)
				}

				slog.Log(r.Context(), level, "http request completed", attrs...)
			}()

			next.ServeHTTP(rec, r)
			completed = true
		})
	}
}
