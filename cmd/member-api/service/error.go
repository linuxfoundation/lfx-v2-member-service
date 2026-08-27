// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// wrapError maps a domain error to the appropriate Goa service error type.
// The generated Make* helpers return *goa.ServiceError, which has a correctly
// implemented Error() method and exposes Fault/Temporary flags to callers.
func wrapError(ctx context.Context, err error) error {
	var notFound pkgerrors.NotFound
	if errors.As(err, &notFound) {
		// 404 is expected control flow, not a service failure.
		slog.InfoContext(ctx, "resource not found", "error", err)
		return membershipservice.MakeNotFound(err)
	}

	var validation pkgerrors.Validation
	if errors.As(err, &validation) {
		// 400 is caller error, not a service failure.
		slog.InfoContext(ctx, "request validation failed", "error", err)
		return membershipservice.MakeBadRequest(err)
	}

	var conflict pkgerrors.Conflict
	if errors.As(err, &conflict) {
		slog.WarnContext(ctx, "request rejected due to conflict", "error", err)
		return membershipservice.MakeConflict(err)
	}

	var serviceUnavailable pkgerrors.ServiceUnavailable
	if errors.As(err, &serviceUnavailable) {
		slog.ErrorContext(ctx, "request failed", "error", err)
		return membershipservice.MakeServiceUnavailable(err)
	}

	var preconditionFailed pkgerrors.PreconditionFailed
	if errors.As(err, &preconditionFailed) {
		slog.WarnContext(ctx, "precondition failed (stale If-Match)", "error", err)
		return membershipservice.MakePreconditionFailed(err)
	}

	var notImplemented pkgerrors.NotImplemented
	if errors.As(err, &notImplemented) {
		// 501 stubs are intentional — log at debug, not error.
		slog.DebugContext(ctx, "stub endpoint called", "error", err)
		return membershipservice.MakeNotImplemented(err)
	}

	slog.ErrorContext(ctx, "request failed", "error", err)
	return membershipservice.MakeInternalServerError(errors.New("internal service error"))
}
