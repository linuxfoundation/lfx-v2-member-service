// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// CDCChangeType enumerates the Salesforce Change Data Capture change types the
// consumer acts on. Values mirror the ChangeEventHeader.changeType field.
type CDCChangeType string

const (
	CDCChangeCreate   CDCChangeType = "CREATE"
	CDCChangeUpdate   CDCChangeType = "UPDATE"
	CDCChangeDelete   CDCChangeType = "DELETE"
	CDCChangeUndelete CDCChangeType = "UNDELETE"
	// CDCChangeGapUndelete is emitted when a record is restored during a CDC
	// overflow gap (granular UNDELETE was not delivered).
	CDCChangeGapUndelete CDCChangeType = "GAP_UNDELETE"
	// CDCChangeGapDelete is emitted by Salesforce when a record is deleted
	// during a CDC overflow gap (granular DELETE was not delivered).
	CDCChangeGapDelete CDCChangeType = "GAP_DELETE"
	// CDCChangeGapOverflow and other GAP_* types signal that granular delivery
	// was not possible. They are handled by the default re-fetch+upsert path.
	CDCChangeGapOverflow CDCChangeType = "GAP_OVERFLOW"
)

// CDCEvent is the normalized, transport-agnostic representation of a Salesforce
// CDC change event. The adapter in internal/infrastructure/salesforce/pubsub owns
// all Avro/gRPC/proto types and emits these domain events.
type CDCEvent struct {
	// Entity is the Salesforce entityName from ChangeEventHeader
	// (e.g. "Account", "Asset", "Project_Role__c").
	Entity string

	// RecordIDs holds every Salesforce record ID in the event. Batch operations
	// emit multiple IDs in a single event — the consumer processes all of
	// them, not just the first.
	RecordIDs []string

	// ChangeType is the ChangeEventHeader.changeType.
	ChangeType CDCChangeType

	// ReplayID is the opaque per-event replay cursor. Persisted (commit-after-
	// process) so a restart resumes from the last fully-handled event.
	ReplayID []byte
}
