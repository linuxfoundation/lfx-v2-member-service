// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	goa "goa.design/goa/v3/pkg"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

func captureLevel(t *testing.T, err error) (slog.Level, error) {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mapped := wrapError(context.Background(), err)

	var record struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &record); err != nil {
		t.Fatalf("failed to decode log record %q: %v", buf.String(), err)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(record.Level)); err != nil {
		t.Fatalf("unexpected level %q: %v", record.Level, err)
	}
	return level, mapped
}

func TestWrapErrorLogLevels(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantLevel slog.Level
	}{
		{"not found is control flow", pkgerrors.NewNotFound("missing"), slog.LevelInfo},
		{"validation is caller error", pkgerrors.NewValidation("bad input"), slog.LevelInfo},
		{"conflict warns", pkgerrors.NewConflict("conflict"), slog.LevelWarn},
		{"unprocessable is caller error", pkgerrors.NewUnprocessableEntity("ineligible"), slog.LevelInfo},
		{"precondition failed warns", pkgerrors.NewPreconditionFailed("stale"), slog.LevelWarn},
		{"not implemented is debug", pkgerrors.NewNotImplemented("stub"), slog.LevelDebug},
		{"service unavailable errors", pkgerrors.NewServiceUnavailable("down"), slog.LevelError},
		{"unknown error errors", errors.New("boom"), slog.LevelError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			level, mapped := captureLevel(t, tc.err)
			if level != tc.wantLevel {
				t.Errorf("got level %v, want %v", level, tc.wantLevel)
			}
			if mapped == nil {
				t.Error("expected wrapError to return a non-nil error")
			}
		})
	}
}

func TestWrapErrorUnprocessableEntity(t *testing.T) {
	cause := errors.New("sfid: 001000000000001AAA")
	mapped := wrapError(context.Background(), pkgerrors.NewUnprocessableEntity("account is not eligible", cause))

	var serviceErr *goa.ServiceError
	if !errors.As(mapped, &serviceErr) {
		t.Fatalf("expected *goa.ServiceError, got %T", mapped)
	}
	if serviceErr.Name != "UnprocessableEntity" {
		t.Errorf("got name %q, want UnprocessableEntity", serviceErr.Name)
	}
	if serviceErr.Message != "account is not eligible" {
		t.Errorf("got message %q; the wrapped cause must stay out of the response", serviceErr.Message)
	}
}
