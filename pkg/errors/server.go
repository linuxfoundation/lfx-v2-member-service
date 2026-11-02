// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import "errors"

// Unexpected represents an unexpected error in the application.
type Unexpected struct {
	base
}

// Error returns the error message for Unexpected.
func (u Unexpected) Error() string {
	return u.error()
}

// NewUnexpected creates a new Unexpected error with the provided message.
func NewUnexpected(message string, err ...error) Unexpected {
	return Unexpected{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// ServiceUnavailable represents a service unavailability error in the application.
type ServiceUnavailable struct {
	base
}

// Error returns the error message for ServiceUnavailable.
func (su ServiceUnavailable) Error() string {
	return su.error()
}

// NewServiceUnavailable creates a new ServiceUnavailable error with the provided message.
func NewServiceUnavailable(message string, err ...error) ServiceUnavailable {
	return ServiceUnavailable{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// NotImplemented represents a not implemented error in the application.
type NotImplemented struct {
	base
}

// Error returns the error message for NotImplemented.
func (ni NotImplemented) Error() string {
	return ni.error()
}

// NewNotImplemented creates a new NotImplemented error with the provided message.
func NewNotImplemented(message string, err ...error) NotImplemented {
	return NotImplemented{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// PreconditionFailed represents a conditional-write failure (HTTP 412). It is
// returned when an If-Match or If-Unmodified-Since check fails — i.e. the
// caller's copy of the resource is stale.
type PreconditionFailed struct {
	base
}

// Error returns the error message for PreconditionFailed.
func (p PreconditionFailed) Error() string {
	return p.error()
}

// NewPreconditionFailed creates a new PreconditionFailed error with the provided message.
func NewPreconditionFailed(message string, err ...error) PreconditionFailed {
	return PreconditionFailed{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// IsPreconditionFailed reports whether err's outermost domain error is PreconditionFailed.
func IsPreconditionFailed(err error) bool {
	return isOutermost[PreconditionFailed](err)
}
