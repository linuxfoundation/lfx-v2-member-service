// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

import (
	"strings"
	"sync/atomic"
)

// accountSlugField is the Salesforce custom field carrying an organization's
// URL slug. It is published on the b2b_org indexer document as `data.slug` and
// as a `slug:{slug}` search tag so Org Lens can address an organization as
// `/org/{slug}/…` and resolve that slug back through the access-filtered
// query-service (lfx-self-serve#2570, spec 050).
const accountSlugField = "Slug__c"

// accountSlugFieldEnabled controls whether Slug__c is requested from Salesforce
// on the two Account read paths (SOQL list/search and sObject single read).
//
// Defaults to enabled. Set from SF_ACCOUNT_SLUG_FIELD_ENABLED via Config.Init so a
// Salesforce org that lacks the custom field (the partial sandbox that motivated
// LFXV2-1363, where every fetch 400'd with INVALID_FIELD) can opt out without a
// code change. When disabled the field is simply not selected and every
// B2BOrg.Slug is empty — Org Lens then addresses those organizations by SFID.
var accountSlugFieldEnabled atomic.Bool

func init() {
	accountSlugFieldEnabled.Store(true)
}

// setAccountSlugFieldEnabled toggles Slug__c selection for the process. Called
// once from Config.Init; exposed unexported for tests.
func setAccountSlugFieldEnabled(enabled bool) {
	accountSlugFieldEnabled.Store(enabled)
}

// withAccountSlugField appends Slug__c to a comma-separated field list when the
// toggle is on. Used for both the SOQL SELECT projection and the sObject
// ?fields= parameter so the two paths cannot disagree.
func withAccountSlugField(fields string) string {
	if !accountSlugFieldEnabled.Load() {
		return fields
	}
	return fields + "," + accountSlugField
}

// b2bOrgCacheKeyPrefix returns the full-org sObject cache prefix for the current
// Slug__c projection. The projection is part of the cache identity (see the
// prefix constants in sobject_readers.go), so a toggle flip can never replay a
// body fetched under the other field list.
func b2bOrgCacheKeyPrefix() string {
	if accountSlugFieldEnabled.Load() {
		return sobjectKeyPrefixB2BOrg
	}
	return sobjectKeyPrefixB2BOrgNoSlug
}

// normalizeOrgSlug canonicalizes a Salesforce Account.Slug__c value for use as
// URL identity: trimmed and lowercased. Salesforce custom slug fields arrive
// mixed-case (the project resolver documents `ToIP` vs `toip` for
// Project__c.Slug__c) while the URL, the search tag, and the uniqueness check
// all key on one form. Normalizing at ingest — not only at lookup — is what
// keeps `data.slug`, the `slug:` tag, and the canonical URL from disagreeing.
// Empty in, empty out; nothing is ever generated.
func normalizeOrgSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}
