// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The mock must mirror the real fga-sync contract that the member-tiers
// handler is built on: an unknown user is a definitive empty result with a
// nil error, because the endpoint returns 200 [] for unknown users so callers
// cannot probe which usernames exist.
func TestMockUserMembershipReader_UnknownUserIsEmptyNotError(t *testing.T) {
	reader := NewMockUserMembershipReader()

	uids, err := reader.MembershipUIDsForUser(context.Background(), "nobody")

	require.NoError(t, err, "unknown user is a miss, not a failure")
	assert.Empty(t, uids)
}

// The returned slice must be a copy: handler code and tests may mutate their
// result, and that must not corrupt the seeded state shared across calls.
func TestMockUserMembershipReader_ResultIsIsolatedFromSeed(t *testing.T) {
	reader := NewMockUserMembershipReader()
	reader.SetUserMemberships("jdoe", []string{"m-1", "m-2"})

	first, err := reader.MembershipUIDsForUser(context.Background(), "jdoe")
	require.NoError(t, err)
	require.NotEmpty(t, first)
	first[0] = "mutated"

	second, err := reader.MembershipUIDsForUser(context.Background(), "jdoe")
	require.NoError(t, err)
	assert.Equal(t, []string{"m-1", "m-2"}, second, "seeded state must not be corrupted by mutating a returned slice")
}
