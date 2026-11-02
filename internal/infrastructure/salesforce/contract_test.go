// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

// contract_test.go proves that the SOQL-bulk path (convertSOQLTo*) and the
// sObject-single-record path (sobject*ToModel) produce equivalent domain
// objects for the same logical record. This guards against divergent field
// mappings between the two read paths.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// Fixed Salesforce IDs used across all contract tests.
const (
	contractAccountSFID  = "0012M00002FyYwwQAF"
	contractAssetSFID    = "02iB0000009ABCdIAM"
	contractProduct2SFID = "01t2M000009ABCdIAM"
	contractRoleSFID     = "a0K2M000000ABCdUAG"
)

// TestContract_B2BOrg asserts that convertSOQLToB2BOrg and sobjectAccountToB2BOrg
// produce equal core fields for the same logical Account record.
func TestContract_B2BOrg(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(contractAccountSFID)
	require.NoError(t, err, "Normalize18 must succeed for a valid SFID")

	website := "https://example.com"
	domain := "example.com"
	logo := "https://logo.example.com/img.png"

	soqlRec := soqlAccount{
		ID:            contractAccountSFID,
		Name:          "Example Corp",
		Website:       &website,
		PrimaryDomain: &domain,
		LogoURL:       &logo,
	}

	sobjectRec := sobjectAccount{
		ID:            contractAccountSFID,
		Name:          "Example Corp",
		Website:       &website,
		PrimaryDomain: &domain,
		LogoURL:       &logo,
	}

	ctx := context.Background()

	fromSOQL, err := convertSOQLToB2BOrg(ctx, soqlRec)
	require.NoError(t, err)

	fromSObject, err := sobjectAccountToB2BOrg(ctx, &sobjectRec, uid)
	require.NoError(t, err)

	assert.Equal(t, uid, fromSOQL.UID, "UID: SOQL path should equal canonical 18-char SFID")
	assert.Equal(t, fromSOQL.UID, fromSObject.UID, "UID: both paths must agree")
	assert.Equal(t, fromSOQL.SFID, fromSObject.SFID, "SFID: both paths must agree")
	assert.Equal(t, fromSOQL.Name, fromSObject.Name, "Name: both paths must agree")
	assert.Equal(t, fromSOQL.Website, fromSObject.Website, "Website: both paths must agree")
	assert.Equal(t, fromSOQL.PrimaryDomain, fromSObject.PrimaryDomain, "PrimaryDomain: both paths must agree")
	assert.Equal(t, fromSOQL.LogoURL, fromSObject.LogoURL, "LogoURL: both paths must agree")
}

// TestContract_ProjectMembership asserts that convertSOQLToProjectMembership and
// sobjectAssetToModel produce equal core fields for the same logical Asset record.
func TestContract_ProjectMembership(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(contractAssetSFID)
	require.NoError(t, err)
	tierUID, err := sfuuid.Normalize18(contractProduct2SFID)
	require.NoError(t, err)

	status := "Active"
	year := "2025"
	tier := "Gold"
	renewalType := "Annual"
	freq := "Annual"
	purchase := "2025-01-01"
	endDate := "2025-12-31"

	soqlRec := soqlAsset{
		ID:               contractAssetSFID,
		Product2ID:       contractProduct2SFID,
		AccountID:        contractAccountSFID,
		Status:           &status,
		Year:             &year,
		Tier:             &tier,
		AutoRenew:        true,
		RenewalType:      &renewalType,
		Price:            50000.0,
		AnnualFullPrice:  50000.0,
		PaymentFrequency: &freq,
		PurchaseDate:     &purchase,
		UsageEndDate:     &endDate,
		CreatedDate:      "2025-01-01T00:00:00.000+0000",
		LastModifiedDate: "2025-03-20T00:00:00.000+0000",
	}

	sobjectRec := sobjectAsset{
		ID:               contractAssetSFID,
		Product2ID:       contractProduct2SFID,
		AccountID:        contractAccountSFID,
		Status:           &status,
		Year:             &year,
		Tier:             &tier,
		AutoRenew:        true,
		RenewalType:      &renewalType,
		Price:            50000.0,
		AnnualFullPrice:  50000.0,
		PaymentFrequency: &freq,
		PurchaseDate:     &purchase,
		UsageEndDate:     &endDate,
		CreatedDate:      "2025-01-01T00:00:00.000+0000",
		LastModifiedDate: "2025-03-20T00:00:00.000+0000",
	}

	fromSOQL, err := convertSOQLToProjectMembership(t.Context(), soqlRec)
	require.NoError(t, err)

	fromSObject, err := sobjectAssetToModel(&sobjectRec, uid)
	require.NoError(t, err)

	assert.Equal(t, uid, fromSOQL.UID, "UID: SOQL path should equal canonical 18-char SFID")
	assert.Equal(t, fromSOQL.UID, fromSObject.UID, "UID: both paths must agree")
	assert.Equal(t, tierUID, fromSOQL.TierUID, "TierUID: SOQL path should equal canonical 18-char SFID")
	assert.Equal(t, fromSOQL.TierUID, fromSObject.TierUID, "TierUID: both paths must agree")
	assert.Equal(t, fromSOQL.Status, fromSObject.Status, "Status: both paths must agree")
	assert.Equal(t, fromSOQL.Year, fromSObject.Year, "Year: both paths must agree")
	assert.Equal(t, fromSOQL.Tier, fromSObject.Tier, "Tier: both paths must agree")
	assert.Equal(t, fromSOQL.AutoRenew, fromSObject.AutoRenew, "AutoRenew: both paths must agree")
	assert.Equal(t, fromSOQL.RenewalType, fromSObject.RenewalType, "RenewalType: both paths must agree")
	assert.Equal(t, fromSOQL.Price, fromSObject.Price, "Price: both paths must agree")
	assert.Equal(t, fromSOQL.AnnualFullPrice, fromSObject.AnnualFullPrice, "AnnualFullPrice: both paths must agree")
	assert.Equal(t, fromSOQL.PaymentFrequency, fromSObject.PaymentFrequency, "PaymentFrequency: both paths must agree")
	assert.Equal(t, fromSOQL.PurchaseDate, fromSObject.PurchaseDate, "PurchaseDate: both paths must agree")
	assert.Equal(t, fromSOQL.EndDate, fromSObject.EndDate, "EndDate: both paths must agree")
	assert.Equal(t, fromSOQL.CreatedAt, fromSObject.CreatedAt, "CreatedAt: both paths must agree")
	assert.Equal(t, fromSOQL.UpdatedAt, fromSObject.UpdatedAt, "UpdatedAt: both paths must agree")
}

// TestContract_KeyContact asserts that convertSOQLToKeyContact and
// sobjectProjectRoleToModel produce equal core Project_Role__c fields for the
// same logical record. Contact and company fields (populated via sub-object
// joins in SOQL but via separate sObject fetches) are outside this contract.
func TestContract_KeyContact(t *testing.T) {
	t.Parallel()

	uid, err := sfuuid.Normalize18(contractRoleSFID)
	require.NoError(t, err)
	membershipUID, err := sfuuid.Normalize18(contractAssetSFID)
	require.NoError(t, err)

	role := "Technical Advisory Board"
	status := "Active"

	soqlRec := soqlProjectRole{
		ID:             contractRoleSFID,
		AssetID:        contractAssetSFID,
		Role:           &role,
		Status:         &status,
		BoardMember:    true,
		PrimaryContact: false,
		CreatedDate:    "2025-01-01T00:00:00.000+0000",
		SystemModstamp: "2025-06-01T00:00:00.000+0000",
	}

	sobjectRec := sobjectProjectRole{
		ID:               contractRoleSFID,
		AssetID:          contractAssetSFID,
		Role:             &role,
		Status:           &status,
		BoardMember:      true,
		PrimaryContact:   false,
		CreatedDate:      "2025-01-01T00:00:00.000+0000",
		LastModifiedDate: "2025-06-01T00:00:00.000+0000",
	}

	fromSOQL, err := convertSOQLToKeyContact(soqlRec, nil)
	require.NoError(t, err)

	fromSObject, err := sobjectProjectRoleToModel(&sobjectRec, uid)
	require.NoError(t, err)

	assert.Equal(t, uid, fromSOQL.UID, "UID: SOQL path should equal canonical 18-char SFID")
	assert.Equal(t, fromSOQL.UID, fromSObject.UID, "UID: both paths must agree")
	assert.Equal(t, membershipUID, fromSOQL.MembershipUID, "MembershipUID: SOQL should equal canonical 18-char SFID")
	assert.Equal(t, fromSOQL.MembershipUID, fromSObject.MembershipUID, "MembershipUID: both paths must agree")
	assert.Equal(t, fromSOQL.Role, fromSObject.Role, "Role: both paths must agree")
	assert.Equal(t, fromSOQL.Status, fromSObject.Status, "Status: both paths must agree")
	assert.Equal(t, fromSOQL.BoardMember, fromSObject.BoardMember, "BoardMember: both paths must agree")
	assert.Equal(t, fromSOQL.PrimaryContact, fromSObject.PrimaryContact, "PrimaryContact: both paths must agree")
	assert.Equal(t, fromSOQL.CreatedAt, fromSObject.CreatedAt, "CreatedAt: both paths must agree")
}
