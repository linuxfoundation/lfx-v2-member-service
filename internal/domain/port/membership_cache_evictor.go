// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// MembershipCacheEvictor evicts a single record from the soft-TTL membership
// cache (the membership-cache bucket) that GetMemberTiers and GetMembership
// read from. It is separate from CacheInvalidator, which owns the sObject REST
// cache; CDC evicts both so a deactivated or re-tiered membership stops being
// served as active within one change event instead of lingering for the
// soft-TTL staleness window.
type MembershipCacheEvictor interface {
	// DeleteMembership evicts the cached membership for the given v2 UID.
	// A missing entry is a no-op (already evicted).
	DeleteMembership(ctx context.Context, uid string) error
}
