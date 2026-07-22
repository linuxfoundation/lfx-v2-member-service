// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// Reindex/CDC entity type vocabulary shared by the backfill runner, the CDC
// consumer's skip-mapping, and the cdc-repair queue. Keeping one canonical set
// of literals avoids the writer (consumer) and reader (drain) silently
// drifting apart on the type strings that key the "pending.{type}.{sfid}" KV
// namespace.
const (
	// ReindexTypeB2BOrg is the reindex/CDC-repair type for Salesforce Account
	// records.
	ReindexTypeB2BOrg = "b2b_org"

	// ReindexTypeProjectMembership is the reindex/CDC-repair type for
	// Salesforce Asset records.
	ReindexTypeProjectMembership = "project_membership"

	// ReindexTypeKeyContact is the reindex/CDC-repair type for Salesforce
	// Project_Role__c records.
	ReindexTypeKeyContact = "key_contact"

	// ReindexTypeB2BOrgSettings is a backfill-only type (avatar enrichment);
	// it has no CDC skip path and is not a valid cdc_repair target.
	ReindexTypeB2BOrgSettings = "b2b_org_settings"
)
