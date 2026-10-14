// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsUnprocessableEntity(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"direct", NewUnprocessableEntity("ineligible"), true},
		{"wrapped", fmt.Errorf("create: %w", NewUnprocessableEntity("ineligible")), true},
		{"other client error", NewNotFound("missing"), false},
		{"plain error", errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnprocessableEntity(tc.err); got != tc.want {
				t.Errorf("IsUnprocessableEntity(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
