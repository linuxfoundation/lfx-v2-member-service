// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// NATS subjects for fga-sync access control messages.
const (
	// FGASyncUpdateAccessSubject is the subject for creating/updating access control via fga-sync.
	FGASyncUpdateAccessSubject = "lfx.fga-sync.update_access"
)

// NATS subjects for indexer messages published by the b2b consumer.
const (
	// IndexProjectProductsB2BSubject is the NATS subject for indexing project_products_b2b documents.
	IndexProjectProductsB2BSubject = "lfx.index.project_products_b2b"

	// IndexProjectMembersB2BSubject is the NATS subject for indexing project_members_b2b documents.
	IndexProjectMembersB2BSubject = "lfx.index.project_members_b2b"

	// IndexKeyContactSubject is the NATS subject for indexing key_contact documents.
	IndexKeyContactSubject = "lfx.index.key_contact"
)

// NATS JetStream consumer configuration for the b2b KV consumer.
const (
	// B2BConsumerStreamName is the JetStream stream name for the v1-objects KV bucket.
	B2BConsumerStreamName = "KV_v1-objects"

	// V1ObjectsKVBucket is the name of the v1-objects NATS KV bucket consumed by the b2b consumer.
	V1ObjectsKVBucket = "v1-objects"

	// V1MappingLookupSubject is the NATS RPC subject for v1-to-v2 ID mapping lookups via v1-sync-helper.
	V1MappingLookupSubject = "lfx.lookup_v1_mapping"

	// KVBucketNameB2BMapping is the name of the KV bucket for forward-lookup indexes owned by the member service.
	KVBucketNameB2BMapping = "project-membership-mapping"
)

// B2BTableConsumerNames maps each salesforce_b2b table name to its durable JetStream
// consumer name. One consumer per table ensures that a NAK/backoff on one table (e.g.
// waiting for a missing FK dependency) does not delay processing of unrelated tables.
// Table names use the original PostgreSQL mixed-case form to match the keys written by
// both the WAL listener and Meltano.
var B2BTableConsumerNames = map[string]string{
	"salesforce_b2b-Project__c":       "member-service-b2b-project",
	"salesforce_b2b-Account":          "member-service-b2b-account",
	"salesforce_b2b-Product2":         "member-service-b2b-product2",
	"salesforce_b2b-Asset":            "member-service-b2b-asset",
	"salesforce_b2b-Contact":          "member-service-b2b-contact",
	"salesforce_b2b-Alternate_Email__c": "member-service-b2b-alternate-email",
	"salesforce_b2b-Project_Role__c":  "member-service-b2b-project-role",
}

// B2BTableFilterSubject returns the JetStream filter subject for a given table prefix.
// NATS wildcards only match whole dot-delimited tokens, so each table requires an
// explicit 4-level subject of the form "$KV.v1-objects.{table}.{sfid}".
func B2BTableFilterSubject(tablePrefix string) string {
	return "$KV." + V1ObjectsKVBucket + "." + tablePrefix + ".*"
}

// B2BReloadTableGroups defines the dependency-ordered groups used when reloading all
// consumers. Each inner slice is a group of tables that may be
// processed concurrently; groups must be processed sequentially so that FK dependencies
// are satisfied before the tables that reference them.
//
// Dependency order:
//   - Group 0: Project__c — referenced by Product2, Asset (via Projects__c)
//   - Group 1: Account, Product2 — referenced by Asset; no FK between them
//   - Group 2: Asset — referenced by Project_Role__c; depends on Account + Product2
//   - Group 3: Contact, Alternate_Email__c — referenced by Project_Role__c; Contact has no
//     FK on Asset; Alternate_Email__c depends on Contact being present first
//   - Group 4: Project_Role__c — depends on Asset + Contact
var B2BReloadTableGroups = [][]string{
	{
		"salesforce_b2b-Project__c",
	},
	{
		"salesforce_b2b-Account",
		"salesforce_b2b-Product2",
	},
	{
		"salesforce_b2b-Asset",
	},
	{
		"salesforce_b2b-Contact",
		"salesforce_b2b-Alternate_Email__c",
	},
	{
		"salesforce_b2b-Project_Role__c",
	},
}
