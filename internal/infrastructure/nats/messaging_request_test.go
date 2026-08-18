// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"testing"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserReader_UsernameByEmail_TransportFailureIsUnexpected(t *testing.T) {
	const email = "private@example.com"
	reader := &userReader{client: &NATSClient{}}

	username, err := reader.UsernameByEmail(context.Background(), email)

	require.Error(t, err)
	assert.Empty(t, username)
	assert.False(t, pkgerrors.IsNotFound(err), "transport failure must not look like a definitive identity miss")
	var unexpected pkgerrors.Unexpected
	assert.ErrorAs(t, err, &unexpected)
	assert.NotContains(t, err.Error(), email, "lookup errors must not expose the raw email")
}

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
			name: "search miss envelope is a miss",
			body: `{"success":false,"error":"user not found"}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "not-found envelope must be NotFound, got %v", err)
			},
		},
		{
			name: "get-by-id miss envelope is a miss",
			body: `{"success":false,"error":"The user does not exist."}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "auth0| get-by-id 404 must be NotFound, got %v", err)
			},
		},
		{
			name: "rate-limit envelope is unexpected (transient, not a miss)",
			body: `{"success":false,"error":"too_many_requests: Global limit has been reached"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err), "a rate-limit error must NOT be reported as a miss")
			},
		},
		{
			name: "non-miss error envelope is unexpected (not swallowed as a miss)",
			body: `{"success":false,"error":"internal server error"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err), "an auth-service failure must NOT be reported as a miss")
			},
		},
		{
			name: "empty body is a miss",
			body: ``,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "an absent body must be NotFound, got %v", err)
			},
		},
		{
			name: "whitespace-only body is a miss",
			body: "   \n\t",
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "a whitespace-only body must be NotFound, got %v", err)
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
