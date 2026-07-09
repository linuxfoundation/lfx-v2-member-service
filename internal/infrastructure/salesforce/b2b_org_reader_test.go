// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// canonicalAccountSFID is a real 15- or 18-char Salesforce Account.Id used throughout
// reader and writer tests. Taken from the sfuuid test suite fixture.
const canonicalAccountSFID = "001B000000IqhSL"

// canonicalAccountJSON is a fully-populated Salesforce Account sObject REST
// response body. All fields included in b2bOrgFields are present.
const canonicalAccountJSON = `{
	"Id":"001B000000IqhSL",
	"Name":"Linux Foundation",
	"Logo_URL__c":"https://linuxfoundation.org/logo.png",
	"Website":"https://linuxfoundation.org",
	"Account_Domain__c":"linuxfoundation.org",
	"Domain_Alias__c":"lf.org, thelinuxfoundation.org",
	"Description":"Supporting open source ecosystems.",
	"Phone":"+1-415-723-9709",
	"ParentId":null,
	"Industry":"Technology",
	"Sector__c":"Non-Profit",
	"CrunchBase_URL__c":"https://www.crunchbase.com/organization/linux-foundation",
	"NumberOfEmployees":200,
	"LF_Membership_Status__c":"Active",
	"Slug__c":"linux-foundation",
	"CreatedDate":"2020-01-15T10:30:00.000+0000",
	"LastModifiedDate":"2024-06-01T08:00:00.000+0000",
	"SystemModstamp":"2024-06-01T08:00:00.000+0000"
}`

// TestSobjectAccountToB2BOrg_FixtureEquivalence verifies that
// sobjectAccountToB2BOrg (sObject REST path) and convertSOQLToB2BOrg (SOQL
// path) produce identical model.B2BOrg values for every field that both
// converters handle. The sObject path additionally populates Slug (Slug__c),
// which accountsSOQLBase does not select — that divergence is expected and
// asserted explicitly below.
func TestSobjectAccountToB2BOrg_FixtureEquivalence(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	// ── sObject path ──────────────────────────────────────────────────────────
	var sObjRaw sobjectAccount
	require.NoError(t, json.Unmarshal([]byte(canonicalAccountJSON), &sObjRaw))
	sObjOrg, err := sobjectAccountToB2BOrg(context.Background(), &sObjRaw, uid)
	require.NoError(t, err)
	require.NotNil(t, sObjOrg)

	// ── SOQL path ─────────────────────────────────────────────────────────────
	// Populate soqlAccount with the same values (Slug__c excluded — accountsSOQLBase
	// does not select it).
	crunchURL := "https://www.crunchbase.com/organization/linux-foundation"
	var empCount int64 = 200
	logoURL := "https://linuxfoundation.org/logo.png"
	website := "https://linuxfoundation.org"
	primaryDomain := "linuxfoundation.org"
	domainAlias := "lf.org, thelinuxfoundation.org"
	description := "Supporting open source ecosystems."
	phone := "+1-415-723-9709"
	industry := "Technology"
	sector := "Non-Profit"
	status := "Active"

	soqlRaw := soqlAccount{
		ID:                canonicalAccountSFID,
		Name:              "Linux Foundation",
		LogoURL:           &logoURL,
		Website:           &website,
		PrimaryDomain:     &primaryDomain,
		DomainAlias:       &domainAlias,
		Description:       &description,
		Phone:             &phone,
		Industry:          &industry,
		Sector:            &sector,
		CrunchBaseURL:     &crunchURL,
		NumberOfEmployees: &empCount,
		Status:            &status,
		CreatedDate:       "2020-01-15T10:30:00.000+0000",
		LastModifiedDate:  "2024-06-01T08:00:00.000+0000",
	}
	soqlOrg, err := convertSOQLToB2BOrg(context.Background(), soqlRaw)
	require.NoError(t, err)
	require.NotNil(t, soqlOrg)

	// ── compare shared fields ─────────────────────────────────────────────────
	assert.Equal(t, soqlOrg.UID, sObjOrg.UID, "UID")
	assert.Equal(t, soqlOrg.SFID, sObjOrg.SFID, "SFID")
	assert.Equal(t, soqlOrg.Name, sObjOrg.Name, "Name")
	assert.Equal(t, soqlOrg.LogoURL, sObjOrg.LogoURL, "LogoURL")
	assert.Equal(t, soqlOrg.Website, sObjOrg.Website, "Website")
	assert.Equal(t, soqlOrg.PrimaryDomain, sObjOrg.PrimaryDomain, "PrimaryDomain")
	assert.Equal(t, soqlOrg.DomainAliases, sObjOrg.DomainAliases, "DomainAliases")
	assert.Equal(t, soqlOrg.Description, sObjOrg.Description, "Description")
	assert.Equal(t, soqlOrg.Phone, sObjOrg.Phone, "Phone")
	assert.Equal(t, soqlOrg.Industry, sObjOrg.Industry, "Industry")
	assert.Equal(t, soqlOrg.Sector, sObjOrg.Sector, "Sector")
	assert.Equal(t, soqlOrg.CrunchBaseURL, sObjOrg.CrunchBaseURL, "CrunchBaseURL")
	assert.Equal(t, soqlOrg.NumberOfEmployees, sObjOrg.NumberOfEmployees, "NumberOfEmployees")
	assert.Equal(t, soqlOrg.Status, sObjOrg.Status, "Status")
	assert.Equal(t, soqlOrg.CreatedAt.UTC(), sObjOrg.CreatedAt.UTC(), "CreatedAt")
	assert.Equal(t, soqlOrg.UpdatedAt.UTC(), sObjOrg.UpdatedAt.UTC(), "UpdatedAt")

	// Slug is only populated by the sObject path (Slug__c absent from accountsSOQLBase).
	assert.Equal(t, "linux-foundation", sObjOrg.Slug, "sObject path must populate Slug")
	assert.Empty(t, soqlOrg.Slug, "SOQL path must leave Slug empty (not in accountsSOQLBase)")
}

// TestB2BOrgReader_GetB2BOrg_Happy verifies that GetB2BOrg returns a fully-
// populated B2BOrg when Salesforce responds with 200 OK.
func TestB2BOrgReader_GetB2BOrg_Happy(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	transport := newRoutingTransport(fakeResponse(http.StatusOK, canonicalAccountJSON, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	org, err := reader.GetB2BOrg(context.Background(), uid)

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, uid, org.UID)
	assert.Equal(t, "Linux Foundation", org.Name)
	assert.Equal(t, "linux-foundation", org.Slug)
	assert.Equal(t, "Technology", org.Industry)
	assert.Equal(t, "Non-Profit", org.Sector)
	assert.Equal(t, "Active", org.Status)
	require.NotNil(t, org.NumberOfEmployees)
	assert.Equal(t, int64(200), *org.NumberOfEmployees)
	assert.Equal(t, []string{"lf.org", "thelinuxfoundation.org"}, org.DomainAliases)
}

// TestB2BOrgReader_GetB2BOrg_NotFound verifies that GetB2BOrg propagates a
// NotFound error when Salesforce returns 404.
func TestB2BOrgReader_GetB2BOrg_NotFound(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	transport := newRoutingTransport(fakeResponse(http.StatusNotFound,
		`[{"errorCode":"NOT_FOUND","message":"The requested resource does not exist"}]`,
		nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	org, err := reader.GetB2BOrg(context.Background(), uid)

	require.Error(t, err)
	assert.Nil(t, org)

	var notFound errs.NotFound
	assert.True(t, errors.As(err, &notFound),
		"expected NotFound error, got %T: %v", err, err)
}

// parentAccountSFID is the Salesforce Account.Id used as the parent org in
// parent-detail tests. It is a different ID from canonicalAccountSFID.
const parentAccountSFID = "003B0000001ckSl"

// canonicalAccountWithParentJSON is a fully-populated Salesforce Account sObject
// REST response that includes a populated ParentId (no nested Parent.Name —
// sObject REST cannot return relationship sub-fields).
const canonicalAccountWithParentJSON = `{
	"Id":"001B000000IqhSL",
	"Name":"Linux Foundation",
	"Logo_URL__c":"https://linuxfoundation.org/logo.png",
	"Website":"https://linuxfoundation.org",
	"Account_Domain__c":"linuxfoundation.org",
	"Domain_Alias__c":null,
	"Description":"Supporting open source ecosystems.",
	"Phone":"+1-415-723-9709",
	"ParentId":"003B0000001ckSl",
	"Industry":"Technology",
	"Sector__c":"Non-Profit",
	"CrunchBase_URL__c":null,
	"NumberOfEmployees":200,
	"LF_Membership_Status__c":"Active",
	"Slug__c":"linux-foundation",
	"CreatedDate":"2020-01-15T10:30:00.000+0000",
	"LastModifiedDate":"2024-06-01T08:00:00.000+0000",
	"SystemModstamp":"2024-06-01T08:00:00.000+0000"
}`

// parentAccountJSON is a minimal Account sObject REST response for the parent org.
const parentAccountJSON = `{
	"Id":"003B0000001ckSl",
	"Name":"Global Parent Org",
	"Logo_URL__c":"https://parent.org/logo.png",
	"CreatedDate":"2018-01-01T00:00:00.000+0000",
	"LastModifiedDate":"2024-01-01T00:00:00.000+0000",
	"SystemModstamp":"2024-01-01T00:00:00.000+0000"
}`

// TestSobjectAccountToB2BOrg_WithParent verifies that when sobjectAccount.Parent
// is populated, sobjectAccountToB2BOrg maps it to B2BOrg.ParentDetail.
func TestSobjectAccountToB2BOrg_WithParent(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	parentUID, err := sfuuid.Normalize18(parentAccountSFID)
	require.NoError(t, err)

	parentSFID := parentAccountSFID // local copy so we can take its address
	logoURL := "https://parent.org/logo.png"
	raw := sobjectAccount{
		ID:       canonicalAccountSFID,
		Name:     "Linux Foundation",
		ParentID: &parentSFID,
		Parent: &sobjectAccountParent{
			ID:      parentAccountSFID,
			Name:    "Global Parent Org",
			LogoURL: &logoURL,
		},
		CreatedDate:      "2020-01-15T10:30:00.000+0000",
		LastModifiedDate: "2024-06-01T08:00:00.000+0000",
		SystemModstamp:   "2024-06-01T08:00:00.000+0000",
	}

	org, err := sobjectAccountToB2BOrg(context.Background(), &raw, uid)

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, parentUID, org.ParentUID, "ParentUID must be derived from raw.ParentID")
	require.NotNil(t, org.ParentDetail, "ParentDetail must be populated when Parent sub-object is present")
	assert.Equal(t, parentUID, org.ParentDetail.UID, "ParentDetail.UID must match parent SFID")
	assert.Equal(t, "Global Parent Org", org.ParentDetail.Name)
	require.NotNil(t, org.ParentDetail.LogoURL)
	assert.Equal(t, "https://parent.org/logo.png", *org.ParentDetail.LogoURL)
}

// TestSobjectAccountToB2BOrg_ParentNoLogo verifies that missing logo on parent
// leaves ParentDetail.LogoURL nil rather than panicking.
func TestSobjectAccountToB2BOrg_ParentNoLogo(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	parentSFID2 := parentAccountSFID
	raw := sobjectAccount{
		ID:       canonicalAccountSFID,
		Name:     "Linux Foundation",
		ParentID: &parentSFID2,
		Parent: &sobjectAccountParent{
			ID:      parentAccountSFID,
			Name:    "No Logo Parent",
			LogoURL: nil,
		},
		CreatedDate:      "2020-01-15T10:30:00.000+0000",
		LastModifiedDate: "2024-06-01T08:00:00.000+0000",
		SystemModstamp:   "2024-06-01T08:00:00.000+0000",
	}

	org, err := sobjectAccountToB2BOrg(context.Background(), &raw, uid)

	require.NoError(t, err)
	require.NotNil(t, org.ParentDetail)
	assert.Nil(t, org.ParentDetail.LogoURL, "nil logo on parent must remain nil in ParentDetail")
}

// TestSobjectAccountToB2BOrg_NilParent verifies that when Parent is nil,
// ParentDetail remains nil even when ParentID is set (incomplete fetch scenario).
func TestSobjectAccountToB2BOrg_NilParent(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	parentSFID3 := parentAccountSFID
	raw := sobjectAccount{
		ID:               canonicalAccountSFID,
		Name:             "Linux Foundation",
		ParentID:         &parentSFID3,
		Parent:           nil, // not fetched
		CreatedDate:      "2020-01-15T10:30:00.000+0000",
		LastModifiedDate: "2024-06-01T08:00:00.000+0000",
		SystemModstamp:   "2024-06-01T08:00:00.000+0000",
	}

	org, err := sobjectAccountToB2BOrg(context.Background(), &raw, uid)

	require.NoError(t, err)
	assert.NotEmpty(t, org.ParentUID, "ParentUID must still be set from ParentID")
	assert.Nil(t, org.ParentDetail, "ParentDetail must be nil when Parent sub-object is absent")
}

// TestConvertSOQLToB2BOrg_WithParent verifies that convertSOQLToB2BOrg populates
// B2BOrg.ParentDetail from the Parent sub-object in a soqlAccount.
func TestConvertSOQLToB2BOrg_WithParent(t *testing.T) {
	t.Parallel()

	parentUID, err := sfuuid.Normalize18(parentAccountSFID)
	require.NoError(t, err)

	logoURL := "https://parent.org/logo.png"
	parentSFID := parentAccountSFID
	acc := soqlAccount{
		ID:       canonicalAccountSFID,
		Name:     "Linux Foundation",
		ParentID: &parentSFID,
		Parent: &soqlAccountParent{
			ID:      parentAccountSFID,
			Name:    "Global Parent Org",
			LogoURL: &logoURL,
		},
		CreatedDate:      "2020-01-15T10:30:00.000+0000",
		LastModifiedDate: "2024-06-01T08:00:00.000+0000",
	}

	org, err := convertSOQLToB2BOrg(context.Background(), acc)

	require.NoError(t, err)
	require.NotNil(t, org)
	require.NotNil(t, org.ParentDetail, "ParentDetail must be populated from SOQL Parent sub-object")
	assert.Equal(t, parentUID, org.ParentDetail.UID)
	assert.Equal(t, "Global Parent Org", org.ParentDetail.Name)
	require.NotNil(t, org.ParentDetail.LogoURL)
	assert.Equal(t, logoURL, *org.ParentDetail.LogoURL)
}

// TestConvertSOQLToB2BOrg_NilParent verifies that a nil SOQL Parent sub-object
// leaves ParentDetail nil.
func TestConvertSOQLToB2BOrg_NilParent(t *testing.T) {
	t.Parallel()

	acc := soqlAccount{
		ID:               canonicalAccountSFID,
		Name:             "Linux Foundation",
		Parent:           nil,
		CreatedDate:      "2020-01-15T10:30:00.000+0000",
		LastModifiedDate: "2024-06-01T08:00:00.000+0000",
	}

	org, err := convertSOQLToB2BOrg(context.Background(), acc)

	require.NoError(t, err)
	assert.Nil(t, org.ParentDetail)
}

// TestB2BOrgReader_GetB2BOrg_WithParent verifies that GetB2BOrg fetches parent
// details via a secondary sObject call and populates B2BOrg.ParentDetail.
func TestB2BOrgReader_GetB2BOrg_WithParent(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	parentUID, err := sfuuid.Normalize18(parentAccountSFID)
	require.NoError(t, err)

	// Route the first sObject call (Account fetch) to canonicalAccountWithParentJSON,
	// and the second (parent Account fetch) to parentAccountJSON. Since both paths
	// contain "/sobjects/Account/", we use a second route for the parent SFID.
	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))
	transport.route(parentAccountSFID, fakeResponse(http.StatusOK, parentAccountJSON, nil))
	transport.route("/sobjects/Account/", fakeResponse(http.StatusOK, canonicalAccountWithParentJSON, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	org, err := reader.GetB2BOrg(context.Background(), uid)

	require.NoError(t, err)
	require.NotNil(t, org)
	assert.Equal(t, uid, org.UID)
	assert.Equal(t, parentUID, org.ParentUID, "ParentUID must be derived from Account.ParentId")
	require.NotNil(t, org.ParentDetail, "ParentDetail must be populated from secondary parent fetch")
	assert.Equal(t, parentUID, org.ParentDetail.UID)
	assert.Equal(t, "Global Parent Org", org.ParentDetail.Name)
	require.NotNil(t, org.ParentDetail.LogoURL)
	assert.Equal(t, "https://parent.org/logo.png", *org.ParentDetail.LogoURL)
}

// TestB2BOrgReader_GetB2BOrg_InvalidUID verifies that GetB2BOrg returns a
// Validation error when the provided UID is not a valid Salesforce ID.
func TestB2BOrgReader_GetB2BOrg_InvalidUID(t *testing.T) {
	t.Parallel()

	transport := &routingTransport{}
	transport.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))
	client := &SObjectClient{sf: fakeSalesforce(t, transport), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	org, err := reader.GetB2BOrg(context.Background(), "not-a-valid-uid")

	require.Error(t, err)
	assert.Nil(t, org)
}

// ─── Cache key collision regression tests (LFXV2-2654) ────────────────────────
//
// These tests prove that fetchParentAccountDetail, FetchB2BOrg, and FetchAccount
// no longer share a single sObject cache entry per Account SFID. Before the fix,
// all three built their cache key as sobjectCacheKey(sobjectKeyPrefixB2BOrg, sfid)
// regardless of which (different) field list they requested, so whichever call
// populated the entry first "won" and poisoned the others indefinitely.

// TestSObjectClient_CacheKeyIsolation_ParentBriefThenFullOrg reproduces the
// original bug scenario: an Account is first fetched narrowly as another
// Account's parent, then fetched directly as a full B2BOrg. The full fetch must
// issue a fresh HTTP request and return every field in b2bOrgFields, unaffected
// by the earlier narrow fetch.
func TestSObjectClient_CacheKeyIsolation_ParentBriefThenFullOrg(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	const narrowJSON = `{
		"Id":"001B000000IqhSL",
		"Name":"Linux Foundation",
		"Logo_URL__c":"https://linuxfoundation.org/logo.png"
	}`

	callCount := 0
	rt := &countingTransport{
		callCount:  &callCount,
		firstResp:  fakeResponse(http.StatusOK, narrowJSON, nil),
		retryResp:  fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		limitsResp: fakeResponse(http.StatusOK, `{}`, nil),
	}
	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	// Step 1: narrow parent-detail fetch, as fetchParentAccountDetail performs
	// when processing some other Account's ParentId. Populates
	// "b2b_org_parent_brief.{uid}".
	_, err = client.fetchParentAccountDetail(context.Background(), canonicalAccountSFID)
	require.NoError(t, err)

	// Step 2: full org fetch for the same Account. Must be a cache miss on
	// "b2b_org.{uid}" and issue a fresh fetch rather than reusing the narrow entry.
	org, err := reader.GetB2BOrg(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, org)

	assert.Equal(t, 2, callCount, "full org fetch must issue a fresh HTTP request, not reuse the narrow cache entry")
	assert.Equal(t, "Technology", org.Industry, "full fetch must return fields absent from the narrow parent-detail response")
	require.NotNil(t, org.NumberOfEmployees)
	assert.Equal(t, int64(200), *org.NumberOfEmployees)
	assert.Equal(t, "Supporting open source ecosystems.", org.Description)
}

// TestSObjectClient_CacheKeyIsolation_FullOrgThenParentBrief verifies the
// reverse order: once a full org fetch has populated "b2b_org.{uid}", a later
// narrow parent-detail fetch for the same Account must not overwrite it.
func TestSObjectClient_CacheKeyIsolation_FullOrgThenParentBrief(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	const narrowJSON = `{
		"Id":"001B000000IqhSL",
		"Name":"Linux Foundation",
		"Logo_URL__c":"https://linuxfoundation.org/logo.png"
	}`

	callCount := 0
	rt := &countingTransport{
		callCount:  &callCount,
		firstResp:  fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		retryResp:  fakeResponse(http.StatusOK, narrowJSON, nil),
		limitsResp: fakeResponse(http.StatusOK, `{}`, nil),
	}
	cache := newMemCache()
	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: cache}
	reader := NewB2BOrgReader(client, nil)

	// Step 1: full org fetch populates "b2b_org.{uid}".
	org1, err := reader.GetB2BOrg(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, org1)

	// Step 2: narrow parent-detail fetch for the same Account populates the
	// distinct "b2b_org_parent_brief.{uid}" key.
	_, err = client.fetchParentAccountDetail(context.Background(), canonicalAccountSFID)
	require.NoError(t, err)

	// The full org cache entry must be untouched by the narrow fetch.
	stored, err := cache.Get(context.Background(), sobjectCacheKey(sobjectKeyPrefixB2BOrg, uid))
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.JSONEq(t, canonicalAccountJSON, string(stored.Body),
		"full org cache entry must not be overwritten by a later narrow parent-detail fetch")
}

// TestSObjectClient_CacheKeyIsolation_FlatAccountThenFullOrg verifies that
// FetchAccount's narrower accountFields fetch and FetchB2BOrg's full fetch do
// not share a cache entry for the same Account SFID.
func TestSObjectClient_CacheKeyIsolation_FlatAccountThenFullOrg(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(canonicalAccountSFID)
	require.NoError(t, err)

	const flatJSON = `{
		"Id":"001B000000IqhSL",
		"Name":"Linux Foundation",
		"Logo_URL__c":"https://linuxfoundation.org/logo.png",
		"Website":"https://linuxfoundation.org",
		"CreatedDate":"2020-01-15T10:30:00.000+0000",
		"LastModifiedDate":"2024-06-01T08:00:00.000+0000",
		"SystemModstamp":"2024-06-01T08:00:00.000+0000"
	}`

	callCount := 0
	rt := &countingTransport{
		callCount:  &callCount,
		firstResp:  fakeResponse(http.StatusOK, flatJSON, nil),
		retryResp:  fakeResponse(http.StatusOK, canonicalAccountJSON, nil),
		limitsResp: fakeResponse(http.StatusOK, `{}`, nil),
	}
	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewB2BOrgReader(client, nil)

	// Step 1: FetchAccount populates "b2b_org_flat.{uid}".
	account, err := client.FetchAccount(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, account)
	assert.Equal(t, "Linux Foundation", account.Name)

	// Step 2: FetchB2BOrg for the same UID must not reuse FetchAccount's entry.
	org, err := reader.GetB2BOrg(context.Background(), uid)
	require.NoError(t, err)
	require.NotNil(t, org)

	assert.Equal(t, 2, callCount, "FetchB2BOrg must issue a fresh HTTP request, not reuse FetchAccount's cache entry")
	assert.Equal(t, "Technology", org.Industry, "full fetch must return fields absent from FetchAccount's narrow response")
}
