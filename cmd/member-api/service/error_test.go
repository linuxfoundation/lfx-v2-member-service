// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	goa "goa.design/goa/v3/pkg"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

type captureCtxKey struct{}

// levelCapture records the level of records logged with a context carrying
// captureCtxKey. The default logger is process-wide, and goroutines started by
// other tests in this package (admin reindex runs) keep logging into it.
type levelCapture struct {
	mu     sync.Mutex
	levels []slog.Level
}

func (c *levelCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *levelCapture) Handle(ctx context.Context, r slog.Record) error {
	if ctx.Value(captureCtxKey{}) == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.levels = append(c.levels, r.Level)
	return nil
}

func (c *levelCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *levelCapture) WithGroup(string) slog.Handler      { return c }

func captureLevel(t *testing.T, err error) (slog.Level, error) {
	t.Helper()

	capture := &levelCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mapped := wrapError(context.WithValue(context.Background(), captureCtxKey{}, true), err)

	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.levels) != 1 {
		t.Fatalf("expected exactly one log record from wrapError, got %d", len(capture.levels))
	}
	return capture.levels[0], mapped
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
