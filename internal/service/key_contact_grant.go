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
	if kc.Username == "" || kc.MembershipUID == "" {
		return
	}
	msg := BuildKeyContactFGAPutMessage(kc.MembershipUID, kc.Username)
	if err := p.Access(ctx, fgaconstants.GenericMemberPutSubject, msg); err != nil {
		slog.WarnContext(ctx, "key_contact FGA member_put publish failed",
			"uid", kc.UID, "membership_uid", kc.MembershipUID,
			"error", err, "publish_failed_for_backfill_repair", true)
		// The index records grants that were published. Recording one that was
		// not would make a later delete revoke a grant that never existed while
		// hiding the one that does.
		return
	}
	slog.DebugContext(ctx, "key_contact FGA member_put published",
		"uid", kc.UID, "membership_uid", kc.MembershipUID,
		"subject", fgaconstants.GenericMemberPutSubject)

	recordKeyContactGrant(ctx, p, idx, kc.UID, kc.MembershipUID, kc.Username)
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
// Every failure is logged and swallowed. The grant itself is already published,
// and failing the caller (a CDC event, a backfill page, an invite acceptance)
// would cost more than the degraded delete path that a missing entry causes.
func recordKeyContactGrant(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, uid, membershipUID, username string) {
	if idx == nil || uid == "" || membershipUID == "" || username == "" {
		return
	}

	for attempt := 1; attempt <= maxGrantIndexAttempts; attempt++ {
		stored, found, err := idx.Get(ctx, uid)
		if err != nil {
			slog.WarnContext(ctx, "key_contact grant index read failed — delete may be unable to address the revoke",
				"uid", uid, "membership_uid", membershipUID, "error", err)
			return
		}
		if found && stored.MembershipUID == membershipUID && stored.Username == username {
			return
		}

		// A zero revision on a miss is the create-only condition.
		putErr := idx.Put(ctx, uid, port.KeyContactGrant{
			MembershipUID: membershipUID,
			Username:      username,
			Revision:      stored.Revision,
		})
		if putErr == nil {
			if found {
				revokeSupersededKeyContactGrant(ctx, p, uid, stored)
			}
			return
		}
		if !pkgerrors.IsConflict(putErr) {
			slog.WarnContext(ctx, "key_contact grant index write failed — delete may be unable to address the revoke",
				"uid", uid, "membership_uid", membershipUID, "error", putErr)
			return
		}
		// Conflict: another writer changed the grant since Get. Loop back and
		// re-read/re-evaluate against the new value — nothing was revoked for
		// this candidate, so there is nothing to undo.
	}

	slog.WarnContext(ctx, "key_contact grant index write abandoned after repeated conflicts — delete may be unable to address the revoke",
		"uid", uid, "membership_uid", membershipUID, "attempts", maxGrantIndexAttempts)
}

// revokeKeyContactGrantIfUnregistered revokes and clears any grant recorded
// for uid. Call this only when a username lookup for the contact has
// definitively resolved to "no LFID" (a genuine NotFound) — never when the
// lookup was skipped (no email, no reader wired) or failed transiently. A
// definitive miss means the contact currently has no LFID, so
// PublishKeyContactFGA will not run for it and cannot revoke a grant made to
// whatever LFID it used to resolve to (a since-changed email, or a renamed/
// removed account); this closes that gap. A transient failure carries no such
// guarantee and must leave a possibly-still-valid grant untouched for the
// next attempt to retry.
func revokeKeyContactGrantIfUnregistered(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, uid string) {
	if idx == nil || uid == "" {
		return
	}
	stored, found, err := idx.Get(ctx, uid)
	if err != nil {
		slog.WarnContext(ctx, "key_contact grant index read failed while checking for revoke on unregistered email",
			"uid", uid, "error", err)
		return
	}
	if !found {
		return
	}
	revokeSupersededKeyContactGrant(ctx, p, uid, stored)
	if delErr := idx.Delete(ctx, uid, stored.Revision); delErr != nil {
		slog.WarnContext(ctx, "key_contact grant index cleanup failed after revoke on unregistered email",
			"uid", uid, "error", delErr)
	}
}

// revokeSupersededKeyContactGrant revokes a recorded grant that a newly
// published one replaces. The replacement's FGA member_put was already
// published before this call (PublishKeyContactFGA), and its index entry was
// already committed (recordKeyContactGrant calls this only after its Put
// succeeds), so the contact is never left without access, and the index never
// points at a pair that no longer has a recorded replacement.
func revokeSupersededKeyContactGrant(ctx context.Context, p port.MemberPublisher, uid string, stored port.KeyContactGrant) {
	if stored.MembershipUID == "" || stored.Username == "" {
		return
	}
	msg := BuildKeyContactFGARemoveMessage(stored.MembershipUID, stored.Username)
	if err := p.Access(ctx, fgaconstants.GenericMemberRemoveSubject, msg); err != nil {
		// Not recoverable by /admin/reindex: a reindex republishes the current
		// grant, and the superseded pair is about to be overwritten in the index,
		// so nothing will retry this revoke.
		slog.ErrorContext(ctx, "key_contact superseded grant revoke failed — dangling tuple requires manual cleanup",
			"uid", uid, "membership_uid", stored.MembershipUID,
			"error", err, "fga_revoke_failed_dangling_tuple", true)
		return
	}
	slog.InfoContext(ctx, "key_contact superseded grant revoked",
		"uid", uid, "membership_uid", stored.MembershipUID)
}
