// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines shared constant values used across the service.
package constants

// NATS Key-Value store bucket names.
const (
	// KVBucketNameCache is the name of the single KV bucket used for all cached
	// membership records. Keys within the bucket are namespaced by type prefix
	// (e.g. "tier/{uid}", "membership/{uid}", "key-contacts/{membership_uid}",
	// "project-sfid/{uid}", "project-uid/{slug}") to avoid collisions.
	KVBucketNameCache = "membership-cache"

	// KVBucketNameSObjectCache is the name of the KV bucket used for the new
	// sObject REST API cache. Keys use the pattern "{type}.{uid}" (e.g.
	// "b2b_org.{uid}", "project_membership.{uid}"). Values carry HTTP conditional
	// GET metadata (ETag, Last-Modified) alongside the JSON-encoded sObject body,
	// enabling If-None-Match / If-Modified-Since cache validation on re-fetch.
	KVBucketNameSObjectCache = "member-service-cache"

	// KVBucketNameOrgSettings is the name of the KV bucket for authoritative
	// b2b_org settings (writers, auditors, pending invites).
	// No MaxAge TTL — entries are never silently evicted.
	KVBucketNameOrgSettings = "org-settings"

	// KVBucketNamePubSubState is the name of the KV bucket holding Salesforce
	// Pub/Sub CDC consumer state: per-channel replay cursors keyed by
	// "pubsub-replay.<channel>". No MaxAge TTL — a quiet channel must never lose
	// its replay cursor to hard eviction (which would force a silent fallback to
	// LATEST and a gap in delivered events).
	// Single-active-consumer enforcement is handled at the Kubernetes level
	// (replicas: 1, Recreate strategy) — no lease key is stored here.
	KVBucketNamePubSubState = "pubsub-state"

	// KVBucketNameOrgWorkspaces is the name of the KV bucket for authoritative
	// b2b_org workspace records (named project containers). One key per org;
	// key format: "org-workspaces.{orgUID}". No MaxAge TTL — workspace membership
	// is authoritative state that must never be silently evicted.
	KVBucketNameOrgWorkspaces = "org-workspaces"

	// KVBucketNameWorkspaceProjects is the name of the KV bucket for authoritative
	// workspace project associations. One key per workspace;
	// key format: "org_workspace_projects.{workspaceUID}". No MaxAge TTL.
	KVBucketNameWorkspaceProjects = "org_workspace_projects"

	// KVBucketNameCDCRepair is the name of the KV bucket holding the durable
	// CDC quota-repair queue. When the CDC consumer skips an upsert because the
	// Salesforce API quota is exhausted, it records the affected record as a
	// pending marker keyed "pending.{reindex_type}.{sfid}" so the record can be
	// repaired later via POST /admin/reindex {cdc_repair:true}. No MaxAge TTL —
	// a pending marker must never be silently evicted before it is drained; a
	// marker is written once on skip and not touched again until repaired, so
	// nothing resets an entry's clock. History is 1 (a pending set — old marker
	// revisions are never needed). No distributed lock: concurrent drains are
	// made safe by idempotent targeted reindex plus revision-conditional delete.
	//
	// This bucket also holds two marker-only types — reindex_types.go's
	// ReindexTypeB2BOrgDeleteAccess and ReindexTypeProjectMembershipDeleteAccess
	// — written when a delete_access publish fails. These are NOT quota-skip
	// markers and are never drained by /admin/reindex {cdc_repair:true}: a
	// targeted reindex re-fetches and re-upserts the live Salesforce record,
	// which cannot repair a purge. They exist purely so ListPending can surface
	// the exact (type, uid) pairs that need a manual delete_access republish;
	// see docs/cdc-consumer.md for the manual list/republish/delete procedure.
	KVBucketNameCDCRepair = "cdc-repair"

	// KVBucketNameKeyContactGrants is the name of the KV bucket recording the
	// FGA key_contact grant published for each key contact, keyed
	// "key_contact.{sfid}" with a {membership_uid, username} value. A CDC delete
	// event carries only the key contact's own SFID and the Salesforce record is
	// already gone when it is handled, so this is the only place the parent
	// membership and granted username can be recovered to build a correct
	// member_remove. No MaxAge TTL — a key contact may live for years before
	// deletion, and an evicted entry cannot be rebuilt from any other source.
	// History is 1 (current grant only — prior grants are never replayed).
	// Writes are revision-conditional so the read-compare-publish-write cycle in
	// the put path cannot silently lose a concurrent grant change.
	KVBucketNameKeyContactGrants = "key-contact-grants"
)
