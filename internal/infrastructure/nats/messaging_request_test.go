// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"testing"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUserMetadataResponse(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantPicture string
		wantErr     func(t *testing.T, err error)
	}{
		{
			name:        "success with picture",
			body:        `{"success":true,"data":{"picture":"https://example.com/a.png","name":"Alice"}}`,
			wantPicture: "https://example.com/a.png",
			wantErr:     func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:        "success with no picture field",
			body:        `{"success":true,"data":{"name":"Alice"}}`,
			wantPicture: "",
			wantErr:     func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name: "success true but data absent is a miss",
			body: `{"success":true}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "absent data must be NotFound, got %v", err)
			},
		},
		{
			name: "genuine not-found envelope is a miss",
			body: `{"success":false,"error":"user not found"}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "not-found envelope must be NotFound, got %v", err)
			},
		},
		{
			name: "non not-found error envelope is unexpected (not swallowed as a miss)",
			body: `{"success":false,"error":"internal server error"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err), "an auth-service failure must NOT be reported as a miss")
			},
		},
		{
			name: "malformed JSON is unexpected",
			body: `not json`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err), "malformed JSON must NOT be reported as a miss")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, err := parseUserMetadataResponse("alice", []byte(tt.body))
			tt.wantErr(t, err)
			assert.Equal(t, tt.wantPicture, meta.Picture)
		})
	}
}
