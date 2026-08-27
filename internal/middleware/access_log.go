// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// healthPaths are polled by Kubernetes probes several times a minute; logging
// them at info level would drown the access log.
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
func redactPath(path string) string {
	if !strings.Contains(path, "@") {
		return path
	}

	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.Contains(segment, "@") {
			segments[i] = redaction.RedactEmail(segment)
		}
	}
	return strings.Join(segments, "/")
}

// AccessLogMiddleware logs one structured record per completed HTTP
// transaction to stdout at info level, so Datadog gets access metrics without
// treating normal traffic as errors.
func AccessLogMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			completed := false

			// Deferred so a panicking handler still produces an access record.
			defer func() {
				status := rec.status
				switch {
				case status != 0:
				case completed:
					status = http.StatusOK
				default:
					// The handler panicked; net/http drops the connection
					// without ever writing a status.
					status = http.StatusInternalServerError
				}

				level := slog.LevelInfo
				if healthPaths[r.URL.Path] {
					level = slog.LevelDebug
				}

				slog.Log(r.Context(), level, "http request completed",
					"verb", r.Method,
					"pattern", redactPath(r.URL.Path),
					"status", status,
					"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
					"user_agent", r.UserAgent(),
					"bytes_written", rec.written,
					"request_id", rec.Header().Get(string(constants.RequestIDHeader)),
				)
			}()

			next.ServeHTTP(rec, r)
			completed = true
		})
	}
}
