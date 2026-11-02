// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import "fmt"

// base is a struct that holds the common fields for error types
type base struct {
	message string
	err     error
}

// Message returns the curated message without any wrapped cause, so callers
// can surface stable text to clients while the full cause stays in server logs.
func (b base) Message() string {
	return b.message
}

// error is a method that returns the error message for the base struct
func (b base) error() string {
	if b.err == nil {
		return b.message
	}
	return fmt.Sprintf("%s: %v", b.message, b.err)
}

// Unwrap exposes the wrapped cause so errors.Is/errors.As can see past a
// domain error type to a sentinel or typed error it was constructed from
// (e.g. errors.Is(err, context.Canceled) after wrapping context.Canceled
// through NewServiceUnavailable).
func (b base) Unwrap() error {
	return b.err
}

// FindDomainError walks err's chain outermost-first looking for one of the
// package's own domain types, returning the first match found.
//
// A plain errors.As(err, &target) matches the first occurrence of target's
// type anywhere in the chain, which gets nesting backwards now that base
// implements Unwrap(): if one domain error wraps a different domain error as
// its cause (e.g. key_contact_writer.go's
// NewUnexpected("...", someServiceUnavailable)), errors.As would find the
// innermost ServiceUnavailable before ever considering the outermost
// Unexpected, silently reclassifying the error's type. Every Is* predicate in
// this package, and cmd/member-api/service/error.go's wrapError, resolve
// through this same outermost-first walk so they all agree on one rule.
//
// Plain errors.Unwrap() only follows a single-cause Unwrap() error link, so
// it cannot see into an errors.Join tree (Unwrap() []error) — and base's own
// Unwrap() calls errors.Join internally for every multi-cause constructor.
// FindDomainError handles both shapes explicitly, recursing depth-first into
// every branch of a joined tree (in order) so a domain error nested under
// errors.Join is still found.
func FindDomainError(err error) (error, bool) {
	switch err.(type) {
	case Unexpected, NotFound, Validation, Conflict, ServiceUnavailable, PreconditionFailed, NotImplemented, UnprocessableEntity:
		return err, true
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, sub := range joined.Unwrap() {
			if match, found := FindDomainError(sub); found {
				return match, true
			}
		}
		return nil, false
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if next := wrapped.Unwrap(); next != nil {
			return FindDomainError(next)
		}
	}

	return nil, false
}

// isOutermost reports whether err's outermost domain error (per
// FindDomainError) is of type T.
func isOutermost[T error](err error) bool {
	if err == nil {
		return false
	}
	match, ok := FindDomainError(err)
	if !ok {
		return false
	}
	_, is := match.(T)
	return is
}
