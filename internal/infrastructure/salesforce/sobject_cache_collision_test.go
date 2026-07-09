// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// collisionTransport simulates a Salesforce sObject REST endpoint for two
// related Accounts: a subsidiary (childSFID) whose ParentId points at a parent
// (parentSFID). It reproduces the conditional-GET semantics that caused
// LFXV2-2654: a full field request returns the full body, a narrow request
// returns only Id/Name/Logo, and ANY conditional request (If-None-Match or
// If-Modified-Since) returns 304 Not Modified — because the underlying record
// has not changed, only the requested field set differs.
type collisionTransport struct {
	mu         sync.Mutex
	childSFID  string
	parentSFID string
	childJSON  string
	parentFull string
	parentMin  string
	requests   []*http.Request
}

func (t *collisionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests = append(t.requests, req.Clone(req.Context()))

	path := req.URL.Path
	if strings.Contains(path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}

	conditional := req.Header.Get("If-None-Match") != "" || req.Header.Get("If-Modified-Since") != ""
	fields := req.URL.Query().Get("fields")

	switch {
	case strings.Contains(path, t.parentSFID):
		// A conditional revalidation of an unchanged record returns 304 and no
		// body — the caller must serve whatever it previously cached.
		if conditional {
			return fakeResponse(http.StatusNotModified, "", nil), nil
		}
		if strings.Contains(fields, "Description") {
			return fakeResponse(http.StatusOK, t.parentFull, map[string]string{"ETag": "parent-full"}), nil
		}
		return fakeResponse(http.StatusOK, t.parentMin, map[string]string{"ETag": "parent-min"}), nil
	case strings.Contains(path, t.childSFID):
		if conditional {
			return fakeResponse(http.StatusNotModified, "", nil), nil
		}
		return fakeResponse(http.StatusOK, t.childJSON, map[string]string{"ETag": "child-full"}), nil
	default:
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}
}

// TestFetchB2BOrg_ParentLookupDoesNotPoisonFullProfile is a regression test for
// LFXV2-2654. Looking up a subsidiary triggers a bare-minimum (Id, Name, Logo)
// fetch of its parent Account. A subsequent full-profile lookup of that same
// parent must return the complete B2BOrg — including Description, Industry,
// employee count, and membership status — not the bare-minimum body cached
// during the subsidiary lookup.
//
// Before the fix both lookups shared the "b2b_org.{uid}" cache slot, so the
// full lookup revalidated the bare-minimum entry, received 304 Not Modified,
// and served the incomplete body. The fix keys the parent-detail fetch under a
// separate "b2b_org_parent.{uid}" slot.
func TestFetchB2BOrg_ParentLookupDoesNotPoisonFullProfile(t *testing.T) {
	t.Parallel()

	childUID, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)
	parentUID, err := sfuuid.Normalize18(parentAccountSFID)
	require.NoError(t, err)

	// Subsidiary: full profile whose ParentId points at the parent Account.
	childJSON := fmt.Sprintf(`{
		"Id":%q,
		"Name":"Red Hat",
		"Logo_URL__c":"https://redhat.com/logo.png",
		"Website":"https://redhat.com",
		"Description":"Enterprise open source.",
		"ParentId":%q,
		"Industry":"Technology",
		"NumberOfEmployees":20000,
		"LF_Membership_Status__c":"Active",
		"CreatedDate":"2019-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2024-01-01T00:00:00.000+0000",
		"SystemModstamp":"2024-01-01T00:00:00.000+0000"
	}`, childUID, parentUID)

	// Parent: full profile (returned when Description is among the requested
	// fields) — this is what a direct lookup must yield.
	parentFull := fmt.Sprintf(`{
		"Id":%q,
		"Name":"Global Parent Org",
		"Logo_URL__c":"https://parent.org/logo.png",
		"Website":"https://parent.org",
		"Description":"The parent company profile.",
		"ParentId":null,
		"Industry":"Manufacturing",
		"NumberOfEmployees":350000,
		"LF_Membership_Status__c":"Active",
		"CreatedDate":"2000-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2024-02-02T00:00:00.000+0000",
		"SystemModstamp":"2024-02-02T00:00:00.000+0000"
	}`, parentUID)

	// Parent: bare-minimum body (returned for the narrow parent-detail fetch).
	parentMin := fmt.Sprintf(`{
		"Id":%q,
		"Name":"Global Parent Org",
		"Logo_URL__c":"https://parent.org/logo.png",
		"CreatedDate":"2000-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2024-02-02T00:00:00.000+0000",
		"SystemModstamp":"2024-02-02T00:00:00.000+0000"
	}`, parentUID)

	transport := &collisionTransport{
		childSFID:  childUID,
		parentSFID: parentUID,
		childJSON:  childJSON,
		parentFull: parentFull,
		parentMin:  parentMin,
	}
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}

	// 1. Look up the subsidiary. This fetches the child (full) and its parent
	//    (bare-minimum), populating the parent-detail cache slot.
	childOrg, _, err := client.FetchB2BOrg(context.Background(), childUID)
	require.NoError(t, err)
	require.NotNil(t, childOrg)
	require.Equal(t, parentUID, childOrg.ParentUID)
	require.NotNil(t, childOrg.ParentDetail, "subsidiary lookup should populate parent detail")
	assert.Equal(t, "Global Parent Org", childOrg.ParentDetail.Name)

	// 2. Now look up the parent directly. It must return the FULL profile.
	parentOrg, _, err := client.FetchB2BOrg(context.Background(), parentUID)
	require.NoError(t, err)
	require.NotNil(t, parentOrg)
	assert.Equal(t, parentUID, parentOrg.UID)
	assert.Equal(t, "Global Parent Org", parentOrg.Name)
	assert.Equal(t, "The parent company profile.", parentOrg.Description,
		"full parent profile must include Description (regression: LFXV2-2654)")
	assert.Equal(t, "Manufacturing", parentOrg.Industry,
		"full parent profile must include Industry")
	assert.Equal(t, "Active", parentOrg.Status,
		"full parent profile must include membership status")
	require.NotNil(t, parentOrg.NumberOfEmployees, "full parent profile must include employee count")
	assert.Equal(t, int64(350000), *parentOrg.NumberOfEmployees)
}

// TestInvalidateB2BOrg_EvictsParentDetailSlot verifies that invalidating a
// B2BOrg removes both the full-profile slot and the bare-minimum parent-detail
// slot, so a Salesforce change event does not leave a stale parent detail
// behind (LFXV2-2654).
func TestInvalidateB2BOrg_EvictsParentDetailSlot(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	cache := newMemCache()
	fullKey := sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid)
	parentKey := sobjectCacheKey(sobjectKeyPrefixB2BOrgParent, uid)

	require.NoError(t, cache.Put(context.Background(), fullKey, &nats.SObjectCacheEntry{Body: []byte(`{}`)}))
	require.NoError(t, cache.Put(context.Background(), parentKey, &nats.SObjectCacheEntry{Body: []byte(`{}`)}))

	client := &SObjectClient{cache: cache}
	require.NoError(t, client.InvalidateB2BOrg(context.Background(), uid))

	full, err := cache.Get(context.Background(), fullKey)
	require.NoError(t, err)
	assert.Nil(t, full, "full b2b_org slot must be evicted")

	parent, err := cache.Get(context.Background(), parentKey)
	require.NoError(t, err)
	assert.Nil(t, parent, "b2b_org_parent slot must also be evicted")
}
