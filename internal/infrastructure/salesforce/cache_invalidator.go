// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"errors"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// Ensure SObjectClient satisfies port.CacheInvalidator at compile time.
var _ port.CacheInvalidator = (*SObjectClient)(nil)

// InvalidateB2BOrg evicts every cached B2BOrg-family entry for the given v2
// UID: the legacy pre-split full-org key (sobjectKeyPrefixB2BOrgLegacy), the
// pre-slug full-org key (sobjectKeyPrefixB2BOrgV2Legacy), both current
// full-org projections (sobjectKeyPrefixB2BOrg, sobjectKeyPrefixB2BOrgNoSlug),
// the flat-account fetch (sobjectKeyPrefixB2BOrgFlat), and the parent-brief
// fetch (sobjectKeyPrefixB2BOrgParentBrief). All six keys may exist across
// deploys and toggle flips, so callers invalidating an Account must not be left
// with a stale entry under any of them (see LFXV2-2654, lfx-self-serve#2570).
func (c *SObjectClient) InvalidateB2BOrg(ctx context.Context, uid string) error {
	return errors.Join(
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgLegacy, uid)),
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgV2Legacy, uid)),
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid)),
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgNoSlug, uid)),
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, uid)),
		c.InvalidateCache(ctx, sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, uid)),
	)
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
