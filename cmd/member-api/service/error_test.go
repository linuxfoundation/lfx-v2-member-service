// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
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

// TestWrapError exercises wrapError's chain-walking type switch: each domain
// type maps to its Goa error, Unexpected always wins regardless of nesting
// depth (key_contact_writer.go's FGA-revoke-on-delete path wraps a
// ServiceUnavailable inside an Unexpected, which must map to 500 not 503),
// and the OUTERMOST domain type wins when one domain error wraps another —
// a plain errors.As cascade would instead match whichever case is listed
// first in the switch, independent of nesting order.
func TestWrapError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantName string
	}{
		{
			name:     "Unexpected wrapping ServiceUnavailable maps to InternalServerError",
			err:      pkgerrors.NewUnexpected("failed to revoke FGA access for deleted key contact", pkgerrors.NewServiceUnavailable("failed to publish message")),
			wantName: "InternalServerError",
		},
		{
			name:     "outermost NotFound wrapping ServiceUnavailable wins over the inner type",
			err:      pkgerrors.NewNotFound("project not found", pkgerrors.NewServiceUnavailable("downstream fetch failed")),
			wantName: "NotFound",
		},
		{
			name:     "NotFound maps to NotFound",
			err:      pkgerrors.NewNotFound("project not found"),
			wantName: "NotFound",
		},
		{
			name:     "Validation maps to BadRequest",
			err:      pkgerrors.NewValidation("invalid tier_uid"),
			wantName: "BadRequest",
		},
		{
			name:     "Conflict maps to Conflict",
			err:      pkgerrors.NewConflict("stale If-Match"),
			wantName: "Conflict",
		},
		{
			name:     "ServiceUnavailable maps to ServiceUnavailable",
			err:      pkgerrors.NewServiceUnavailable("downstream fetch failed"),
			wantName: "ServiceUnavailable",
		},
		{
			name:     "PreconditionFailed maps to PreconditionFailed",
			err:      pkgerrors.NewPreconditionFailed("stale If-Match"),
			wantName: "PreconditionFailed",
		},
		{
			name:     "NotImplemented maps to NotImplemented",
			err:      pkgerrors.NewNotImplemented("stub endpoint"),
			wantName: "NotImplemented",
		},
		{
			name:     "unrecognized error maps to InternalServerError",
			err:      errors.New("boom"),
			wantName: "InternalServerError",
		},
		{
			// errors.Unwrap() (singular) cannot see into an errors.Join tree
			// (Unwrap() []error), and pkgerrors.base itself builds one for
			// every multi-cause constructor — findDomainError must recurse
			// into it explicitly rather than relying on a linear Unwrap walk.
			name:     "domain error nested under errors.Join is found",
			err:      fmt.Errorf("outer context: %w", errors.Join(errors.New("unrelated cause"), pkgerrors.NewConflict("stale If-Match"))),
			wantName: "Conflict",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapError(context.Background(), tt.err)

			svcErr, ok := got.(*goa.ServiceError)
			if !ok {
				t.Fatalf("wrapError returned %T, want *goa.ServiceError", got)
			}
			if svcErr.Name != tt.wantName {
				t.Errorf("wrapError(%v) = %q, want %q", tt.err, svcErr.Name, tt.wantName)
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
