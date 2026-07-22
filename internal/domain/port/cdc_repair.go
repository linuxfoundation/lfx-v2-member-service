// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import (
	"context"
	"time"
)

// RepairMarker is one pending entry in the CDC quota-repair queue: a record the
// CDC consumer skipped while the Salesforce API quota was exhausted, awaiting
// repair via POST /admin/reindex {cdc_repair:true}.
type RepairMarker struct {
	// Type is the reindex target type: "b2b_org", "project_membership", or
	// "key_contact".
	Type string
	// SFID is the canonical 18-character Salesforce ID of the record.
	SFID string
	// SkippedAt is when the marker was (most recently) written on skip.
	SkippedAt time.Time
	// Revision is the KV revision observed at selection, used for the
	// revision-conditional delete that guards against a concurrent re-skip or a
	// concurrent drain.
	Revision uint64
}

// CDCRepairStore is the durable queue of records skipped by the CDC consumer's
// quota guard. Writes happen on the consumer side (on skip); listing and
// deletion happen on the API side (during a cdc_repair drain).
//
// There is no distributed lock: concurrent drains are made safe by idempotent
// targeted reindex plus the revision-conditional DeletePending below.
type CDCRepairStore interface {
	// PutPending writes (or overwrites) a pending marker for (reindexType, sfid).
	// A later skip for the same target rewrites the same marker with a fresh
	// SkippedAt.
	PutPending(ctx context.Context, reindexType, sfid string) error

	// ListPending returns up to limit pending markers for reindexType, each
	// carrying the KV revision observed at selection. It does not enumerate the
	// full bucket and does not promise any particular ordering.
	ListPending(ctx context.Context, reindexType string, limit int) ([]RepairMarker, error)

	// DeletePending revision-conditionally deletes the marker for
	// (reindexType, sfid). It returns a Conflict error when the stored revision
	// no longer matches (the consumer re-skipped, or a concurrent drain already
	// acted) so the newer marker survives for the next drain. A marker that is
	// already absent is treated as success.
	DeletePending(ctx context.Context, reindexType, sfid string, revision uint64) error
}
