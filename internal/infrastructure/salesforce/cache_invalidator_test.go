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

// TestInvalidateB2BOrg_ClearsAllFourKeys verifies that InvalidateB2BOrg evicts
// the legacy full-org, current full-org, flat-account, and parent-brief cache
// entries for the same UID. After the field-list split, deploys can see both
// the legacy and current full-org keys until the old entries age out.
func TestInvalidateB2BOrg_ClearsAllFourKeys(t *testing.T) {
	t.Parallel()

	const uid = "00000000-0000-0000-0000-000000000001"

	cache := newMemCache()
	entry := &nats.SObjectCacheEntry{Body: json.RawMessage(`{}`)}
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgLegacy, uid), entry))
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid), entry))
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgFlat, uid), entry))
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgParentBrief, uid), entry))

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(200, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: cache}

	err := client.InvalidateB2BOrg(context.Background(), uid)
	require.NoError(t, err)

	for _, key := range []string{
		sobjectCacheKey(sobjectKeyPrefixB2BOrgLegacy, uid),
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
