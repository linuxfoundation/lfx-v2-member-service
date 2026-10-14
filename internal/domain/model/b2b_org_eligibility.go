// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// RecordTypeCompanyAccount is the Salesforce Account record type developer name
// for company Accounts. Developer names are stable across Salesforce orgs;
// record type Ids are not.
const RecordTypeCompanyAccount = "Company_Account"

// IsB2BOrgEligible reports whether a non-deleted Salesforce Account is a B2B
// organization: every Company Account is, and any other record type
// (Partner_Restricted, Project_Account, PersonAccount, or none) is only while
// it holds a live Membership Asset.
//
// The rule lives here rather than in SOQL because Salesforce rejects a
// semi-join inside OR, so the queries fetch both inputs and every caller
// decides through this one function.
func IsB2BOrgEligible(recordTypeDeveloperName string, hasMembership bool) bool {
	return recordTypeDeveloperName == RecordTypeCompanyAccount || hasMembership
}
