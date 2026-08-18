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

	// ReindexTypeB2BOrgDeleteAccess and ReindexTypeProjectMembershipDeleteAccess
	// mark a failed delete_access publish for durable operator recovery. These
	// are deliberately distinct from ReindexTypeB2BOrg/ReindexTypeProjectMembership:
	// a targeted /admin/reindex on those types re-fetches and re-upserts the live
	// Salesforce record, which cannot repair a purge — the record is gone, so the
	// fetch reports outcomeNotFound and no delete_access is re-emitted. These two
	// types are not wired into reindexItem or any automated drain; they exist
	// solely so ListPending can surface the exact (type, uid) pairs that failed to
	// purge for manual recovery.
	ReindexTypeB2BOrgDeleteAccess            = "b2b_org_delete_access"
	ReindexTypeProjectMembershipDeleteAccess = "project_membership_delete_access"
)
