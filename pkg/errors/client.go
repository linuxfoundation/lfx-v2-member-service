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
