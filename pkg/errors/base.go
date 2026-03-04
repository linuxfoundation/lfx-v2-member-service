// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import "fmt"

// base is a struct that holds the common fields for error types
type base struct {
	message string
	err     error
}

// error is a method that returns the error message for the base struct
func (b base) error() string {
	if b.err == nil {
		return b.message
	}
	return fmt.Sprintf("%s: %v", b.message, b.err)
}
