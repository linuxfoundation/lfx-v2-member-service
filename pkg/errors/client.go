// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import "errors"

// Validation represents a validation error in the application.
type Validation struct {
	base
}

// Error returns the error message for Validation.
func (v Validation) Error() string {
	return v.error()
}

// NewValidation creates a new Validation error with the provided message.
func NewValidation(message string, err ...error) Validation {
	return Validation{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// IsValidation reports whether err's outermost domain error is Validation.
func IsValidation(err error) bool {
	return isOutermost[Validation](err)
}

// NotFound represents a not found error in the application.
type NotFound struct {
	base
}

// Error returns the error message for NotFound.
func (v NotFound) Error() string {
	return v.error()
}

// NewNotFound creates a new NotFound error with the provided message.
func NewNotFound(message string, err ...error) NotFound {
	return NotFound{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// IsNotFound reports whether err's outermost domain error is NotFound.
func IsNotFound(err error) bool {
	return isOutermost[NotFound](err)
}

// Conflict represents a conflict error in the application.
type Conflict struct {
	base
}

// Error returns the error message for Conflict.
func (c Conflict) Error() string {
	return c.error()
}

// NewConflict creates a new Conflict error with the provided message.
func NewConflict(message string, err ...error) Conflict {
	return Conflict{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// IsConflict reports whether err's outermost domain error is Conflict.
func IsConflict(err error) bool {
	return isOutermost[Conflict](err)
}

// UnprocessableEntity represents a well-formed request for a resource that
// exists but cannot be used for the requested operation.
type UnprocessableEntity struct {
	base
}

// Error returns the error message for UnprocessableEntity.
func (u UnprocessableEntity) Error() string {
	return u.error()
}

// NewUnprocessableEntity creates a new UnprocessableEntity error with the provided message.
func NewUnprocessableEntity(message string, err ...error) UnprocessableEntity {
	return UnprocessableEntity{
		base: base{
			message: message,
			err:     errors.Join(err...),
		},
	}
}

// IsUnprocessableEntity reports whether err is an UnprocessableEntity error.
func IsUnprocessableEntity(err error) bool {
	if err == nil {
		return false
	}
	var unprocessable UnprocessableEntity
	return errors.As(err, &unprocessable)
}
