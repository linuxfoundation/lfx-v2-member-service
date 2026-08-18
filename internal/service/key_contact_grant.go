// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// key_contact_grant.go orchestrates the FGA key_contact grant lifecycle around
// the durable grant index: publishing a grant, recording what was published so a
// later delete can address the revoke, and revoking the grant a new one
// supersedes. It lives outside messaging.go because that file is limited to pure
// message builders and thin publish wrappers with no port reads or state.

package service

import (
	"context"
	"fmt"
	"log/slog"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// maxGrantIndexAttempts bounds the compare-and-set retry when recording a grant.
// A conflict means another writer changed the grant since it was read, so the
// comparison has to be redone against the new value — but the loop must not spin
// against a contended key, and abandoning it only costs the (logged) index
// update, not the grant itself.
const maxGrantIndexAttempts = 3

// PublishKeyContactFGA emits an FGA member_put for accepted key contacts
// (non-empty username + membershipUID). Pending contacts have no FGA tuple.
// Used by the CDC consumer, the key_contact writer, the backfill runner, and the
// invite-accepted handler.
//
// It also records the published grant in idx and revokes any grant this one
// supersedes. Recording is what makes deletion revocable at all: a CDC delete
// event carries only the key contact's own SFID, and the Salesforce record is
// already gone by then, so the membership object and username cannot be
// recovered from any other source at revoke time. A nil idx skips both (mock
// mode), leaving the publish behaviour unchanged.
func PublishKeyContactFGA(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, kc *model.KeyContact) {
	_, _ = publishKeyContactFGA(ctx, p, idx, kc)
}

// publishKeyContactFGA is the error-reporting form used by CDC restoration,
// where any unrecorded or unconfirmed grant must hold the replay cursor.
func publishKeyContactFGA(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, kc *model.KeyContact) (bool, error) {
	if kc.Username == "" || kc.MembershipUID == "" {
		return false, nil
	}
	msg := BuildKeyContactFGAPutMessage(kc.MembershipUID, kc.Username)
	if err := p.Access(ctx, fgaconstants.GenericMemberPutSubject, msg); err != nil {
		slog.WarnContext(ctx, "key_contact FGA member_put publish failed",
			"uid", kc.UID, "membership_uid", kc.MembershipUID,
			"error", err, "publish_failed_for_backfill_repair", true)
		// The index records grants that were published. Recording one that was
		// not would make a later delete revoke a grant that never existed while
		// hiding the one that does.
		return false, fmt.Errorf("publish key_contact grant %s: %w", kc.UID, err)
	}
	slog.DebugContext(ctx, "key_contact FGA member_put published",
		"uid", kc.UID, "membership_uid", kc.MembershipUID,
		"subject", fgaconstants.GenericMemberPutSubject)

	return true, recordKeyContactGrant(ctx, p, idx, kc.UID, kc.MembershipUID, kc.Username)
}

// recordKeyContactGrant stores the grant just published for key contact uid and
// revokes the one it supersedes (a Salesforce-side reparent, or a changed
// username).
//
// The read-compare-write is revision-conditional: if a concurrent writer's grant
// were silently overwritten here, that writer's predecessor would never be
// revoked and the index would disagree with what was published — precisely the
// dangling grant this index exists to prevent. On conflict the comparison is
// redone against the new value.
//
// The superseded revoke fires only after the conditional Put has committed, not
// before: revoking first and then failing to record the replacement (a
// transient KV error, or conflicts exhausted) would leave the index pointing at
// a pair that was just revoked while the new grant it should describe is
// unrecorded — a later delete would revoke the stale pair again and leave the
// live tuple unaddressed. Recording first means a Put failure simply abandons
// the revoke (logged), leaving the stale index entry in place for the next
// write to reconcile, rather than leaving the index confidently wrong.
//
// The superseded pair rides along as PendingRevoke in that same Put, rather
// than being dropped once the new pair is recorded: KV writes on this index are
// already durable on return, so the instant the Put above committed without a
// PendingRevoke, the old pair's address would be gone from every durable store
// with the revoke not yet even attempted. Carrying it means a crash in that
// window still leaves the address recoverable from the index itself.
//
// Every failure is logged and returned. The exported publish wrapper preserves
// best-effort behavior for ordinary writers; CDC restoration uses the returned
// error to hold replay until the grant and its future revoke address are safe.
func recordKeyContactGrant(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, uid, membershipUID, username string) error {
	if idx == nil || uid == "" || membershipUID == "" || username == "" {
		return nil
	}

	for attempt := 1; attempt <= maxGrantIndexAttempts; attempt++ {
		stored, found, err := idx.Get(ctx, uid)
		if err != nil {
			slog.WarnContext(ctx, "key_contact grant index read failed — delete may be unable to address the revoke",
				"uid", uid, "membership_uid", membershipUID, "error", err)
			return fmt.Errorf("read key_contact grant index for %s: %w", uid, err)
		}
		if found && stored.MembershipUID == membershipUID && stored.Username == username {
			// An unchanged contact cannot prove that another key-contact record
			// does not still justify the tuple named by PendingRevoke.
			return nil
		}

		newGrant := port.KeyContactGrant{
			MembershipUID: membershipUID,
			Username:      username,
			Revision:      stored.Revision,
		}
		var superseded *port.KeyContactGrantRef
		if found {
			superseded = &port.KeyContactGrantRef{MembershipUID: stored.MembershipUID, Username: stored.Username}
			if stored.PendingRevoke != nil {
				// The previous supersede's revoke was never confirmed
				// delivered before this one landed — this requires two
				// reparents/renames of the same contact inside one Flush
				// round trip, vanishingly rare, but overwriting it here would
				// discard the only remaining address for that older grant.
				slog.ErrorContext(ctx, "key_contact grant index pending revoke overwritten by a newer supersede before it was confirmed delivered — dangling tuple requires manual cleanup",
					"uid", uid, "membership_uid", stored.PendingRevoke.MembershipUID,
					"fga_revoke_failed_dangling_tuple", true)
			}
			newGrant.PendingRevoke = superseded
		}

		putErr := idx.Put(ctx, uid, newGrant)
		if putErr == nil {
			if superseded != nil {
				return revokeSupersededKeyContactGrant(ctx, p, idx, uid, *superseded)
			}
			return nil
		}
		if !pkgerrors.IsConflict(putErr) {
			slog.WarnContext(ctx, "key_contact grant index write failed — delete may be unable to address the revoke",
				"uid", uid, "membership_uid", membershipUID, "error", putErr)
			return fmt.Errorf("write key_contact grant index for %s: %w", uid, putErr)
		}
		// Conflict: another writer changed the grant since Get. Loop back and
		// re-read/re-evaluate against the new value — nothing was revoked for
		// this candidate, so there is nothing to undo.
	}

	slog.WarnContext(ctx, "key_contact grant index write abandoned after repeated conflicts — delete may be unable to address the revoke",
		"uid", uid, "membership_uid", membershipUID, "attempts", maxGrantIndexAttempts)
	return fmt.Errorf("write key_contact grant index for %s: conflicts exhausted", uid)
}

// revokeSupersededKeyContactGrant revokes a recorded grant that a newly
// published one replaces. The replacement's FGA member_put was already
// published before this call (PublishKeyContactFGA), and its index entry —
// carrying superseded as PendingRevoke — was already committed
// (recordKeyContactGrant calls this only after its Put succeeds), so the
// contact is never left without access, and the superseded pair's address
// survives even if this call is interrupted.
//
// Access alone does not prove delivery — a nil error only means the message
// was handed to the local NATS connection. Flush closes that window: only once
// it confirms delivery is the PendingRevoke marker cleared. A crash or lost
// flush between Access and Flush leaves the address recorded rather than
// falsely claiming the revoke completed. It is not blindly retried from an
// unchanged contact because another contact may still justify the same tuple.
func revokeSupersededKeyContactGrant(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, uid string, superseded port.KeyContactGrantRef) error {
	if superseded.MembershipUID == "" || superseded.Username == "" {
		return nil
	}
	msg := BuildKeyContactFGARemoveMessage(superseded.MembershipUID, superseded.Username)
	if err := p.Access(ctx, fgaconstants.GenericMemberRemoveSubject, msg); err != nil {
		slog.ErrorContext(ctx, "key_contact superseded grant revoke publish failed — pending revoke retained in index for retry",
			"uid", uid, "membership_uid", superseded.MembershipUID,
			"error", err, "fga_revoke_failed_dangling_tuple", true)
		return fmt.Errorf("publish pending key_contact revoke for %s: %w", uid, err)
	}
	if flushErr := p.Flush(ctx); flushErr != nil {
		slog.ErrorContext(ctx, "key_contact superseded grant revoke flush failed — delivery indeterminate, pending revoke retained in index for retry",
			"uid", uid, "membership_uid", superseded.MembershipUID,
			"error", flushErr, "fga_revoke_failed_dangling_tuple", true)
		return fmt.Errorf("flush pending key_contact revoke for %s: %w", uid, flushErr)
	}
	slog.InfoContext(ctx, "key_contact superseded grant revoked",
		"uid", uid, "membership_uid", superseded.MembershipUID)

	if err := clearPendingRevoke(ctx, idx, uid, superseded); err != nil {
		slog.WarnContext(ctx, "key_contact grant index pending-revoke marker clear failed — confirmed-delivered marker left in index",
			"uid", uid, "membership_uid", superseded.MembershipUID, "error", err)
	}
	return nil
}

// clearPendingRevoke removes the PendingRevoke marker for superseded now that
// its revoke is confirmed delivered. It is best-effort: leaving a
// confirmed-delivered marker in place after this fails is stale but harmless
// (the grant it names really was revoked), unlike leaving it in place because
// delivery was never confirmed.
func clearPendingRevoke(ctx context.Context, idx port.KeyContactGrantIndex, uid string, superseded port.KeyContactGrantRef) error {
	for attempt := 1; attempt <= maxGrantIndexAttempts; attempt++ {
		current, found, err := idx.Get(ctx, uid)
		if err != nil {
			return fmt.Errorf("read key_contact pending revoke for %s: %w", uid, err)
		}
		if !found {
			return nil
		}
		if current.PendingRevoke == nil || *current.PendingRevoke != superseded {
			// Already cleared, or superseded by a marker for a different
			// grant — leave that one alone rather than clobbering it.
			return nil
		}
		current.PendingRevoke = nil
		putErr := idx.Put(ctx, uid, current)
		if putErr == nil {
			return nil
		}
		if !pkgerrors.IsConflict(putErr) {
			return fmt.Errorf("clear key_contact pending revoke for %s: %w", uid, putErr)
		}
		// Conflict: re-read and retry against the new value.
	}
	return fmt.Errorf("clear key_contact pending revoke for %s: conflicts exhausted", uid)
}
