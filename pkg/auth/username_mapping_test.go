// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMapUsernameToAuthSub(t *testing.T) {
	assert.Equal(t, "", MapUsernameToAuthSub(""))
	assert.Equal(t, "auth0|joiner", MapUsernameToAuthSub("joiner"))
	assert.Equal(t, "auth0|alice", MapUsernameToAuthSub("alice"))

	// Unsafe usernames (here an email) are hashed, never used verbatim.
	hashed := MapUsernameToAuthSub("accept@example.com")
	assert.NotEqual(t, "auth0|accept@example.com", hashed)
	assert.True(t, strings.HasPrefix(hashed, "auth0|"))
	assert.Greater(t, len(hashed), len("auth0|"))

	// Deterministic: the same legacy username always maps to the same sub.
	assert.Equal(t, hashed, MapUsernameToAuthSub("accept@example.com"))
}

func TestAuthSubLookupKey(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		want      string
	}{
		{name: "empty", principal: "", want: ""},
		{name: "blank trimmed", principal: "   ", want: ""},
		{name: "bare LFID is mapped to sub", principal: "alice", want: "auth0|alice"},
		{name: "already qualified auth0 sub passes through", principal: "auth0|alice", want: "auth0|alice"},
		{name: "other provider sub passes through", principal: "oidc|google|123", want: "oidc|google|123"},
		{name: "surrounding whitespace trimmed before mapping", principal: " bob ", want: "auth0|bob"},
		{name: "legacy username routes through the hash branch", principal: "accept@example.com", want: MapUsernameToAuthSub("accept@example.com")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, AuthSubLookupKey(tt.principal))
		})
	}
}
