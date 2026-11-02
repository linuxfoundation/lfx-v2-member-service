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
//
// Every domain type, including pkgerrors.Unexpected, is resolved by walking
// the chain outermost first via pkgerrors.FindDomainError and type-switching
// on the match, so the OUTERMOST domain error present decides the mapping
// rather than whichever case happens to be listed first in the switch. A
// plain errors.As cascade here would get this backwards: errors.As matches
// the first occurrence of its target type anywhere in the chain, so if a
// domain error ever wraps a different domain error as its cause, listing
// order — not nesting order — would decide the outcome. This matters
// specifically for Unexpected now that pkgerrors.base implements Unwrap(): a
// bare errors.As(err, &unexpected) would match an Unexpected nested
// *underneath* another domain type (e.g. a hypothetical
// NewValidation("...", someUnexpected)), silently downgrading that type's
// intended status code to 500. Resolving Unexpected through the same
// outermost-first walk as every other type avoids that asymmetry.
// key_contact_writer.go's FGA-revoke-on-delete path wraps a ServiceUnavailable
// (from a failed FGA publish) inside a pkgerrors.NewUnexpected(...), with
// Unexpected as the OUTERMOST type, so it still correctly maps to 500 rather
// than inheriting the wrapped ServiceUnavailable's 503. Repository call sites
// routinely wrap domain errors with fmt.Errorf("...%w", err) (e.g.
// member_reader.go's SFID-resolution paths); that wrapper type matches no
// case here, so the walk unwraps through it to the pkgerrors type underneath,
// same as errors.As would.
//
// pkgerrors.FindDomainError is the same resolver backing pkgerrors.IsNotFound
// / IsConflict / IsValidation / IsPreconditionFailed, so this handler and
// those predicates agree on one outermost-first rule rather than diverging
// now that base.Unwrap() exists.
func wrapError(ctx context.Context, err error) error {
	if match, ok := pkgerrors.FindDomainError(err); ok {
		switch v := match.(type) {
		case pkgerrors.Unexpected:
			slog.ErrorContext(ctx, "request failed", "error", err)
			return membershipservice.MakeInternalServerError(errors.New("internal service error"))
		case pkgerrors.NotFound:
			// 404 is expected control flow, not a service failure.
			slog.InfoContext(ctx, "resource not found", "error", err)
			return membershipservice.MakeNotFound(v)
		case pkgerrors.Validation:
			// 400 is caller error, not a service failure.
			slog.InfoContext(ctx, "request validation failed", "error", err)
			return membershipservice.MakeBadRequest(v)
		case pkgerrors.Conflict:
			slog.WarnContext(ctx, "request rejected due to conflict", "error", err)
			return membershipservice.MakeConflict(v)
		case pkgerrors.UnprocessableEntity:
			// 422 is caller error, not a service failure.
			slog.InfoContext(ctx, "request rejected as unprocessable", "error", err)
			return membershipservice.MakeUnprocessableEntity(errors.New(v.Message()))
		case pkgerrors.ServiceUnavailable:
			slog.ErrorContext(ctx, "request failed", "error", err)
			return membershipservice.MakeServiceUnavailable(v)
		case pkgerrors.PreconditionFailed:
			slog.WarnContext(ctx, "precondition failed (stale If-Match)", "error", err)
			return membershipservice.MakePreconditionFailed(v)
		case pkgerrors.NotImplemented:
			// 501 stubs are intentional — log at debug, not error.
			slog.DebugContext(ctx, "stub endpoint called", "error", err)
			return membershipservice.MakeNotImplemented(v)
		}
	}

	slog.ErrorContext(ctx, "request failed", "error", err)
	return membershipservice.MakeInternalServerError(errors.New("internal service error"))
}
