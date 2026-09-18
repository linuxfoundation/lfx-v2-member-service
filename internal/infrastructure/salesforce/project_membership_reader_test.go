// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// TestAssembleProjectMembership_HappyPath verifies that AssembleProjectMembership
// fetches all 4 sObjects (Asset, Account, Product2, Project__c) and denormalises
// them into a single fully-populated ProjectMembership record.
func TestAssembleProjectMembership_HappyPath(t *testing.T) {
	t.Parallel()

	// Fixture data matching the specification.
	const (
		assetSFID    = "02iB0000009ABCDIA4"
		accountSFID  = "0012M00002FyYwwQAF"
		product2SFID = "01t2M000009ABCDQA4"
		projectSFID  = "a092M000001XYZAQA4"
	)

	assetJSON := `{
		"Id":"` + assetSFID + `",
		"Name":"Acme Corp Membership",
		"Status":"Active",
		"AccountId":"` + accountSFID + `",
		"Product2Id":"` + product2SFID + `",
		"Year__c":"2025",
		"Tier__c":"Gold",
		"RecordTypeId":null,
		"Auto_Renew__c":true,
		"Renewal_Type__c":"Annual",
		"Price":50000.0,
		"Annual_Full_Price__c":50000.0,
		"PaymentFrequency__c":"Annual",
		"PaymentTerms__c":null,
		"Agreement_Date__c":null,
		"PurchaseDate":"2025-01-01",
		"InstallDate":null,
		"UsageEndDate":"2025-12-31",
		"Projects__c":"` + projectSFID + `",
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"SystemModstamp":"2025-03-20T00:00:00.000+0000"
	}`

	accountJSON := `{
		"Id":"` + accountSFID + `",
		"Name":"Acme Corp",
		"Logo_URL__c":"https://acme.com/logo.png",
		"Website":"https://acme.com",
		"Account_Domain__c":"acme.com",
		"Domain_Alias__c":null,
		"Description":"A great company",
		"Phone":"+1-555-0100",
		"ParentId":null,
		"Industry":"Technology",
		"Sector__c":"Private",
		"CrunchBase_URL__c":null,
		"NumberOfEmployees":1000,
		"LF_Membership_Status__c":"Active",
		"CreatedDate":"2024-01-15T10:00:00.000+0000",
		"LastModifiedDate":"2024-01-15T10:00:00.000+0000",
		"SystemModstamp":"2024-01-15T10:00:00.000+0000"
	}`

	product2JSON := `{
		"Id":"` + product2SFID + `",
		"Name":"Gold Membership",
		"Family":"Membership",
		"Type__c":"Corporate",
		"Project__c":null,
		"CreatedDate":"2023-06-01T00:00:00.000+0000",
		"LastModifiedDate":"2023-06-01T00:00:00.000+0000",
		"SystemModstamp":"2023-06-01T00:00:00.000+0000"
	}`

	projectJSON := `{
		"Id":"` + projectSFID + `",
		"Name":"Linux Foundation",
		"Slug__c":"linux-foundation",
		"LastModifiedDate":"2024-03-20T00:00:00.000+0000"
	}`

	rt := &routingTransport{}
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusOK, assetJSON, nil))
	rt.route("/sobjects/Account/"+accountSFID, fakeResponse(http.StatusOK, accountJSON, nil))
	rt.route("/sobjects/Product2/"+product2SFID, fakeResponse(http.StatusOK, product2JSON, nil))
	rt.route("/sobjects/Project__c/"+projectSFID, fakeResponse(http.StatusOK, projectJSON, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewProjectMembershipReader(client)

	derivedUID, err := sfuuid.Normalize18(assetSFID)
	require.NoError(t, err)

	membership, lastMod, err := reader.AssembleProjectMembership(context.Background(), derivedUID)

	require.NoError(t, err)
	require.NotNil(t, membership)

	// Verify denormalised fields from Asset.
	assert.Equal(t, derivedUID, membership.UID)
	assert.Equal(t, "Active", membership.Status)
	assert.Equal(t, "2025", membership.Year)
	assert.Equal(t, "Gold", membership.Tier)
	assert.True(t, membership.AutoRenew)

	// Verify denormalised fields from Account.
	assert.Equal(t, "Acme Corp", membership.CompanyName)
	assert.Equal(t, "https://acme.com/logo.png", membership.CompanyLogoURL)
	assert.Equal(t, "https://acme.com", membership.CompanyDomain)
	assert.NotEmpty(t, membership.B2BOrgUID, "B2BOrgUID must be populated from AccountID")

	// Verify denormalised fields from Product2.
	assert.Equal(t, "Gold Membership", membership.TierName)
	assert.Equal(t, "Membership", membership.TierFamily)
	assert.Equal(t, "Corporate", membership.TierProductType)

	// Verify denormalised fields from Project__c.
	// ProjectUID is resolved at the MemberReader layer (NATS RPC); AssembleProjectMembership only sets ProjectSFID and ProjectSlug.
	assert.Equal(t, projectSFID, membership.ProjectSFID, "ProjectSFID must be populated from Projects__c")
	assert.Empty(t, membership.ProjectUID, "ProjectUID is resolved by MemberReader, not AssembleProjectMembership")
	assert.Equal(t, "linux-foundation", membership.ProjectSlug)

	// Verify lastMod is the oldest (Product2's: 2023-06-01).
	assert.True(t, lastMod.Before(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
		"lastMod should be from Product2 (2023-06-01), got %v", lastMod)
}

// TestAssembleProjectMembership_NoProject verifies that when Projects__c is nil,
// ProjectSFID, ProjectUID, and ProjectSlug remain empty, but all other denormalisations work.
func TestAssembleProjectMembership_NoProject(t *testing.T) {
	t.Parallel()

	const (
		assetSFID    = "02iB0000009ABCD"
		accountSFID  = "0012M00002FyYww"
		product2SFID = "01t2M000009ABCD"
	)

	assetJSON := `{
		"Id":"` + assetSFID + `",
		"Name":"Membership",
		"Status":"Active",
		"AccountId":"` + accountSFID + `",
		"Product2Id":"` + product2SFID + `",
		"Year__c":"2025",
		"Tier__c":"Gold",
		"Auto_Renew__c":false,
		"Price":0,
		"Annual_Full_Price__c":0,
		"Projects__c":null,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"SystemModstamp":"2025-03-20T00:00:00.000+0000"
	}`

	accountJSON := `{
		"Id":"` + accountSFID + `",
		"Name":"Acme Corp",
		"Logo_URL__c":null,
		"Website":null,
		"Account_Domain__c":null,
		"Domain_Alias__c":null,
		"Description":null,
		"Phone":null,
		"ParentId":null,
		"Industry":null,
		"Sector__c":null,
		"CrunchBase_URL__c":null,
		"NumberOfEmployees":null,
		"LF_Membership_Status__c":null,
		"CreatedDate":"2024-01-15T10:00:00.000+0000",
		"LastModifiedDate":"2024-01-15T10:00:00.000+0000",
		"SystemModstamp":"2024-01-15T10:00:00.000+0000"
	}`

	product2JSON := `{
		"Id":"` + product2SFID + `",
		"Name":"Gold Membership",
		"Family":"Membership",
		"Type__c":"Corporate",
		"CreatedDate":"2023-06-01T00:00:00.000+0000",
		"LastModifiedDate":"2023-06-01T00:00:00.000+0000",
		"SystemModstamp":"2023-06-01T00:00:00.000+0000"
	}`

	rt := &routingTransport{}
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusOK, assetJSON, nil))
	rt.route("/sobjects/Account/"+accountSFID, fakeResponse(http.StatusOK, accountJSON, nil))
	rt.route("/sobjects/Product2/"+product2SFID, fakeResponse(http.StatusOK, product2JSON, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewProjectMembershipReader(client)

	assetUID, err := sfuuid.Normalize18(assetSFID)
	require.NoError(t, err)

	membership, _, err := reader.AssembleProjectMembership(context.Background(), assetUID)

	require.NoError(t, err)
	require.NotNil(t, membership)

	// Project fields should be empty.
	assert.Empty(t, membership.ProjectSFID)
	assert.Empty(t, membership.ProjectUID)
	assert.Empty(t, membership.ProjectSlug)

	// But company and tier fields should still be populated.
	assert.Equal(t, "Acme Corp", membership.CompanyName)
	assert.Equal(t, "Gold Membership", membership.TierName)
}

// TestAssembleProjectMembership_AssetNotFound verifies that when Salesforce
// returns 404 for the Asset, an error is returned.
func TestAssembleProjectMembership_AssetNotFound(t *testing.T) {
	t.Parallel()

	const assetSFID = "02iB000009NOTFN"

	rt := &routingTransport{}
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusNotFound, `[]`, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewProjectMembershipReader(client)

	assetUID, err := sfuuid.Normalize18(assetSFID)
	require.NoError(t, err)

	membership, _, err := reader.AssembleProjectMembership(context.Background(), assetUID)

	require.Error(t, err)
	assert.Nil(t, membership)
}

// TestAssembleProjectMembership_B2BOrgNotFound verifies that when Account
// fetch fails, the error is propagated.
func TestAssembleProjectMembership_B2BOrgNotFound(t *testing.T) {
	t.Parallel()

	const (
		assetSFID   = "02iB0000009ABCD"
		accountSFID = "0012M00002NOTFN"
	)

	assetJSON := `{
		"Id":"` + assetSFID + `",
		"Name":"Membership",
		"Status":"Active",
		"AccountId":"` + accountSFID + `",
		"Product2Id":"",
		"Year__c":null,
		"Tier__c":null,
		"Auto_Renew__c":false,
		"Price":0,
		"Annual_Full_Price__c":0,
		"Projects__c":null,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"SystemModstamp":"2025-03-20T00:00:00.000+0000"
	}`

	rt := &routingTransport{}
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusOK, assetJSON, nil))
	rt.route("/sobjects/Account/"+accountSFID, fakeResponse(http.StatusNotFound, `[]`, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewProjectMembershipReader(client)

	assetUID, err := sfuuid.Normalize18(assetSFID)
	require.NoError(t, err)

	membership, _, err := reader.AssembleProjectMembership(context.Background(), assetUID)

	require.Error(t, err)
	assert.Nil(t, membership)
}
