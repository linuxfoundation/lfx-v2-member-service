// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// MembershipCacheEvictor evicts one record from the soft-TTL membership cache
// that GetMemberTiers and GetMembership read from. It is separate from
// CacheInvalidator, which owns the sObject cache; CDC evicts both so a status
// or tier change is served fresh on the next read, not after the stale window.
type MembershipCacheEvictor interface {
	// DeleteMembership evicts the cached membership for the given v2 UID.
	// A missing entry is a no-op (already evicted).
	DeleteMembership(ctx context.Context, uid string) error
}
