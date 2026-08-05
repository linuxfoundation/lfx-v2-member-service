// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package port

import "context"

// KeyContactGrant is the FGA key_contact grant this service last published for a
// key contact: the project_membership object the relation was granted on, and
// the username it was granted to.
//
// It exists because a CDC delete event carries only the key contact's own SFID —
// no field payloads — and the Salesforce record is already gone by the time the
// event is handled, so neither the parent membership nor the username can be
// recovered at revoke time. Both are required: fga-sync rejects a member_remove
// with an empty username without performing any cleanup.
type KeyContactGrant struct {
	// MembershipUID is the parent Asset SFID — the project_membership object
	// the key_contact relation was granted on.
	MembershipUID string

	// Username is the LFID the relation was granted to.
	Username string

	// Revision is the KV revision observed on read, used for the
	// revision-conditional write that guards the read-modify-write in the put
	// path. Zero means "not currently stored".
	Revision uint64
}

// KeyContactGrantIndex is the durable record of published key_contact grants,
// keyed by key contact UID. It is authoritative rather than a cache: entries
// must outlive the grant they describe (key contacts can live for years before
// deletion), so it has no TTL and there is no source to rebuild an entry from
// once the Salesforce record is deleted.
//
// The index tracks what this service published, not live OpenFGA state — FGA
// reconciliation is asynchronous, so a published message is not a converged
// tuple. That is the correct basis for deciding what to revoke.
type KeyContactGrantIndex interface {
	// Get returns the stored grant for uid. The bool reports whether an entry
	// exists; a missing entry is not an error.
	Get(ctx context.Context, uid string) (KeyContactGrant, bool, error)

	// Put stores the grant for uid, conditional on grant.Revision: zero writes
	// only if no entry exists, non-zero writes only if the stored revision still
	// matches. Either mismatch returns a Conflict error so the caller can
	// re-read and re-evaluate rather than clobbering a concurrent write.
	Put(ctx context.Context, uid string, grant KeyContactGrant) error

	// Delete removes the entry for uid. An already-absent entry is success.
	Delete(ctx context.Context, uid string) error
}
