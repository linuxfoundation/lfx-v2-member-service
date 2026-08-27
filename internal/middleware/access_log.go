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

// redactPath redacts email addresses embedded in path segments, because
// several routes carry a member email as a path parameter and the access log
// must not expose it.
//
// It takes the escaped path: chi routes on the escaped form, so an address
// containing an encoded slash (john%2Fdoe%40example.com) still reaches the
// {email} route. Splitting the decoded path would break that address across
// two segments and redact only the second half.
func redactPath(escapedPath string) string {
	if !strings.ContainsAny(escapedPath, "@%") {
		return escapedPath
	}

	segments := strings.Split(escapedPath, "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !strings.Contains(decoded, "@") {
			// Leave non-email segments exactly as received, so an encoded
			// value is never silently reshaped into extra path segments.
			continue
		}
		segments[i] = redaction.RedactEmail(decoded)
	}
	return strings.Join(segments, "/")
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
					"path", redactPath(r.URL.EscapedPath()),
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
