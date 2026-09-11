// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// UserMembershipReader resolves the project memberships a user is directly
// associated with as a key contact, by LFID username. Implementations return a
// reverse index only: candidate membership UIDs that callers must verify
// against the membership records themselves, which remain the authority.
type UserMembershipReader interface {
	// MembershipUIDsForUser returns the UIDs of the project memberships
	// (Assets) the given user is a key contact of. An unknown user yields an
	// empty slice.
	MembershipUIDsForUser(ctx context.Context, username string) ([]string, error)
}
