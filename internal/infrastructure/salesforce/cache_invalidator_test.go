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

// TestInvalidateB2BOrg_ClearsAllB2BOrgKeys verifies that InvalidateB2BOrg evicts
// every B2BOrg-family cache entry for the same UID: the legacy full-org key, the
// retired v2 / v3 / v3_noslug keys, the current v4 key, the flat-account key,
// and the parent-brief key. Deploys can leave any of them populated until the
// old entries are evicted.
func TestInvalidateB2BOrg_ClearsAllB2BOrgKeys(t *testing.T) {
	t.Parallel()

	const uid = "00000000-0000-0000-0000-000000000001"

	cache := newMemCache()
	entry := &nats.SObjectCacheEntry{Body: json.RawMessage(`{}`)}
	allKeys := []string{
		sobjectCacheKey(sobjectKeyPrefixB2BOrgLegacy, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgV2Legacy, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgV3Legacy, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgV3NoSlugLegacy, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, uid),
		sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, uid),
	}
	for _, key := range allKeys {
		require.NoError(t, cache.Put(context.Background(), key, entry))
	}

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(200, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: cache}

	err := client.InvalidateB2BOrg(context.Background(), uid)
	require.NoError(t, err)

	for _, key := range allKeys {
		stored, getErr := cache.Get(context.Background(), key)
		require.NoError(t, getErr)
		assert.Nil(t, stored, "key %q must be evicted", key)
	}
}

// TestInvalidateB2BOrg_NoErrorWhenNoEntriesExist verifies that invalidating a
// UID with no cached entries under any of the legacy/current B2BOrg keys is a
// no-op, not an error.
func TestInvalidateB2BOrg_NoErrorWhenNoEntriesExist(t *testing.T) {
	t.Parallel()

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(200, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}

	err := client.InvalidateB2BOrg(context.Background(), "00000000-0000-0000-0000-000000000002")

	require.NoError(t, err)
}
