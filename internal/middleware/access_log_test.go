// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	goahttp "goa.design/goa/v3/http"
)

func decodeAccessLog(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("failed to decode log record %q: %v", buf.String(), err)
	}
	return record
}

func runAccessLog(t *testing.T, req *http.Request, next http.Handler) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rr := httptest.NewRecorder()
	AccessLogMiddleware()(next).ServeHTTP(rr, req)

	return decodeAccessLog(t, &buf), rr
}

func TestAccessLogMiddlewareLogsCompletedRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/b2b_orgs/123", nil)
	req.Pattern = "GET /b2b_orgs/{uid}"
	req.Header.Set("User-Agent", "test-agent/1.0")

	record, _ := runAccessLog(t, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))

	if record["level"] != "INFO" {
		t.Errorf("got level %v, want INFO", record["level"])
	}
	if record["verb"] != http.MethodGet {
		t.Errorf("got verb %v, want %v", record["verb"], http.MethodGet)
	}
	if record["pattern"] != "/b2b_orgs/{uid}" {
		t.Errorf("got pattern %v, want /b2b_orgs/{uid}", record["pattern"])
	}
	if record["path"] != "/b2b_orgs/123" {
		t.Errorf("got path %v, want /b2b_orgs/123", record["path"])
	}
	if status, ok := record["status"].(float64); !ok || int(status) != http.StatusCreated {
		t.Errorf("got status %v, want %d", record["status"], http.StatusCreated)
	}
	if record["user_agent"] != "test-agent/1.0" {
		t.Errorf("got user_agent %v, want test-agent/1.0", record["user_agent"])
	}
	if _, ok := record["duration_ms"].(float64); !ok {
		t.Errorf("expected numeric duration_ms, got %v", record["duration_ms"])
	}
	if bytesWritten, ok := record["bytes_written"].(float64); !ok || int(bytesWritten) != 2 {
		t.Errorf("got bytes_written %v, want 2", record["bytes_written"])
	}
}

// Registering on a real Goa muxer is what makes r.Pattern available; wrapping
// the muxer from outside silently yields "<unmatched>" for every request.
func TestAccessLogMiddlewareResolvesPatternFromGoaMuxer(t *testing.T) {
	mux := goahttp.NewMuxer()
	mux.Use(AccessLogMiddleware())
	mux.Handle(http.MethodDelete, "/b2b_orgs/{uid}/settings/users/{email}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := httptest.NewRequest(http.MethodDelete, "/b2b_orgs/123/settings/users/johndoe@example.com", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeAccessLog(t, &buf)
	if want := "/b2b_orgs/{uid}/settings/users/{email}"; record["pattern"] != want {
		t.Errorf("got pattern %v, want %q", record["pattern"], want)
	}
	if want := "/b2b_orgs/123/settings/users/joh****@example.com"; record["path"] != want {
		t.Errorf("got path %v, want %q", record["path"], want)
	}
	if status, ok := record["status"].(float64); !ok || int(status) != http.StatusNoContent {
		t.Errorf("got status %v, want %d", record["status"], http.StatusNoContent)
	}
}

// An unrouted request must not put its concrete URL in pattern, otherwise
// scanning for arbitrary paths inflates route cardinality.
func TestAccessLogMiddlewareReportsUnmatchedRoute(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/definitely/not/a/route", nil)

	record, _ := runAccessLog(t, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	if record["pattern"] != unmatchedRoute {
		t.Errorf("got pattern %v, want %q", record["pattern"], unmatchedRoute)
	}
	if record["path"] != "/definitely/not/a/route" {
		t.Errorf("got path %v, want the concrete path", record["path"])
	}
}

func TestAccessLogMiddlewareDefaultsStatusToOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/livez", nil)

	record, _ := runAccessLog(t, req, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

	if status, ok := record["status"].(float64); !ok || int(status) != http.StatusOK {
		t.Errorf("got status %v, want %d", record["status"], http.StatusOK)
	}
}

func TestAccessLogMiddlewareIncludesRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/b2b_orgs/123", nil)
	req.Header.Set("X-REQUEST-ID", "req-abc")

	// Chained the same way as the server: request ID is assigned inside.
	handler := RequestIDMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	record, rr := runAccessLog(t, req, handler)

	if record["request_id"] != "req-abc" {
		t.Errorf("got request_id %v, want req-abc", record["request_id"])
	}
	if got := rr.Header().Get("X-REQUEST-ID"); got != "req-abc" {
		t.Errorf("got response request id header %q, want req-abc", got)
	}
}

func TestAccessLogMiddlewareRedactsEmailInPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/b2b_orgs/123/settings/users/johndoe@example.com", nil)

	record, _ := runAccessLog(t, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	pattern, _ := record["path"].(string)
	if strings.Contains(pattern, "johndoe") {
		t.Errorf("path %q leaks the email local part", pattern)
	}
	if want := "/b2b_orgs/123/settings/users/joh****@example.com"; pattern != want {
		t.Errorf("got path %q, want %q", pattern, want)
	}
}

// chi routes on the escaped path, so an address whose local part contains an
// encoded slash still reaches the {email} route. It must be redacted as one
// value rather than split across segments.
func TestAccessLogMiddlewareRedactsEncodedSlashEmail(t *testing.T) {
	mux := goahttp.NewMuxer()
	mux.Use(AccessLogMiddleware())
	mux.Handle(http.MethodDelete, "/b2b_orgs/{uid}/settings/users/{email}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := httptest.NewRequest(http.MethodDelete, "/b2b_orgs/123/settings/users/john%2Fdoe%40example.com", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeAccessLog(t, &buf)
	path, _ := record["path"].(string)
	for _, leak := range []string{"john/doe", "john%2Fdoe", "johndoe"} {
		if strings.Contains(path, leak) {
			t.Errorf("path %q leaks %q", path, leak)
		}
	}
	if want := "/b2b_orgs/123/settings/users/joh****@example.com"; path != want {
		t.Errorf("got path %q, want %q", path, want)
	}
}

func TestAccessLogMiddlewareLogsHealthProbesAtDebug(t *testing.T) {
	for _, path := range []string{"/livez", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)

			record, _ := runAccessLog(t, req, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))

			if record["level"] != "DEBUG" {
				t.Errorf("got level %v, want DEBUG", record["level"])
			}
		})
	}
}

// A failing probe is a readiness incident, so it must survive the default
// LOG_LEVEL=info rather than being discarded with the healthy probe noise.
func TestAccessLogMiddlewareKeepsFailedHealthProbesAtInfo(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

			record, _ := runAccessLog(t, req, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))

			if record["level"] != "INFO" {
				t.Errorf("got level %v, want INFO", record["level"])
			}
			if got, ok := record["status"].(float64); !ok || int(got) != status {
				t.Errorf("got status %v, want %d", record["status"], status)
			}
		})
	}
}

func TestAccessLogMiddlewareLogsPanicAsServerError(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "panic before writing",
			handler: func(_ http.ResponseWriter, _ *http.Request) {
				panic("boom")
			},
		},
		{
			// The status was already written, but net/http abandons the
			// response, so recording 200 would report a false success.
			name: "panic after writing a success status",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("partial"))
				panic("boom")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/b2b_orgs/123", nil)

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			handler := AccessLogMiddleware()(tc.handler)

			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected the panic to propagate to the server")
					}
				}()
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}()

			record := decodeAccessLog(t, &buf)
			if status, ok := record["status"].(float64); !ok || int(status) != http.StatusInternalServerError {
				t.Errorf("got status %v, want %d", record["status"], http.StatusInternalServerError)
			}
			if record["panic"] != true {
				t.Errorf("got panic %v, want true", record["panic"])
			}
			if record["level"] != "ERROR" {
				t.Errorf("got level %v, want ERROR", record["level"])
			}
		})
	}
}
