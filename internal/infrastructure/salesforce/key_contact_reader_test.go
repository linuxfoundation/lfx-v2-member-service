// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// TestAssembleKeyContact_HappyPath verifies that AssembleKeyContact fetches
// all 4 sObjects (Project_Role__c, Contact, Asset, Account) and assembles a
// fully-populated KeyContact.
func TestAssembleKeyContact_HappyPath(t *testing.T) {
	t.Parallel()

	const (
		roleSFID     = "a0F2M000008ABCD"
		contactSFID  = "0032M000009ABCD"
		assetSFID    = "02iB0000009ABCD"
		accountSFID  = "0012M00002FyYww"
		product2SFID = "01t2M000009ABCD"
	)

	roleJSON := `{
		"Id":"` + roleSFID + `",
		"Asset__c":"` + assetSFID + `",
		"Contact__c":"` + contactSFID + `",
		"Role__c":"Representative/Voting Contact",
		"Status__c":"Active",
		"BoardMember__c":true,
		"PrimaryContact__c":false,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"SystemModstamp":"2025-03-20T00:00:00.000+0000"
	}`

	contactJSON := `{
		"Id":"` + contactSFID + `",
		"FirstName":"Jane",
		"LastName":"Doe",
		"Title":"CTO",
		"Email":"jane.doe@example.com",
		"LastModifiedDate":"2025-02-10T00:00:00.000+0000"
	}`

	assetJSON := `{
		"Id":"` + assetSFID + `",
		"Name":"Acme Corp Membership",
		"Status":"Active",
		"AccountId":"` + accountSFID + `",
		"Product2Id":"` + product2SFID + `",
		"Year__c":"2025",
		"Tier__c":"Gold",
		"Auto_Renew__c":true,
		"Price":50000.0,
		"Annual_Full_Price__c":50000.0,
		"Projects__c":null,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-10T00:00:00.000+0000",
		"SystemModstamp":"2025-03-10T00:00:00.000+0000"
	}`

	accountJSON := `{
		"Id":"` + accountSFID + `",
		"Name":"Acme Corp",
		"Logo_URL__c":"https://acme.com/logo.png",
		"Website":"https://acme.com",
		"Account_Domain__c":"acme.com",
		"Domain_Alias__c":null,
		"Description":null,
		"Phone":null,
		"ParentId":null,
		"Industry":"Technology",
		"Sector__c":null,
		"CrunchBase_URL__c":null,
		"NumberOfEmployees":null,
		"LF_Membership_Status__c":"Active",
		"CreatedDate":"2024-01-15T00:00:00.000+0000",
		"LastModifiedDate":"2024-06-01T00:00:00.000+0000",
		"SystemModstamp":"2024-06-01T00:00:00.000+0000"
	}`

	rt := &routingTransport{}
	rt.route("/sobjects/Project_Role__c/"+roleSFID, fakeResponse(http.StatusOK, roleJSON, nil))
	rt.route("/sobjects/Contact/"+contactSFID, fakeResponse(http.StatusOK, contactJSON, nil))
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusOK, assetJSON, nil))
	rt.route("/sobjects/Account/"+accountSFID, fakeResponse(http.StatusOK, accountJSON, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewKeyContactReader(client)

	roleUID, err := sfuuid.Normalize18(roleSFID)
	require.NoError(t, err)

	kc, lastMod, err := reader.AssembleKeyContact(context.Background(), roleUID)

	require.NoError(t, err)
	require.NotNil(t, kc)

	assert.Equal(t, roleUID, kc.UID)
	assert.Equal(t, "Representative/Voting Contact", kc.Role)
	assert.Equal(t, "Active", kc.Status)
	assert.True(t, kc.BoardMember)
	assert.False(t, kc.PrimaryContact)

	assert.Equal(t, "Jane", kc.FirstName)
	assert.Equal(t, "Doe", kc.LastName)
	assert.Equal(t, "CTO", kc.Title)
	assert.Equal(t, "jane.doe@example.com", kc.Email)

	assert.NotEmpty(t, kc.MembershipUID)
	assert.NotEmpty(t, kc.TierUID)
	assert.NotEmpty(t, kc.B2BOrgUID)
	assert.Equal(t, "Acme Corp", kc.CompanyName)
	assert.Equal(t, "https://acme.com/logo.png", kc.CompanyLogoURL)

	// lastMod should be the oldest across all 4 records (Account: 2024-06-01).
	assert.False(t, lastMod.IsZero())
	assert.True(t, lastMod.Year() == 2024,
		"lastMod should come from Account (2024-06-01), got %v", lastMod)
}

// TestAssembleKeyContact_NoContact verifies that a Project_Role__c with no
// Contact__c still assembles a KeyContact — personal fields remain empty.
func TestAssembleKeyContact_NoContact(t *testing.T) {
	t.Parallel()

	const (
		roleSFID    = "a0F2M000008ABCD"
		assetSFID   = "02iB0000009ABCD"
		accountSFID = "0012M00002FyYww"
	)

	roleJSON := `{
		"Id":"` + roleSFID + `",
		"Asset__c":"` + assetSFID + `",
		"Contact__c":null,
		"Role__c":"Billing Contact",
		"Status__c":"Active",
		"BoardMember__c":false,
		"PrimaryContact__c":true,
		"CreatedDate":"2025-01-01T00:00:00.000+0000",
		"LastModifiedDate":"2025-03-20T00:00:00.000+0000",
		"SystemModstamp":"2025-03-20T00:00:00.000+0000"
	}`

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
		"LastModifiedDate":"2025-03-10T00:00:00.000+0000",
		"SystemModstamp":"2025-03-10T00:00:00.000+0000"
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
		"CreatedDate":"2024-01-15T00:00:00.000+0000",
		"LastModifiedDate":"2024-06-01T00:00:00.000+0000",
		"SystemModstamp":"2024-06-01T00:00:00.000+0000"
	}`

	rt := &routingTransport{}
	rt.route("/sobjects/Project_Role__c/"+roleSFID, fakeResponse(http.StatusOK, roleJSON, nil))
	rt.route("/sobjects/Asset/"+assetSFID, fakeResponse(http.StatusOK, assetJSON, nil))
	rt.route("/sobjects/Account/"+accountSFID, fakeResponse(http.StatusOK, accountJSON, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewKeyContactReader(client)

	roleUID, err := sfuuid.Normalize18(roleSFID)
	require.NoError(t, err)

	kc, _, err := reader.AssembleKeyContact(context.Background(), roleUID)

	require.NoError(t, err)
	require.NotNil(t, kc)

	assert.Empty(t, kc.FirstName)
	assert.Empty(t, kc.LastName)
	assert.Empty(t, kc.Email)
	assert.Equal(t, "Acme Corp", kc.CompanyName)
	assert.Equal(t, "Billing Contact", kc.Role)
	assert.True(t, kc.PrimaryContact)
}

// TestAssembleKeyContact_RoleNotFound verifies that a 404 from Project_Role__c
// is propagated as an error.
func TestAssembleKeyContact_RoleNotFound(t *testing.T) {
	t.Parallel()

	const roleSFID = "a0F2M000008NOTF"

	rt := &routingTransport{}
	rt.route("/sobjects/Project_Role__c/"+roleSFID, fakeResponse(http.StatusNotFound, `[]`, nil))
	rt.route("/limits", fakeResponse(http.StatusOK, `{}`, nil))

	client := &SObjectClient{sf: fakeSalesforce(t, rt), cache: newMemCache()}
	reader := NewKeyContactReader(client)

	roleUID, err := sfuuid.Normalize18(roleSFID)
	require.NoError(t, err)

	kc, _, err := reader.AssembleKeyContact(context.Background(), roleUID)

	require.Error(t, err)
	assert.Nil(t, kc)
}
