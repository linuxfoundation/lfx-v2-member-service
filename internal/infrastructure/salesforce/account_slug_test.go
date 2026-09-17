// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeOrgSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase passthrough", in: "google-llc", want: "google-llc"},
		{name: "mixed case lowered", in: "Google-LLC", want: "google-llc"},
		{name: "surrounding whitespace trimmed", in: "  ToIP \n", want: "toip"},
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace-only becomes empty", in: "   ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, normalizeOrgSlug(tt.in))
		})
	}
}

// TestConvertSOQLToB2BOrg_SlugLowercased verifies the SOQL list/search path now
// carries Account.Slug__c, lowercased at ingest, so `data.slug` and the `slug:`
// tag agree with the canonical URL (lfx-self-serve#2570).
func TestConvertSOQLToB2BOrg_SlugLowercased(t *testing.T) {
	t.Parallel()

	mixed := "Google-LLC"
	org, err := convertSOQLToB2BOrg(context.Background(), soqlAccount{ID: canonicalAccountSFID, Name: "Google LLC", Slug: &mixed})
	require.NoError(t, err)
	assert.Equal(t, "google-llc", org.Slug)

	none, err := convertSOQLToB2BOrg(context.Background(), soqlAccount{ID: canonicalAccountSFID, Name: "No Slug Org"})
	require.NoError(t, err)
	assert.Empty(t, none.Slug, "absent Slug__c must not be generated")
}

// TestAccountSlugFieldToggle covers the per-environment Slug__c projection
// switch on both Account read paths. Deliberately NOT parallel: the toggle is
// process-wide, and Go runs t.Parallel tests only after every sequential
// top-level test has returned, so flipping it here cannot leak into them.
func TestAccountSlugFieldToggle(t *testing.T) {
	t.Cleanup(func() { setAccountSlugFieldEnabled(true) })

	t.Run("enabled by default: SOQL SELECT and sObject fields include Slug__c", func(t *testing.T) {
		setAccountSlugFieldEnabled(true)
		assert.Contains(t, accountsSOQLBase(), "Slug__c")
		assert.Contains(t, withAccountSlugField(b2bOrgFieldsBase), ",Slug__c")
	})

	t.Run("disabled: Slug__c omitted from both paths", func(t *testing.T) {
		setAccountSlugFieldEnabled(false)
		assert.NotContains(t, accountsSOQLBase(), "Slug__c")
		assert.Equal(t, b2bOrgFieldsBase, withAccountSlugField(b2bOrgFieldsBase))
		// The rest of the projection is untouched either way.
		assert.Contains(t, accountsSOQLBase(), "LF_Membership_Status__c")
	})

	t.Run("FetchB2BOrg sends Slug__c in ?fields= only when enabled", func(t *testing.T) {
		uid, err := sfuuid.Normalize18(canonicalAccountSFID)
		require.NoError(t, err)

		for _, enabled := range []bool{true, false} {
			setAccountSlugFieldEnabled(enabled)
			rt := newRoutingTransport(fakeResponse(http.StatusOK, canonicalAccountJSON, nil))
			client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}

			_, _, err := client.FetchB2BOrg(context.Background(), uid)
			require.NoError(t, err)

			req := rt.lastSObjectRequest()
			require.NotNil(t, req)
			fields := req.URL.Query().Get("fields")
			assert.Equal(t, enabled, strings.Contains(fields, "Slug__c"), "enabled=%v fields=%q", enabled, fields)
		}
	})

	t.Run("toggle flip never replays a body cached under the other projection", func(t *testing.T) {
		uid, err := sfuuid.Normalize18(canonicalAccountSFID)
		require.NoError(t, err)

		// Distinguishable fixtures per projection, so a read from the wrong cache
		// entry changes the decoded org rather than passing on identical bytes:
		// the slug projection answers with Slug__c, the noslug projection without.
		var noSlugDoc map[string]any
		require.NoError(t, json.Unmarshal([]byte(canonicalAccountJSON), &noSlugDoc))
		delete(noSlugDoc, "Slug__c")
		noSlugJSON, err := json.Marshal(noSlugDoc)
		require.NoError(t, err)

		// Call 1 (enabled): 200 with slug. Call 2 (disabled): 200 without slug.
		// Call 3 (re-enabled): 304, so whatever is served comes from the cache
		// entry the code chose to key on — that choice is what the test proves.
		rt := &sequenceTransport{responses: []*http.Response{
			fakeResponse(http.StatusOK, canonicalAccountJSON, map[string]string{"ETag": `"with-slug"`}),
			fakeResponse(http.StatusOK, string(noSlugJSON), map[string]string{"ETag": `"no-slug"`}),
			fakeResponse(http.StatusNotModified, "", nil),
		}}
		cache := newMemCache()
		client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: cache}

		// Enabled: first fetch populates the slug-projection key.
		setAccountSlugFieldEnabled(true)
		first, _, err := client.FetchB2BOrg(context.Background(), uid)
		require.NoError(t, err)
		require.Equal(t, 1, rt.calls())
		require.Equal(t, "linux-foundation", first.Slug)
		withSlug, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid))
		require.NoError(t, err)
		require.NotNil(t, withSlug, "enabled fetch writes the b2b_org_v3 key")

		// Disabled: must NOT read the slug-projection entry (which would 304 and
		// refresh forever) — a fresh request goes out and lands under the noslug key.
		setAccountSlugFieldEnabled(false)
		second, _, err := client.FetchB2BOrg(context.Background(), uid)
		require.NoError(t, err)
		assert.Equal(t, 2, rt.calls(), "disabled fetch must not be served from the slug-projection cache entry")
		assert.Empty(t, second.Slug, "disabled fetch decodes the slug-less body")
		noSlug, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgNoSlug, uid))
		require.NoError(t, err)
		require.NotNil(t, noSlug, "disabled fetch writes the b2b_org_v3_noslug key")

		// Re-enabled: Salesforce answers 304, so the body is whatever entry the
		// client keyed on. Only the slug-projection entry carries a slug — reading
		// the noslug entry by mistake would decode an empty Slug and fail here.
		setAccountSlugFieldEnabled(true)
		third, _, err := client.FetchB2BOrg(context.Background(), uid)
		require.NoError(t, err)
		assert.Equal(t, 3, rt.calls())
		assert.Equal(t, "linux-foundation", third.Slug, "re-enabled 304 must be served from the slug-projection entry, not the noslug one")
	})

	t.Run("Config.Init applies the toggle from the config", func(t *testing.T) {
		// Init needs live credentials to authenticate, so only the toggle side
		// effect is exercised: it runs before any network call and must survive
		// the (expected) auth failure.
		setAccountSlugFieldEnabled(true)
		_, _ = Config{AccountSlugFieldDisabled: true}.Init()
		assert.False(t, accountSlugFieldEnabled.Load(), "disabled config must switch the projection off")

		_, _ = Config{}.Init()
		assert.True(t, accountSlugFieldEnabled.Load(), "zero-value config must keep the projection on")
	})
}

// TestSObjectClient_CacheKeyIsolation_V2PoisonedFullOrgKeyIgnored verifies that
// a pre-slug "b2b_org_v2.{uid}" entry is ignored after the v3 bump: the sObject
// cache key carries no field-list component and handle304 keeps a hot entry's
// TTL alive forever, so without the bump a slug-less body would be served
// indefinitely (spec 050 / DR-001 evidence).
func TestSObjectClient_CacheKeyIsolation_V2PoisonedFullOrgKeyIgnored(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	// A full-shaped pre-slug body: everything b2bOrgFieldsBase returns, no Slug__c.
	var preSlug map[string]any
	require.NoError(t, json.Unmarshal([]byte(canonicalAccountJSON), &preSlug))
	delete(preSlug, "Slug__c")
	preSlugJSON, err := json.Marshal(preSlug)
	require.NoError(t, err)

	cache := newMemCache()
	require.NoError(t, cache.Put(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgV2Legacy, uid), &nats.SObjectCacheEntry{
		Body: json.RawMessage(preSlugJSON),
		ETag: `"pre-slug"`,
	}))

	callCount := 0
	rt := &countingTransport{
		callCount:  &callCount,
		firstResp:  fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		retryResp:  fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		limitsResp: fakeResponse(http.StatusOK, `{}`, nil),
	}
	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: cache}

	org, _, err := client.FetchB2BOrg(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, org)

	assert.Equal(t, 1, callCount, "FetchB2BOrg must ignore the v2 key and issue a fresh HTTP request")
	assert.Equal(t, "linux-foundation", org.Slug, "fresh fetch must carry the slug the v2 body lacked")

	current, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid))
	require.NoError(t, err)
	require.NotNil(t, current, "fresh body must be written under the v3 key")
	assert.JSONEq(t, canonicalAccountJSON, string(current.Body))

	stale, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgV2Legacy, uid))
	require.NoError(t, err)
	require.NotNil(t, stale, "v2 entry is left for InvalidateB2BOrg / the deploy purge, never read")

	require.NoError(t, client.InvalidateB2BOrg(context.Background(), uid))
	gone, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrgV2Legacy, uid))
	require.NoError(t, err)
	assert.Nil(t, gone, "InvalidateB2BOrg must evict the v2 key too")
}

// sequenceTransport answers sObject requests with a fixed ordered list of
// responses (one per call) and /limits with 200, so a test can script a
// 200 → 200 → 304 conversation and inspect which cached body a 304 resolves to.
type sequenceTransport struct {
	mu        sync.Mutex
	responses []*http.Response
	n         int
}

func (st *sequenceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if strings.Contains(req.URL.Path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}
	if st.n >= len(st.responses) {
		return nil, fmt.Errorf("sequenceTransport: unexpected sObject call #%d", st.n+1)
	}
	resp := cloneResponse(st.responses[st.n])
	st.n++
	return resp, nil
}

func (st *sequenceTransport) calls() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.n
}
