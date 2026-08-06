// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// keyContactGrantPrefix is the key namespace for published grant records.
// Full key form: "key_contact.{sfid}".
const keyContactGrantPrefix = "key_contact."

// keyContactGrantValue is the stored payload. The key contact's own UID lives in
// the key; the value carries only what a later revoke needs.
type keyContactGrantValue struct {
	MembershipUID string `json:"membership_uid"`
	Username      string `json:"username"`

	// PendingRevokeMembershipUID/PendingRevokeUsername mirror
	// port.KeyContactGrant.PendingRevoke — a superseded pair whose
	// member_remove has not yet been confirmed delivered. omitempty so a
	// grant with no pending revoke serializes the same as before this field
	// existed.
	PendingRevokeMembershipUID string `json:"pending_revoke_membership_uid,omitempty"`
	PendingRevokeUsername      string `json:"pending_revoke_username,omitempty"`
}

// KeyContactGrantIndex implements port.KeyContactGrantIndex over the
// key-contact-grants NATS KV bucket (authoritative, no TTL, history 1).
type KeyContactGrantIndex struct {
	client *NATSClient
}

// NewKeyContactGrantIndex creates a KeyContactGrantIndex backed by the given
// NATS client. The bucket must have been initialised via KeyValueStore.
func NewKeyContactGrantIndex(client *NATSClient) *KeyContactGrantIndex {
	return &KeyContactGrantIndex{client: client}
}

// Ensure KeyContactGrantIndex satisfies the port at compile time.
var _ port.KeyContactGrantIndex = (*KeyContactGrantIndex)(nil)

// grantKey builds the "key_contact.{sfid}" key.
func grantKey(uid string) string {
	return keyContactGrantPrefix + uid
}

// kv returns the key-contact-grants KV handle or an error if it was not
// initialised.
func (s *KeyContactGrantIndex) kv() (jetstream.KeyValue, error) {
	kv, ok := s.client.kvStore[constants.KVBucketNameKeyContactGrants]
	if !ok {
		return nil, errs.NewUnexpected(fmt.Sprintf("KV bucket %q not initialized", constants.KVBucketNameKeyContactGrants))
	}
	return kv, nil
}

// Get returns the stored grant for uid. A missing entry reports found=false
// without an error: it means no grant has been published for this contact (or
// the entry predates the index), which callers handle rather than fail on.
func (s *KeyContactGrantIndex) Get(ctx context.Context, uid string) (port.KeyContactGrant, bool, error) {
	if uid == "" {
		return port.KeyContactGrant{}, false, errs.NewValidation("key-contact-grants: uid is required")
	}
	kv, err := s.kv()
	if err != nil {
		return port.KeyContactGrant{}, false, err
	}

	entry, getErr := kv.Get(ctx, grantKey(uid))
	if getErr != nil {
		if errors.Is(getErr, jetstream.ErrKeyNotFound) {
			return port.KeyContactGrant{}, false, nil
		}
		return port.KeyContactGrant{}, false, errs.NewUnexpected(
			fmt.Sprintf("key-contact-grants: failed to read grant for %s", uid), getErr)
	}

	var val keyContactGrantValue
	if unmarshalErr := json.Unmarshal(entry.Value(), &val); unmarshalErr != nil {
		// A malformed value cannot be used to address a revoke, so report empty
		// fields rather than garbage — the caller's revoke guard (empty
		// MembershipUID/Username) then skips publishing from it. found=true with
		// the real revision is required for this to heal: a create-only Put
		// (revision 0) would collide with this still-existing key and fail as
		// Conflict every retry, but a CAS Update against entry.Revision()
		// overwrites it with the next good value.
		slog.WarnContext(ctx, "key-contact-grants: malformed stored grant value, treating fields as empty",
			"uid", uid, "error", unmarshalErr)
		return port.KeyContactGrant{Revision: entry.Revision()}, true, nil
	}

	grant := port.KeyContactGrant{
		MembershipUID: val.MembershipUID,
		Username:      val.Username,
		Revision:      entry.Revision(),
	}
	if val.PendingRevokeMembershipUID != "" && val.PendingRevokeUsername != "" {
		grant.PendingRevoke = &port.KeyContactGrantRef{
			MembershipUID: val.PendingRevokeMembershipUID,
			Username:      val.PendingRevokeUsername,
		}
	}
	return grant, true, nil
}

// Put stores the grant for uid, conditional on grant.Revision. A revision of
// zero creates the entry only if absent; a non-zero revision updates only if the
// stored revision still matches. Both mismatches return Conflict so the caller
// can re-read and re-evaluate the grant rather than overwrite a concurrent
// change with a stale pair.
func (s *KeyContactGrantIndex) Put(ctx context.Context, uid string, grant port.KeyContactGrant) error {
	if uid == "" {
		return errs.NewValidation("key-contact-grants: uid is required")
	}
	if grant.MembershipUID == "" || grant.Username == "" {
		// An entry without both halves cannot address a revoke, and no grant is
		// ever published without both.
		return errs.NewValidation(
			fmt.Sprintf("key-contact-grants: membership_uid and username are required for %s", uid))
	}
	kv, err := s.kv()
	if err != nil {
		return err
	}

	value := keyContactGrantValue{
		MembershipUID: grant.MembershipUID,
		Username:      grant.Username,
	}
	if grant.PendingRevoke != nil {
		value.PendingRevokeMembershipUID = grant.PendingRevoke.MembershipUID
		value.PendingRevokeUsername = grant.PendingRevoke.Username
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return errs.NewUnexpected(fmt.Sprintf("key-contact-grants: failed to marshal grant for %s", uid), marshalErr)
	}

	key := grantKey(uid)
	if grant.Revision == 0 {
		if _, createErr := kv.Create(ctx, key, data); createErr != nil {
			if errors.Is(createErr, jetstream.ErrKeyExists) {
				return errs.NewConflict(fmt.Sprintf("key-contact-grants: grant for %s was created concurrently", uid))
			}
			return errs.NewUnexpected(fmt.Sprintf("key-contact-grants: failed to create grant for %s", uid), createErr)
		}
		return nil
	}

	if _, updateErr := kv.Update(ctx, key, data, grant.Revision); updateErr != nil {
		// ErrKeyExists means a "wrong last sequence" rejection — i.e. a
		// revision mismatch because another writer changed the grant since the
		// read this call was built on. ErrKeyNotFound means the entry was
		// deleted since that read (e.g. the CDC delete path raced this write) —
		// also stale-read, not a real failure: the caller's CAS loop re-reads,
		// finds no entry, and retries as a create. Anything else (a transient
		// NATS/network failure, context cancellation) is not a conflict:
		// mapping it to Conflict would make the retry loop burn its attempts
		// re-reading the same unchanged value against an outage, republish the
		// superseded revoke on every attempt, and finally log "abandoned after
		// repeated conflicts" for what was never a conflict — discarding the
		// real cause.
		if errors.Is(updateErr, jetstream.ErrKeyExists) || errors.Is(updateErr, jetstream.ErrKeyNotFound) {
			return errs.NewConflict(fmt.Sprintf("key-contact-grants: grant for %s changed since read", uid))
		}
		return errs.NewUnexpected(fmt.Sprintf("key-contact-grants: failed to update grant for %s", uid), updateErr)
	}
	return nil
}

// Delete revision-conditionally removes the entry for uid, mirroring
// CDCRepairStore.DeletePending: a revision mismatch (the entry was rewritten
// since the caller's read — e.g. a re-invite racing this delete) is reported
// as Conflict so the newer grant survives instead of being tombstoned. An
// already-absent entry is success: the contact is gone either way, and the
// caller must not fail a delete over it.
//
// revision == 0 (the port's documented "not currently stored" value, e.g. a
// caller that read no entry) is a no-op: it returns immediately without
// calling NATS. This is required, not just an optimization — jetstream.go's
// KV Delete only sets the expected-sequence header when revision != 0
// (jetstream/kv.go), so LastRevision(0) is silently unconditional and would
// delete whatever is currently stored, tombstoning a grant written
// concurrently since the caller's read. A zero revision means the caller
// never saw an entry to delete in the first place, so skipping is correct
// even without that pitfall.
func (s *KeyContactGrantIndex) Delete(ctx context.Context, uid string, revision uint64) error {
	if uid == "" {
		return errs.NewValidation("key-contact-grants: uid is required")
	}
	if revision == 0 {
		return nil
	}
	kv, err := s.kv()
	if err != nil {
		return err
	}
	if delErr := kv.Delete(ctx, grantKey(uid), jetstream.LastRevision(revision)); delErr != nil {
		if errors.Is(delErr, jetstream.ErrKeyNotFound) {
			return nil
		}
		// Any other error (notably a last-revision mismatch) means the grant
		// was rewritten since the caller's read.
		return errs.NewConflict(fmt.Sprintf("key-contact-grants: grant for %s changed since read", uid))
	}
	return nil
}
