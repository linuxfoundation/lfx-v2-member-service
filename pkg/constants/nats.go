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
	// B2BConsumerName is the durable consumer name for the salesforce_b2b KV consumer.
	B2BConsumerName = "member-service-b2b-consumer"

	// B2BConsumerStreamName is the JetStream stream name for the v1-objects KV bucket.
	B2BConsumerStreamName = "KV_v1-objects"

	// V1ObjectsKVBucket is the name of the v1-objects NATS KV bucket consumed by the b2b consumer.
	V1ObjectsKVBucket = "v1-objects"

	// V1MappingLookupSubject is the NATS RPC subject for v1-to-v2 ID mapping lookups via v1-sync-helper.
	V1MappingLookupSubject = "lfx.lookup_v1_mapping"

	// KVBucketNameB2BMapping is the name of the KV bucket for forward-lookup indexes owned by the member service.
	KVBucketNameB2BMapping = "project-membership-mapping"
)

// B2BConsumerFilterSubjects lists the exact per-table JetStream subjects to filter for
// the b2b KV consumer. Using FilterSubjects (NATS 2.10+) with explicit per-table entries
// is required because NATS wildcards (* and >) only match whole dot-delimited tokens, so
// a pattern like "$KV.v1-objects.salesforce_b2b->" would never match 4-level subjects of
// the form "$KV.v1-objects.salesforce_b2b-Account.{sfid}". Table names use the original
// PostgreSQL mixed-case form to match the keys written by both the WAL listener and Meltano.
var B2BConsumerFilterSubjects = []string{
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Account.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Asset.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Product2.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Contact.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Alternate_Email__c.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Project__c.*",
	"$KV." + V1ObjectsKVBucket + ".salesforce_b2b-Project_Role__c.*",
}
