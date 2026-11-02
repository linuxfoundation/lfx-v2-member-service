// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package errors

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestIsNotFound also exercises the shared outermost-first resolution rule
// (FindDomainError) that backs every Is* predicate in this file and in
// server.go: a predicate must answer for the OUTERMOST domain type present,
// not whichever domain type happens to be nested anywhere in the chain. This
// matters now that base implements Unwrap() — a plain errors.As would get
// nesting backwards and find an inner type before the outer one.
func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "plain error", err: errors.New("boom"), want: false},
		{name: "bare NotFound", err: NewNotFound("missing"), want: true},
		{name: "NotFound wrapped by fmt.Errorf %w", err: fmt.Errorf("context: %w", NewNotFound("missing")), want: true},
		{
			name: "NotFound nested underneath an outer Unexpected is NOT reported as NotFound",
			err:  NewUnexpected("failed to revoke access", NewNotFound("missing")),
			want: false,
		},
		{
			name: "outermost NotFound wrapping an inner Unexpected IS reported as NotFound",
			err:  NewNotFound("missing", NewUnexpected("cause")),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsNotFound(tt.err))
		})
	}
}

func TestIsConflict(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "plain error", err: errors.New("boom"), want: false},
		{name: "bare Conflict", err: NewConflict("stale revision"), want: true},
		{
			name: "Conflict nested underneath an outer Unexpected is NOT reported as Conflict",
			err:  NewUnexpected("failed to revoke access", NewConflict("stale revision")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsConflict(tt.err))
		})
	}
}

func TestIsValidation(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "plain error", err: errors.New("boom"), want: false},
		{name: "bare Validation", err: NewValidation("bad input"), want: true},
		{
			name: "Validation nested underneath an outer Unexpected is NOT reported as Validation",
			err:  NewUnexpected("failed to revoke access", NewValidation("bad input")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidation(tt.err))
		})
	}
}
