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

// IsValidation reports whether err is a Validation error.
func IsValidation(err error) bool {
	if err == nil {
		return false
	}
	var v Validation
	return errors.As(err, &v)
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

// IsNotFound reports whether err is a NotFound error.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var notFound NotFound
	return errors.As(err, &notFound)
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

// IsConflict reports whether err is a Conflict error.
func IsConflict(err error) bool {
	if err == nil {
		return false
	}
	var conflict Conflict
	return errors.As(err, &conflict)
}
