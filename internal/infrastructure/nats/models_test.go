// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"testing"

	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsUserMissError(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{name: "search miss", errMsg: "user not found", want: true},
		{name: "get-by-id miss", errMsg: "The user does not exist.", want: true},
		{name: "miss is case-insensitive", errMsg: "USER NOT FOUND", want: true},
		{name: "does-not-exist is case-insensitive", errMsg: "The User Does Not Exist.", want: true},
		{name: "rate limit is not a miss", errMsg: "too_many_requests: Global limit has been reached", want: false},
		{name: "internal error is not a miss", errMsg: "internal server error", want: false},
		{name: "empty is not a miss", errMsg: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUserMissError(tt.errMsg))
		})
	}
}

func TestErrorMessageNATSResponseCheckError(t *testing.T) {
	tests := []struct {
		name    string
		message string
		wantErr func(t *testing.T, err error)
	}{
		{
			name:    "success envelope returns nil",
			message: `{"success":true}`,
			wantErr: func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:    "non-JSON returns nil",
			message: `plain-text-username`,
			wantErr: func(t *testing.T, err error) { require.NoError(t, err) },
		},
		{
			name:    "search miss envelope is NotFound",
			message: `{"success":false,"error":"user not found"}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "not-found envelope must be NotFound, got %v", err)
			},
		},
		{
			name:    "get-by-id miss envelope is NotFound",
			message: `{"success":false,"error":"The user does not exist."}`,
			wantErr: func(t *testing.T, err error) {
				assert.True(t, pkgerrors.IsNotFound(err), "does-not-exist envelope must be NotFound, got %v", err)
			},
		},
		{
			name:    "rate-limit envelope is unexpected",
			message: `{"success":false,"error":"too_many_requests: Global limit has been reached"}`,
			wantErr: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.False(t, pkgerrors.IsNotFound(err), "a rate-limit error must NOT be a miss")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e ErrorMessageNATSResponse
			tt.wantErr(t, e.CheckError(tt.message))
		})
	}
}
