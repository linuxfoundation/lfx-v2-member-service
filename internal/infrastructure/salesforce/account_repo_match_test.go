// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// soqlAccount JSON tags, for use in a SOQL query response body.
func soqlAccountRecord(id, name, website, primaryDomain, domainAlias string) string {
	return fmt.Sprintf(
		`{"Id":%q,"Name":%q,"Website":%q,"Account_Domain__c":%q,"Domain_Alias__c":%q}`,
		id, name, website, primaryDomain, domainAlias,
	)
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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "https://acme.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
	assert.Equal(t, 1, tr.queryCalls, "primary_domain tier match should not fall through to later tiers")
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasMatch(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(2)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "other.com", "acme.com, acme.io")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
	assert.Equal(t, 2, tr.queryCalls)
}

func TestAccountRepo_FindAccountByNameOrWebsite_WebsiteSubstringVerifiesExactDomain(t *testing.T) {
	t.Parallel()

	// LIKE '%hat.com%' would also match "redhat.com" at the SOQL layer; verify
	// that the exact-hostname check in the verify callback rejects it.
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain: no match
		soqlAccountsResponse(), // domain_alias: no match
		soqlAccountsResponse(soqlAccountRecord(batchParentSFID(3), "Red Hat", "https://redhat.com", "", "")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme US", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "ambiguous primary_domain match must not fall through to the name tier")
	assert.Nil(t, org)
	assert.Equal(t, 1, tr.queryCalls, "ambiguity at the first tier must stop resolution immediately")
}

func TestAccountRepo_FindAccountByNameOrWebsite_PrimaryDomainQueryIncludesWWWForm(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(12)
	tr := &predicateAssertingTransport{
		wantSubstr: "'www.acme.com'",
		response:   soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "www.acme.com", "")),
	}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok, "stored primary domain www.ACME.com must match requested acme.com")
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_DomainAliasMatchIgnoresCaseAndWWW(t *testing.T) {
	t.Parallel()

	sfid := batchParentSFID(11)
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soqlAccountsResponse(soqlAccountRecord(sfid, "Acme Corp", "", "other.com", "WWW.Acme.COM")),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.NoError(t, err)
	require.True(t, ok, "stored domain alias WWW.Acme.COM must match requested acme.com")
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_SaturatedRawPageIsAmbiguous(t *testing.T) {
	t.Parallel()

	// A LIKE substring query can return up to accountLikeMatchPageSize raw hits
	// before exact-hostname verification narrows them down. A page saturated at
	// the limit (no ORDER BY in the query) cannot prove that no further match
	// exists beyond the cutoff, so it must be treated as ambiguous rather than
	// resolved from whichever hits happened to land within the page.
	records := make([]string, accountLikeMatchPageSize)
	for i := range records {
		// None of these verify-match "acme.com" exactly: only the raw SOQL
		// LIKE predicate matched. The saturation check must fire before
		// verify-filtering ever runs, regardless of the post-filter count.
		records[i] = soqlAccountRecord(batchParentSFID(100+i), "Irrelevant Co", "", "", "notacme.com")
	}
	tr := &seqQueryTransport{responses: []string{
		soqlAccountsResponse(), // primary_domain tier: no match
		soqlAccountsResponse(records...),
	}}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Irrelevant Co", "acme.com")
	require.NoError(t, err)
	assert.False(t, ok, "a saturated raw domain_alias page must not resolve, even though no verified match survived")
	assert.Nil(t, org)
	assert.Equal(t, 2, tr.queryCalls, "ambiguity at domain_alias must stop before the website/name tiers")
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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Acme Corp", "")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "Acme Corp", org.Name)
}

func TestAccountRepo_FindAccountByNameOrWebsite_NotFound(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "Nonexistent Org", "nowhere.example")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, org)
}

func TestAccountRepo_FindAccountByNameOrWebsite_EmptyInputsReturnNotFound(t *testing.T) {
	t.Parallel()

	tr := &seqQueryTransport{}
	repo := NewAccountRepo(fakeSalesforce(t, tr))

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
	repo := NewAccountRepo(fakeSalesforce(t, tr))

	org, ok, err := repo.FindAccountByNameOrWebsite(context.Background(), "", "acme.com")
	require.Error(t, err)
	assert.False(t, ok)
	assert.Nil(t, org)
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
