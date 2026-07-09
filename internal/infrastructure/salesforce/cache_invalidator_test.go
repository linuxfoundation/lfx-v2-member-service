// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
)

// TestInvalidateB2BOrg_ClearsAllThreeKeys verifies that InvalidateB2BOrg
// evicts the full-org, flat-account, and parent-brief cache entries for the
// same UID, not only the full-org entry (see LFXV2-2654: before the B2BOrg
// cache key split, these three fetch shapes shared a single key, so a single
// InvalidateCache call covered all of them; splitting the read keys without
// updating invalidation would leave the other two stale).
func TestInvalidateB2BOrg_ClearsAllThreeKeys(t *testing.T) {
	t.Parallel()

	const uid = "00000000-0000-0000-0000-000000000001"

	cache := newMemCache()
	entry := &nats.SObjectCacheEntry{Body: json.RawMessage(`{}`)}
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid), entry))
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, uid), entry))
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, uid), entry))

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(200, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: cache}

	err := client.InvalidateB2BOrg(context.Background(), uid)
	require.NoError(t, err)

	for _, key := range []string{
		sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, uid),
	} {
		stored, getErr := cache.Get(context.Background(), key)
		require.NoError(t, getErr)
		assert.Nil(t, stored, "key %q must be evicted", key)
	}
}

// TestInvalidateB2BOrg_NoErrorWhenNoEntriesExist verifies that invalidating a
// UID with no cached entries under any of the three keys is a no-op, not an
// error.
func TestInvalidateB2BOrg_NoErrorWhenNoEntriesExist(t *testing.T) {
	t.Parallel()

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(200, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}

	err := client.InvalidateB2BOrg(context.Background(), "00000000-0000-0000-0000-000000000002")

	require.NoError(t, err)
}
