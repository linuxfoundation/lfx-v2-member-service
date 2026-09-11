// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"sync"
)

// MockUserMembershipReader provides a mock implementation of
// port.UserMembershipReader for testing and local development. Data is stored
// in-memory, keyed by username.
type MockUserMembershipReader struct {
	memberships map[string][]string
	mu          sync.RWMutex
}

// NewMockUserMembershipReader creates a new mock reader pre-seeded so that
// "keycontact1" is a key contact of the membership seeded by
// MockMembershipRepository, which serves the member-tiers hop-2 reads in
// mock mode.
func NewMockUserMembershipReader() *MockUserMembershipReader {
	return &MockUserMembershipReader{
		memberships: map[string][]string{
			"keycontact1": {"11111111-1111-1111-1111-111111111111"},
		},
	}
}

// SetUserMemberships replaces the membership UIDs the given username is a key
// contact of. Intended for test setup.
func (m *MockUserMembershipReader) SetUserMemberships(username string, uids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.memberships[username] = uids
}

// MembershipUIDsForUser returns the seeded membership UIDs for the given
// username; unknown usernames yield an empty slice, mirroring the real
// fga-sync behaviour.
func (m *MockUserMembershipReader) MembershipUIDsForUser(_ context.Context, username string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.memberships[username]...), nil
}
