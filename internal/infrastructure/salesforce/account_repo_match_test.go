// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
