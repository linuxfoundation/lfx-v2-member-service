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
	KVBucketNameCDCRepair = "cdc-repair"
)
