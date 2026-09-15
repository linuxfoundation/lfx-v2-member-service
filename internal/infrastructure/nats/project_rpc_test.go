// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"testing"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseProjectRPCReply pins the discrimination contract between a confirmed
// "project not found" and any other service error for both RPC subjects.
// GetSlug and SlugToUID delegate directly to request(), which calls
// parseProjectRPCReply, so the table below covers the observable behaviour of
// both public methods when a reply is actually received.
func TestParseProjectRPCReply(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		wantValue string
		wantErr   func(t *testing.T, err error)
	}{
		{
			name:      "plain UID is a success",
			body:      []byte("01234567-89ab-cdef-0123-456789abcdef"),
			wantValue: "01234567-89ab-cdef-0123-456789abcdef",
			wantErr:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:      "plain slug is a success",
			body:      []byte("kubernetes"),
			wantValue: "kubernetes",
			wantErr:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:      "surrounding whitespace is trimmed on success",
			body:      []byte("  kubernetes\n"),
			wantValue: "kubernetes",
			wantErr:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "not_found envelope is a definitive NotFound",
			body: []byte(`{"error":"not_found","message":"project not found"}`),
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, pkgerrors.IsNotFound(err),
					"not_found code must map to NotFound, got %v", err)
			},
		},
		{
			name: "not_found without message is still NotFound",
			body: []byte(`{"error":"not_found"}`),
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.True(t, pkgerrors.IsNotFound(err),
					"not_found code must map to NotFound, got %v", err)
			},
		},
		{
			name: "internal code is Unexpected, not NotFound",
			body: []byte(`{"error":"internal","message":"internal server error"}`),
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err),
					"internal code must NOT map to NotFound — callers must be able to tell a transient failure from a confirmed absence")
				var unexpected pkgerrors.Unexpected
				assert.ErrorAs(t, err, &unexpected,
					"internal code must map to Unexpected, got %v", err)
			},
		},
		{
			name: "unknown future code is Unexpected, not NotFound",
			body: []byte(`{"error":"unknown_future_code"}`),
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err),
					"an unrecognised code must NOT map to NotFound")
				var unexpected pkgerrors.Unexpected
				assert.ErrorAs(t, err, &unexpected)
			},
		},
		{
			name:      "empty body is an empty success (caller may handle)",
			body:      []byte{},
			wantValue: "",
			wantErr:   func(t *testing.T, err error) { require.NoError(t, err) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProjectRPCReply(tt.body)
			tt.wantErr(t, err)
			assert.Equal(t, tt.wantValue, got)
		})
	}
}
