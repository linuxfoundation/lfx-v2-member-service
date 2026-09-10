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
