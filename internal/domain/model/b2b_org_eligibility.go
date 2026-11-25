// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// RecordTypeCompanyAccount is the Salesforce Account record type developer name
// for company Accounts. Developer names are stable across Salesforce orgs;
// record type Ids are not.
const RecordTypeCompanyAccount = "Company_Account"

// IsB2BOrgEligible reports whether a non-deleted Salesforce Account is a B2B
// organization. An Account holding a non-deleted Membership Asset always is.
// With includeCompanyAccounts, every Company Account is as well; without it,
// a Company Account without a membership is not, which is the rule that
// applied before Company Accounts qualified. The Asset's Status and dates are
// not checked.
//
// The rule lives here rather than in SOQL because Salesforce rejects a
// semi-join inside OR, so the queries fetch both inputs and every caller
// decides through this one function.
func IsB2BOrgEligible(recordTypeDeveloperName string, hasMembership, includeCompanyAccounts bool) bool {
	return hasMembership || (includeCompanyAccounts && recordTypeDeveloperName == RecordTypeCompanyAccount)
}
