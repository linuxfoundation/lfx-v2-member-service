// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

// predicateAssertingTransport returns the fixed /limits response for sf.Init,
// then for each /query call returns response only if the decoded SOQL query
// string contains wantSubstr, else an empty result. Unlike seqQueryTransport
// (which returns canned responses regardless of query content), this
// validates the actual SOQL predicate sent to Salesforce rather than just the
// in-process verify filtering applied after a fetch.
type predicateAssertingTransport struct {
	wantSubstr string
	response   string
	queryCalls int
}

func (t *predicateAssertingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/limits") {
		return fakeResponse(http.StatusOK, `{}`, nil), nil
	}
	t.queryCalls++
	q, err := url.QueryUnescape(req.URL.RawQuery)
	if err != nil {
		return fakeResponse(http.StatusOK, `{"totalSize":0,"done":true,"records":[]}`, nil), nil
	}
	if strings.Contains(q, t.wantSubstr) {
		return fakeResponse(http.StatusOK, t.response, nil), nil
	}
	return fakeResponse(http.StatusOK, `{"totalSize":0,"done":true,"records":[]}`, nil), nil
}

// soqlAccountRecord builds a single-record JSON fragment matching the
// soqlAccount JSON tags for a b2b_org-eligible Company Account, for use in a
// SOQL query response body.
func soqlAccountRecord(id, name, website, primaryDomain, domainAlias string) string {
	return soqlAccountRecordWithType(id, name, website, primaryDomain, domainAlias, model.RecordTypeCompanyAccount)
}

// soqlAccountRecordWithType is soqlAccountRecord with an explicit record type
// and no Membership Asset, so a non-Company type yields an ineligible Account.
func soqlAccountRecordWithType(id, name, website, primaryDomain, domainAlias, recordType string) string {
	return fmt.Sprintf(
		`{"Id":%q,"Name":%q,"Website":%q,"Account_Domain__c":%q,"Domain_Alias__c":%q,`,
		id, name, website, primaryDomain, domainAlias,
	) + eligibilityFields(recordType, false) + `}`
}

// soslSearchResponse builds a SOSL /search response body returning ids.
func soslSearchResponse(ids ...string) string {
	records := make([]string, len(ids))
	for i, id := range ids {
		records[i] = fmt.Sprintf(`{"attributes":{"type":"Account"},"Id":%q}`, id)
	}
	return `{"searchRecords":[` + strings.Join(records, ",") + `]}`
}

// longTextFilterTransport is seqQueryTransport plus Salesforce's rule that a
// long text area field such as Domain_Alias__c can be selected but not
// filtered on: a /query naming it after FROM Account gets the 400
// INVALID_FIELD response Salesforce returns.
type longTextFilterTransport struct {
	seqQueryTransport
}

func (t *longTextFilterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/query") {
		if _, filter, ok := strings.Cut(req.URL.Query().Get("q"), "FROM Account"); ok && strings.Contains(filter, "Domain_Alias__c") {
			return fakeResponse(http.StatusBadRequest,
				`[{"message":"field 'Domain_Alias__c' can not be filtered in a query call","errorCode":"INVALID_FIELD"}]`, nil), nil
		}
	}
	return t.seqQueryTransport.RoundTrip(req)
}

// cancelOnSearchTransport is seqQueryTransport that calls cancel when the
// SOSL search arrives, i.e. while that request is in flight.
type cancelOnSearchTransport struct {
	seqQueryTransport
	cancel context.CancelFunc
}

func (t *cancelOnSearchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/search") {
		t.cancel()
	}
	return t.seqQueryTransport.RoundTrip(req)
}

func soqlAccountsResponse(records ...string) string {
	body := "["
	for i, r := range records {
		if i > 0 {
			body += ","
		}
		body += r
	}
	body += "]"
	return fmt.Sprintf(`{"totalSize":%d,"done":true,"records":%s}`, len(records), body)
}

func TestAccountRepo_FindAccountByNameOrWebsite_PrimaryDomainMatch(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(1)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "https://acme.com", "acme.com", "")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "https://acme.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
	assert.Equal(t, 1, tr.queryCalls, "primary_domain tier match should not fall through to later tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasMatch(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(2)
	tr := &longTextFilterTransport{seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(sfid),
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "other.com", "acme.com, acme.io")),
	}}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
	require.Equal(t, 3, tr.queryCalls)
	assert.True(t, strings.HasSuffix(tr.paths[1], "/search"), "the alias step must search, got %s", tr.paths[1])
	assert.Contains(t, tr.queries[1], `FIND {"acme.com"}`)
	assert.Contains(t, tr.queries[2], "Id IN ('"+sfid+"')", "the re-read must load exactly the Ids the search returned")
}

func TestAccountRepo_FindAccountByNameOrWebsite_MalformedAliasSearchFailsClosed(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(33)
	for _, tc := range []struct{ name, body, wantErr string }{
		{"no searchRecords", `{}`, "no searchRecords"},
		{"null searchRecords", `{"searchRecords":null}`, "no searchRecords"},
		{"empty record", `{"searchRecords":[{}]}`, "has no Id"},
		{"null record", `{"searchRecords":[null]}`, "has no Id"},
		// Dropping the Id-less record would leave sfid as a unique match.
		{"Id-less record next to a valid one", `{"searchRecords":[{"Id":"` + sfid + `"},{"attributes":{"type":"Account"}}]}`, "has no Id"},
		// Valid JSON, so only the size cap can reject it.
		{"oversized body", `{"searchRecords":[{"Id":"` + sfid + `"}]}` + strings.Repeat(" ", soslMaxResponseBytes), "exceeds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &seqQueryTransport{responses: []string{
				soqlAccountsResponse(), // primary_domain tier: no match
				tc.body,
				soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "other.com", "acme.com")),
			}}
			repo := NewAccountRepo(fakeSalesforce(t, tr), true)

			org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", "acme.com")
			require.ErrorContains(t, err, tc.wantErr, "a malformed search response must not read as fewer candidates")
			assert.False(t, ok)
			assert.Nil(t, org)
			assert.Equal(t, 2, tr.queryCalls, "it must stop before the website/name tiers")
		})
	}
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasAmbiguousStopsEscalation(t *testing.T) {
	t.Parallel()

	us, eu := batchParentSFID(30), batchParentSFID(31)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(us, eu),
		soqlAccountsResponse(
			soqlAccountRecord(us, "Acme US", "", "acme.us", "acme.com"),
			soqlAccountRecord(eu, "Acme EU", "", "acme.eu", "acme.com"),
		),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme US", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "two Accounts sharing an alias must not resolve to either")
	assert.Nil(t, org)
	assert.Equal(t, 3, tr.queryCalls, "ambiguity at domain_alias must stop before the website/name tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasSearchErrorPropagates(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{
		responses:     []string{soqlAccountsResponse(), `{"message":"boom"}`},
		queryStatuses: []int{0, http.StatusInternalServerError},
	}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", "acme.com")
	require.Error(t, err, "a failed alias search must not read as no candidates")
	assert.False(t, ok)
	assert.Nil(t, org)
	assert.Equal(t, 2, tr.queryCalls, "a failed alias search must stop before the website/name tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_ContextCancelledDuringAliasSearchStopsReRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &cancelOnSearchTransport{cancel: cancel, seqQueryTransport: seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(batchParentSFID(32)),
	}}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(ctx, "", "acme.com")
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, ok)
	assert.Nil(t, org)
	assert.Equal(t, 2, tr.queryCalls, "a context cancelled during the alias search must stop before the Id re-read")
}

func TestAccountRepo_FindAccountByNameOrWebsite_WebsiteSubstringVerifiesExactDomain(t *testing.T) {
	t.Parallel()

	// LIKE '%hat.com%' would also match "redhat.com" at the SOQL layer; verify
	// that the exact-hostname check in the verify callback rejects it.
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain: no match
		soslSearchResponse(),   // domain_alias: no match
		soqlAccountsResponse(soqlAccountRecord(batchParentSFID(3), "Red Hat", "https://redhat.com", "", "")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "hat.com")
	require.NoError(t, err)
	assert.False(t, ok, "redhat.com must not match a lookup for hat.com")
	assert.Nil(t, org)
}

func TestAccountRepo_FindAccountByNameOrWebsite_AmbiguousStopsEscalation(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(
			soqlAccountRecord(batchParentSFID(4), "Acme US", "", "acme.com", ""),
			soqlAccountRecord(batchParentSFID(5), "Acme EU", "", "acme.com", ""),
		),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme US", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "ambiguous primary_domain match must not fall through to the name tier")
	assert.Nil(t, org)
	assert.Equal(t, 1, tr.queryCalls, "ambiguity at the first tier must stop resolution immediately")
}

func TestAccountRepo_FindAccountByNameOrWebsite_IneligibleAccountsExcluded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		records   []string
		wantOK    bool
		wantName  string
		wantCalls int
	}{
		{
			name: "only ineligible match falls through to no match",
			records: []string{
				soqlAccountRecordWithType(batchParentSFID(20), "Acme Project", "", "acme.com", "", "Project_Account"),
			},
			wantOK:    false,
			wantCalls: 4,
		},
		{
			name: "ineligible duplicate does not make an eligible match ambiguous",
			records: []string{
				soqlAccountRecord(batchParentSFID(21), "Acme Corp", "", "acme.com", ""),
				soqlAccountRecordWithType(batchParentSFID(22), "Acme Person", "", "acme.com", "", ""),
			},
			wantOK:    true,
			wantName:  "Acme Corp",
			wantCalls: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &seqQueryTransport{responses: []string{soqlAccountsResponse(tc.records...)}}
			repo := NewAccountRepo(fakeSalesforce(t, tr), true)

			org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", "acme.com")
			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.NotNil(t, org)
				assert.Equal(t, tc.wantName, org.Name)
			} else {
				assert.Nil(t, org)
			}
			assert.Equal(t, tc.wantCalls, tr.queryCalls)
		})
	}
}

func TestAccountRepo_FindAccountByNameOrWebsite_PrimaryDomainQueryIncludesWWWForm(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(12)
	tr := &predicateAssertingTransport{
		wantSubstr: "'www.acme.com'",
		response:   soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "www.acme.com", "")),
	}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok, "primary_domain SOQL predicate must include the www.-prefixed stored form, not just the folded form")
	assert.Equal(t, "Acme Corp", org.Name)
}

// TestAccountRepo_FindAccountByNameOrWebsite_PrimaryDomainMatchIgnoresCaseAndWWW
// exercises the in-process fold-aware verify callback only; it uses an
// unconditional fake transport, so it does not validate the SOQL predicate
// itself (see PrimaryDomainQueryIncludesWWWForm above for that).
func TestAccountRepo_FindAccountByNameOrWebsite_PrimaryDomainMatchIgnoresCaseAndWWW(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(10)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "www.ACME.com", "")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok, "stored primary domain www.ACME.com must match requested acme.com")
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasMatchIgnoresCaseAndWWW(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(11)
	tr := &longTextFilterTransport{seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(sfid),
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "other.com", "WWW.Acme.COM")),
	}}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok, "stored domain alias WWW.Acme.COM must match requested acme.com")
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_SaturatedRawPageIsAmbiguous(t *testing.T) {
	t.Parallel()

	// The domain_alias SOSL search matches the domain in any searchable field,
	// so it can return up to aliasSearchLimit Ids before exact-alias
	// verification narrows them down. A page at the limit cannot prove that no
	// further match exists beyond the cutoff, so it must be treated as
	// ambiguous rather than resolved from whichever hits landed within it.
	ids := make([]string, aliasSearchLimit)
	for i := range ids {
		ids[i] = batchParentSFID(100 + i)
	}
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(ids...),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Irrelevant Co", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "a saturated domain_alias search must not resolve")
	assert.Nil(t, org)
	assert.Equal(t, 2, tr.queryCalls, "a saturated domain_alias search must stop before the Id re-read and the website/name tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_SaturatedWebsitePageIsAmbiguous(t *testing.T) {
	t.Parallel()

	// A LIKE substring query can return up to accountLikeMatchPageSize raw hits
	// before exact-hostname verification narrows them down. A page saturated at
	// the limit cannot prove that no further match exists beyond the cutoff,
	// so the check must fire before verify, whatever the post-filter count.
	records := make([]string, accountLikeMatchPageSize)
	for i := range records {
		records[i] = soqlAccountRecord(batchParentSFID(400+i), "Irrelevant Co", "https://notacme.com", "", "")
	}
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(),   // domain_alias tier: no candidates
		soqlAccountsResponse(records...),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Irrelevant Co", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "a saturated website page must not resolve")
	assert.Nil(t, org)
	assert.Equal(t, 3, tr.queryCalls, "a saturated website page must stop before the name tier")
}

func TestAccountRepo_FindAccountByNameOrWebsite_PlaceholderWebsiteSkipsDomainTiers(t *testing.T) {
	t.Parallel()

	for _, website := range []string{"n/a", "-", ".", "a.", "www..", "www.", "https://www."} {
		t.Run(website, func(t *testing.T) {
			t.Parallel()
			sfid := batchParentSFID(34)
			tr := &seqQueryTransport{responses: []string{
				soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "", "")),
			}}
			repo := NewAccountRepo(fakeSalesforce(t, tr), true)

			org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", website)
			require.NoError(t, err, "SOSL rejects such terms; the lookup must not send them")
			require.True(t, ok, "the name tier must still run")
			assert.Equal(t, "Acme Corp", org.Name)
			require.Len(t, tr.queries, 1, "a placeholder must not reach the domain tiers, whose LIKE could saturate")
			assert.Contains(t, tr.queries[0], "Name = 'Acme Corp'")
		})
	}
}

func TestAccountRepo_FindAccountByNameOrWebsite_AliasVerifyDropsOtherSearchHits(t *testing.T) {
	t.Parallel()

	// IN ALL FIELDS also returns Accounts that mention the domain elsewhere;
	// exact-alias verify must drop them rather than count them as owners.
	owner, other := batchParentSFID(36), batchParentSFID(37)
	otherRecord := soqlAccountRecord(other, "Other Co", "", "other.com", "notacme.com")

	t.Run("alongside a true owner", func(t *testing.T) {
		t.Parallel()
		tr := &seqQueryTransport{responses: []string{
			soqlAccountsResponse(), // primary_domain tier: no match
			soslSearchResponse(owner, other),
			soqlAccountsResponse(soqlAccountRecord(owner, "Acme Corp", "", "acme.io", "acme.com"), otherRecord),
		}}
		repo := NewAccountRepo(fakeSalesforce(t, tr), true)

		org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
		require.NoError(t, err)
		require.True(t, ok, "an unrelated hit must not make the alias ambiguous")
		assert.Equal(t, "Acme Corp", org.Name)
	})

	t.Run("with no true owner", func(t *testing.T) {
		t.Parallel()
		tr := &seqQueryTransport{responses: []string{
			soqlAccountsResponse(), // primary_domain tier: no match
			soslSearchResponse(other),
			soqlAccountsResponse(otherRecord),
			soqlAccountsResponse(soqlAccountRecord(owner, "Acme Corp", "https://acme.com", "", "")),
		}}
		repo := NewAccountRepo(fakeSalesforce(t, tr), true)

		org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
		require.NoError(t, err)
		require.True(t, ok, "with no verified alias owner the lookup must fall through to the website tier")
		assert.Equal(t, "Acme Corp", org.Name)
		assert.Equal(t, 4, tr.queryCalls)
	})
}

func TestAccountRepo_FindAccountByNameOrWebsite_AliasSearchBelowLimitReReads(t *testing.T) {
	t.Parallel()

	// One Id under the limit is not saturated and must be re-read. The re-read
	// carries the membership filter (Company Accounts excluded) and every Id,
	// so it is the longest request; it must stay under Salesforce's 16,384
	// character request URI limit.
	ids := make([]string, aliasSearchLimit-1)
	for i := range ids {
		ids[i] = batchParentSFID(1000 + i)
	}
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(ids...),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), false)

	_, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok)
	i := slices.IndexFunc(tr.queries, func(q string) bool { return strings.Contains(q, "Id IN ('") })
	require.GreaterOrEqual(t, i, 0, "a search under the limit must be re-read")
	reRead := tr.queries[i]
	assert.Contains(t, reRead, "SELECT AccountId FROM Asset")
	assert.Less(t, len("/services/data/v63.0/query?q="+url.QueryEscape(reRead)), 16384)
}

func TestAccountRepo_FindAccountByNameOrWebsite_WWWSingleLabelDomainSearchesDottedForm(t *testing.T) {
	t.Parallel()

	// www.com is a valid stored alias that folds, like the website, to the
	// dot-less "com"; the search must use the dotted form rather than skip.
	sfid := batchParentSFID(35)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soslSearchResponse(sfid),
		soqlAccountsResponse(soqlAccountRecord(sfid, "WWW Corp", "", "other.com", "www.com")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "https://www.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "WWW Corp", org.Name)
	require.Equal(t, 3, tr.queryCalls)
	assert.Contains(t, tr.queries[1], `FIND {"www.com"}`)
}

func TestAccountRepo_FindAccountByNameOrWebsite_NonSaturatedPageWithNextPageTokenIsAmbiguous(t *testing.T) {
	t.Parallel()

	// A page can report done:false (non-empty nextRecordsUrl) with fewer
	// records than the SOQL LIMIT if Salesforce's batch size ever ends up
	// smaller than the requested limit. len(Records) < limit alone cannot
	// prove this was the final page in that case, so NextPageToken must be
	// checked too, not just saturation.
	record := soqlAccountRecord(batchParentSFID(13), "Acme US", "", "acme.com", "")
	tr := &seqQueryTransport{responses: []string{
		fmt.Sprintf(`{"totalSize":2,"done":false,"nextRecordsUrl":"/services/data/v63.0/query/01gXXX-2000","records":[%s]}`, record),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "a page with a non-empty NextPageToken must not resolve, even with len(Records) < limit")
	assert.Nil(t, org)
	assert.Equal(t, 1, tr.queryCalls, "ambiguity from an unconsumed NextPageToken must stop before later tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_NameFallback(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(6)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "", "")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", "")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_NotFound(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Nonexistent Org", "nowhere.example")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, org)
}

func TestAccountRepo_FindAccountByNameOrWebsite_EmptyInputsReturnNotFound(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, org)
	assert.Equal(t, 0, tr.queryCalls, "no SOQL query should be issued when both name and website are empty")
}

func TestAccountRepo_FindAccountByNameOrWebsite_QueryErrorPropagates(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{
		responses:     []string{`{"message":"boom"}`},
		queryStatuses: []int{500},
	}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Nil(t, org)
}

func TestAccountRepo_FindAccountByNameOrWebsite_ExpiredContextStopsBeforeLaterTiers(t *testing.T) {
	t.Parallel()

	// go-salesforce's QueryPage never binds ctx to the underlying HTTP
	// request, so an already-expired ctx cannot abort the in-flight
	// primary_domain call, but it must stop the domain_alias/website/name
	// tiers from starting afterward: letting each subsequent tier consume a
	// full httpTimeout after the caller's deadline has passed is how the
	// handler's 30s budget gets blown across up to five sequential requests.
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr), true)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	org, ok, err := repo.FindAccountByNameOrWebsite(ctx, "", "acme.com")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, ok)
	assert.Nil(t, org)
	assert.Equal(t, 1, tr.queryCalls, "an expired context must stop before the domain_alias/website/name tiers")
}

func TestWebsiteDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"acme.com", "acme.com"},
		{"https://acme.com", "acme.com"},
		{"https://www.acme.com/path", "acme.com"},
		{"http://WWW.Acme.COM", "acme.com"},
		{"not a url", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, websiteDomain(tc.in), "websiteDomain(%q)", tc.in)
	}
}
