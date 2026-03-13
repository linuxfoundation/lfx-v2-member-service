// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

// NATS KV bucket and RPC subject constants shared across the NATS infrastructure layer.
const (
	// V1ObjectsKVBucket is the name of the v1-objects NATS KV bucket. Used by the
	// storage layer to resolve v2 project UIDs to Salesforce SFIDs for inbound filter
	// translation.
	V1ObjectsKVBucket = "v1-objects"

	// V1MappingLookupSubject is the NATS RPC subject for bidirectional v1↔v2 ID mapping
	// lookups via v1-sync-helper. Supports both v1→v2 (e.g. project.sfid.{sfid}) and
	// v2→v1 (e.g. project.uid.{uuid}) key patterns. Used by the project resolver to map
	// v2 UIDs to B2C SFIDs.
	V1MappingLookupSubject = "lfx.lookup_v1_mapping"
)
