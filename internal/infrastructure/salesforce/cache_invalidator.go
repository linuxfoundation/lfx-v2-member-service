// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// Ensure SObjectClient satisfies port.CacheInvalidator at compile time.
var _ port.CacheInvalidator = (*SObjectClient)(nil)

// InvalidateB2BOrg evicts the cached Account entries for the given v2 UID. It
// removes both the full B2BOrg profile slot ("b2b_org.{uid}") and the
// bare-minimum parent-detail slot ("b2b_org_parent.{uid}"), because the same
// Account may be cached under either prefix (a member company that is also the
// parent of another company). Evicting only one would leave the other stale
// after a Salesforce change event (LFXV2-2654).
func (c *SObjectClient) InvalidateB2BOrg(ctx context.Context, uid string) error {
	if err := c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid)); err != nil {
		return err
	}
	return c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgParent, uid))
}

// InvalidateProjectMembership evicts the cached project_membership record for
// the given v2 UID.
func (c *SObjectClient) InvalidateProjectMembership(ctx context.Context, uid string) error {
	return c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixProjectMembership, uid))
}

// InvalidateKeyContact evicts the cached key_contact record for the given v2
// UID.
func (c *SObjectClient) InvalidateKeyContact(ctx context.Context, uid string) error {
	return c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixKeyContact, uid))
}
