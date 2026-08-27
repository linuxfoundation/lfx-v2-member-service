// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
)

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

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// AccessLogMiddleware logs one structured record per completed HTTP
// transaction to stdout at info level, so Datadog gets access metrics without
// treating normal traffic as errors.
func AccessLogMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}

			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			ctx := r.Context()
			requestID := rec.Header().Get(string(constants.RequestIDHeader))
			if requestID == "" {
				requestID, _ = ctx.Value(constants.RequestIDHeader).(string)
			}

			slog.InfoContext(ctx, "http request completed",
				"verb", r.Method,
				"pattern", r.URL.Path,
				"status", status,
				"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
				"user_agent", r.UserAgent(),
				"bytes_written", rec.written,
				"request_id", requestID,
			)
		})
	}
}
