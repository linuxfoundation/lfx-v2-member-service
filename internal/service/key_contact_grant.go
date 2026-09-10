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
	"errors"
	"fmt"
	"log/slog"
	"strings"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// maxGrantIndexAttempts bounds the compare-and-set retry when recording a grant.
// A conflict means another writer changed the grant since it was read, so the
// comparison has to be redone against the new value — but the loop must not spin
// against a contended key, and abandoning it only costs the (logged) index
// update, not the grant itself.
const maxGrantIndexAttempts = 3

// Reasons passed to revokeKeyContactGrantIfNoLongerLive, defined once so its
// several call sites can't drift out of sync with each other.
const (
	reasonEmailUnregistered = "email resolved no registered account"
	reasonInactiveStatus    = "key contact status is Inactive"
)

// membershipKeyContactLister lists a membership's key contacts, letting an
// Inactive revoke check whether a sibling still justifies the tuple.
type membershipKeyContactLister interface {
	ListKeyContactsForMembership(ctx context.Context, membershipUID string) ([]*model.KeyContact, error)
}

// keyContactsByMembershipLister adapts port.KeyContactsByMembershipReader to
// membershipKeyContactLister for one membership. Unlike port.MemberReader, its
// backing fetch is never served from the stale membership-group cache.
type keyContactsByMembershipLister struct {
	reader port.KeyContactsByMembershipReader
}

func (l keyContactsByMembershipLister) ListKeyContactsForMembership(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	grouped, err := l.reader.FetchKeyContactsByAssetSFIDs(ctx, []string{membershipUID})
	if err != nil {
		return nil, err
	}
	return grouped[membershipUID], nil
}

// siblingListerFor wraps r for the revoke sibling check, or returns nil when
// no reader is wired (disabling the check). A non-nil users adds the
// email-to-username resolution keyContactPairJustified needs for pairs whose
// email is unknown.
func siblingListerFor(r port.KeyContactsByMembershipReader, users usernameByEmailResolver) membershipKeyContactLister {
	if r == nil {
		return nil
	}
	return withEmailResolver(keyContactsByMembershipLister{reader: r}, users)
}

// usernameByEmailResolver is the single capability keyContactPairJustified
// type-asserts for on a sibling lister; port.UserReader carries more methods
// than a lister wrapper should have to forward.
type usernameByEmailResolver interface {
	UsernameByEmail(ctx context.Context, email string) (string, error)
}

// resolvingSiblingLister pairs a sibling lister with an email-to-username
// resolver, exposing the usernameByEmailResolver capability
// keyContactPairJustified asserts for. users only needs to satisfy the narrow
// resolver interface, not the full port.UserReader: port.UserReader values
// satisfy it automatically, and a caller with only a resolver (e.g. one
// tied to a single accepted invite) can plug in without implementing the rest.
type resolvingSiblingLister struct {
	membershipKeyContactLister
	users usernameByEmailResolver
}

func (l resolvingSiblingLister) UsernameByEmail(ctx context.Context, email string) (string, error) {
	return l.users.UsernameByEmail(ctx, email)
}

// withEmailResolver adds the email-to-username resolution capability to a
// sibling lister; a nil lister or users leaves it unchanged.
func withEmailResolver(lister membershipKeyContactLister, users usernameByEmailResolver) membershipKeyContactLister {
	if lister == nil || users == nil {
		return lister
	}
	return resolvingSiblingLister{membershipKeyContactLister: lister, users: users}
}

// errSiblingScanUncovered reports a sibling lookup for a membership the
// lister's prefetched data does not cover, making the scan inconclusive.
var errSiblingScanUncovered = errors.New("membership not covered by prefetched sibling data")

// coverageAwareLister serves covered memberships from inner. An uncovered one
// goes to fallback, or errors so the scan reads inconclusive, not falsely certain.
type coverageAwareLister struct {
	covered  map[string]struct{}
	inner    membershipKeyContactLister
	fallback membershipKeyContactLister
}

func (l coverageAwareLister) ListKeyContactsForMembership(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	if _, ok := l.covered[membershipUID]; ok {
		return l.inner.ListKeyContactsForMembership(ctx, membershipUID)
	}
	if l.fallback != nil {
		return l.fallback.ListKeyContactsForMembership(ctx, membershipUID)
	}
	return nil, fmt.Errorf("%w: %s", errSiblingScanUncovered, membershipUID)
}

// sliceSiblingLister adapts an already-fetched contacts slice. A membership
// absent from the slice reads as uncovered: served live via reader, else inconclusive.
func sliceSiblingLister(contacts []*model.KeyContact, reader port.KeyContactsByMembershipReader, users usernameByEmailResolver) membershipKeyContactLister {
	covered := make(map[string]struct{}, len(contacts))
	for _, kc := range contacts {
		if kc.MembershipUID != "" {
			covered[kc.MembershipUID] = struct{}{}
		}
	}
	var fallback membershipKeyContactLister
	if reader != nil {
		fallback = keyContactsByMembershipLister{reader: reader}
	}
	return withEmailResolver(coverageAwareLister{
		covered:  covered,
		inner:    sliceKeyContactLister(contacts),
		fallback: fallback,
	}, users)
}

// PublishKeyContactFGA emits an FGA member_put for accepted key contacts
// (non-empty username + membershipUID). Pending contacts have no FGA tuple.
// Used by the CDC consumer, the key_contact writer, the backfill runner, and the
// invite-accepted handler.
//
// Inactive contacts are revoked unless a sibling record on the membership
// still resolves to the same person, in which case only this entry is
// cleared.
//
// It also records the published grant in idx and revokes any grant this one
// supersedes. Recording is what makes deletion revocable at all: a CDC delete
// event carries only the key contact's own SFID, and the Salesforce record is
// already gone by then, so the membership object and username cannot be
// recovered from any other source at revoke time. A nil idx skips both (mock
// mode), leaving the publish behaviour unchanged.
//
// lister may be nil, which disables the sibling check.
//
// recheck is an optional trailing live sibling lister (never a snapshot),
// re-run after a remove publishes to catch a different-UID regrant racing the
// scan against lister. Omit it where no live reader is available; passing it
// is variadic only so existing callers compile unchanged.
func PublishKeyContactFGA(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, kc *model.KeyContact, lister membershipKeyContactLister, recheck ...membershipKeyContactLister) {
	var live membershipKeyContactLister
	if len(recheck) > 0 {
		live = recheck[0]
	}
	_, _ = publishKeyContactFGA(ctx, p, idx, kc, lister, live)
}

// publishKeyContactFGA is the error-reporting form used by CDC restoration,
// where any unrecorded or unconfirmed grant must hold the replay cursor.
// recheck must be a LIVE lister, or nil where none can be constructed.
func publishKeyContactFGA(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, kc *model.KeyContact, lister, recheck membershipKeyContactLister) (bool, error) {
	if strings.EqualFold(kc.Status, constants.RoleStatusInactive) {
		// An inactive contact must never hold a live tuple: revoke any
		// recorded grant instead of publishing a put.
		if idx != nil {
			stored, found, err := idx.Get(ctx, kc.UID)
			if err != nil {
				slog.WarnContext(ctx, "key_contact grant index read failed: skipping revoke to avoid stripping a possibly still-justified tuple",
					"uid", kc.UID, "membership_uid", kc.MembershipUID, "error", err)
				return false, fmt.Errorf("read key_contact grant index for %s: %w", kc.UID, err)
			}
			if found && stored.PendingRevoke != nil {
				// Deactivation is the last scheduled visit to this entry: drain
				// the superseded pair's revoke now or it stays orphaned. A
				// failed or uncertain drain keeps the marker as the address,
				// so the CDC replay cursor must hold until it is confirmed.
				if drainErr := revokeSupersededKeyContactGrant(ctx, p, idx, lister, recheck, kc.UID, *stored.PendingRevoke); drainErr != nil {
					return false, fmt.Errorf("drain superseded key_contact grant for %s: %w", kc.UID, drainErr)
				}
			}
			usable := found && stored.MembershipUID != "" && stored.Username != ""
			if kc.MembershipUID != "" && kc.Email != "" && !usable {
				// A cold index (a miss, or a marker-only pair already cleared)
				// leaves the record's own pair as the only revocable address.
				if kc.Username != "" {
					outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, p, lister, keyContactPairRevoke{
						membershipUID: kc.MembershipUID,
						username:      kc.Username,
						excludeUID:    kc.UID,
						email:         kc.Email,
						reason:        reasonInactiveStatus,
						flush:         true,
						recheck:       recheck,
					})
					switch outcome {
					case revokeUncertain, revokeFailed:
						return false, fmt.Errorf("revoke inactive key_contact pair for %s: %w", kc.UID, revokeErr)
					case revokeUnneeded:
						if justifiedBy != nil && !pairDurablyOwned(ctx, idx, justifiedBy, kc.MembershipUID, kc.Username) {
							return false, fmt.Errorf("transfer durable revoke address for inactive key_contact %s", kc.UID)
						}
					}
				}
				// Do not persist an index entry for the record's own pair: the
				// record still exists in Salesforce, so CDC or backfill re-touch
				// retries this path again.
				return false, nil
			}
			if usable && kc.Username != "" && kc.MembershipUID != "" &&
				(stored.MembershipUID != kc.MembershipUID || stored.Username != kc.Username) {
				// The indexed pair is stale: an earlier reparent or rename
				// published the current pair's member_put, but the index
				// update that would have recorded it failed. The current
				// pair is unindexed, so this revoke is its only address; it
				// runs before the stored pair below, which may clear or
				// claim the entry.
				outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, p, lister, keyContactPairRevoke{
					membershipUID: kc.MembershipUID,
					username:      kc.Username,
					excludeUID:    kc.UID,
					email:         kc.Email,
					reason:        reasonInactiveStatus,
					flush:         true,
					recheck:       recheck,
				})
				switch outcome {
				case revokeUncertain, revokeFailed:
					// The stored pair's own revoke can wait for redelivery.
					return false, fmt.Errorf("revoke inactive key_contact current pair for %s: %w", kc.UID, revokeErr)
				case revokeUnneeded:
					if justifiedBy != nil && !pairDurablyOwned(ctx, idx, justifiedBy, kc.MembershipUID, kc.Username) {
						return false, fmt.Errorf("transfer durable revoke address for inactive key_contact %s current pair", kc.UID)
					}
				}
			}
		}
		if err := revokeKeyContactGrantIfNoLongerLive(ctx, p, idx, lister, recheck, kc.UID, kc.Username, kc.Email, reasonInactiveStatus); err != nil {
			return false, fmt.Errorf("revoke inactive key_contact stored pair for %s: %w", kc.UID, err)
		}
		return false, nil
	}
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

	return true, recordKeyContactGrant(ctx, p, idx, lister, recheck, kc.UID, kc.MembershipUID, kc.Username)
}

// keyContactPairRevoke describes one FGA key_contact pair a caller believes
// is no longer justified, and how its publish should behave.
type keyContactPairRevoke struct {
	membershipUID string // FGA object of the pair, and the sibling-scan scope
	username      string // FGA user of the pair
	excludeUID    string // the key contact record being removed or deactivated
	email         string // email known to belong to username; "" when unknown
	reason        string // logged only
	flush         bool   // confirm broker delivery before reporting success

	// recheck, when non-nil, is a LIVE (never prefetched) sibling lister used to
	// re-run justification after the remove publishes, closing the window where
	// a different contact UID grants the same pair between the scan and the
	// publish. Leave nil when no live reader is available (invite path) or the
	// caller cannot supply one (revokeSupersededKeyContactGrant).
	recheck membershipKeyContactLister
}

// keyContactRevokeOutcome reports what revokeKeyContactPairIfUnjustified did.
type keyContactRevokeOutcome int

const (
	// revokePublished: the pair proved unjustified and the remove went out.
	revokePublished keyContactRevokeOutcome = iota
	// revokeUnneeded: a live sibling still justifies the pair, or there is
	// no username to revoke.
	revokeUnneeded
	// revokeUncertain: the sibling scan or resolution failed; nothing was
	// published, failing safe.
	revokeUncertain
	// revokeFailed: the pair proved unjustified but publish or flush failed.
	revokeFailed
)

// errSiblingUnresolvable reports a live sibling on the membership whose email
// could not be checked against req.username because the lister carries no
// email resolver, making the scan inconclusive rather than falsely certain.
var errSiblingUnresolvable = errors.New("live sibling could not be resolved to a username")

// keyContactPairJustified reports the live (non-Inactive) sibling record on
// the membership, excluding the record being removed, that still justifies
// the pair: by carrying req.email when it is known, or by an email that
// resolves to req.username when the lister exposes usernameByEmailResolver. A
// nil sibling means not justified. An error means uncertainty and the caller
// must not revoke. A resolver NotFound error is a definitive miss and does
// not itself cause uncertainty; an eligible sibling that cannot be checked at
// all (no resolver wired) does.
func keyContactPairJustified(ctx context.Context, lister membershipKeyContactLister, req keyContactPairRevoke) (*model.KeyContact, error) {
	if lister == nil || req.username == "" {
		return nil, nil
	}
	siblings, err := lister.ListKeyContactsForMembership(ctx, req.membershipUID)
	if err != nil {
		return nil, err
	}
	resolver, _ := lister.(usernameByEmailResolver)
	unresolvable := false
	for _, sib := range siblings {
		if sib.UID == req.excludeUID || sib.Email == "" ||
			strings.EqualFold(sib.Status, constants.RoleStatusInactive) {
			continue
		}
		if req.email != "" && strings.EqualFold(sib.Email, req.email) {
			return sib, nil
		}
		if resolver == nil {
			// An eligible sibling that cannot be checked reads as "possibly
			// this person", not "certainly a different person".
			unresolvable = true
			continue
		}
		resolved, resolveErr := resolver.UsernameByEmail(ctx, sib.Email)
		if resolveErr != nil {
			if pkgerrors.IsNotFound(resolveErr) {
				continue
			}
			return nil, resolveErr
		}
		if resolved == req.username {
			return sib, nil
		}
	}
	if unresolvable {
		return nil, fmt.Errorf("key_contact sibling scan for %s: %w", req.membershipUID, errSiblingUnresolvable)
	}
	return nil, nil
}

// publishKeyContactRemove is the only function that emits a key_contact FGA
// member_remove. Every caller reaches it through the justification owners
// (revokeKeyContactPairIfUnjustified or revokeKeyContactGrantIfNoLongerLive),
// except the CDC legacy unindexed fallback, which publishes a knowingly
// unaddressable remove.
func publishKeyContactRemove(ctx context.Context, p port.MemberPublisher, req keyContactPairRevoke) error {
	msg := BuildKeyContactFGARemoveMessage(req.membershipUID, req.username)
	if err := p.Access(ctx, fgaconstants.GenericMemberRemoveSubject, msg); err != nil {
		slog.ErrorContext(ctx, "key_contact FGA member_remove publish failed",
			"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason,
			"error", err, "fga_revoke_failed_dangling_tuple", true)
		return fmt.Errorf("publish key_contact revoke for %s: %w", req.excludeUID, err)
	}
	if !req.flush {
		return nil
	}
	if flushErr := p.Flush(ctx); flushErr != nil {
		slog.ErrorContext(ctx, "key_contact FGA member_remove flush failed: delivery indeterminate",
			"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason,
			"error", flushErr, "fga_revoke_failed_dangling_tuple", true)
		return fmt.Errorf("flush key_contact revoke for %s: %w", req.excludeUID, flushErr)
	}
	slog.InfoContext(ctx, "key_contact FGA member_remove published",
		"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason)
	return nil
}

// revokeKeyContactPairIfUnjustified is the choke point for key_contact FGA
// revokes that are not driven by a recorded grant-index entry: it owns the
// sibling-justification decision, the fail-safe on an uncertain scan, and the
// publish. Index bookkeeping stays with the caller, which alone knows whether
// an entry may be cleared for each outcome. The returned error is non-nil for
// revokeUncertain and revokeFailed. justifiedBy is the sibling that justified
// the pair on revokeUnneeded, and nil for every other outcome.
//
// When req.recheck is set, a successful publish is followed by re-running the
// scan against that live lister: a different contact UID can grant the same
// pair between the first scan and this publish, and the remove would
// otherwise strip access that was just re-granted. A recheck that finds the
// pair justified again publishes a compensating member_put. If that repair
// (and its flush) succeeds, the net effect of the remove plus the repair is a
// live tuple justified by the racing sibling, so the outcome is
// revokeUnneeded with justifiedBy set to that sibling, not revokePublished:
// every caller already runs the durable-ownership transfer on revokeUnneeded
// and preserves retry state if that transfer fails, which is exactly what
// must happen here before the original entry can be cleared. If the recheck
// read fails, or the racing sibling is found but the compensating put or its
// flush fails, the outcome is revokeUncertain instead: the caller must
// preserve retry state rather than report a revoke that may have stripped
// live access.
func revokeKeyContactPairIfUnjustified(ctx context.Context, p port.MemberPublisher, lister membershipKeyContactLister, req keyContactPairRevoke) (keyContactRevokeOutcome, *model.KeyContact, error) {
	if req.username == "" {
		return revokeUnneeded, nil, nil
	}
	justifiedBy, err := keyContactPairJustified(ctx, lister, req)
	if err != nil {
		slog.WarnContext(ctx, "key_contact sibling scan failed: skipping revoke to avoid stripping a possibly still-justified tuple",
			"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason, "error", err)
		return revokeUncertain, nil, fmt.Errorf("sibling scan for key_contact %s: %w", req.excludeUID, err)
	}
	if justifiedBy != nil {
		slog.DebugContext(ctx, "key_contact pair still justified by a live sibling: skipping revoke",
			"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason)
		return revokeUnneeded, justifiedBy, nil
	}
	if pubErr := publishKeyContactRemove(ctx, p, req); pubErr != nil {
		return revokeFailed, nil, pubErr
	}
	if req.recheck != nil {
		recheckRevoke := req
		recheckRevoke.recheck = nil
		racedBy, recheckErr := keyContactPairJustified(ctx, req.recheck, recheckRevoke)
		if recheckErr != nil {
			slog.ErrorContext(ctx, "key_contact post-revoke recheck failed: tuple may be incorrectly absent until the next backfill",
				"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason,
				"error", recheckErr, "fga_remove_raced_possible_lost_grant", true)
			return revokeUncertain, nil, fmt.Errorf("post-revoke recheck for key_contact %s: %w", req.excludeUID, recheckErr)
		}
		if racedBy != nil {
			repairMsg := BuildKeyContactFGAPutMessage(req.membershipUID, req.username)
			repairErr := p.Access(ctx, fgaconstants.GenericMemberPutSubject, repairMsg)
			if repairErr == nil {
				repairErr = p.Flush(ctx)
			}
			if repairErr != nil {
				slog.ErrorContext(ctx, "key_contact post-revoke repair failed: tuple may be incorrectly absent until the next backfill",
					"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason,
					"error", repairErr, "fga_remove_raced_possible_lost_grant", true)
				return revokeUncertain, racedBy, fmt.Errorf("post-revoke repair for key_contact %s: %w", req.excludeUID, repairErr)
			}
			slog.WarnContext(ctx, "key_contact grant repaired: a concurrent grant raced this revoke and was reapplied",
				"uid", req.excludeUID, "membership_uid", req.membershipUID, "reason", req.reason)
			// The remove plus a successful compensating put nets out to a live
			// tuple justified by the racing sibling: report the same outcome
			// as if the scan had found it justified up front, so the caller
			// runs the durable-ownership transfer before clearing this entry.
			return revokeUnneeded, racedBy, nil
		}
	}
	return revokePublished, nil, nil
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
//
// recheck must be a LIVE lister, or nil where none can be constructed; it is
// threaded through to the superseded-grant revoke so a different-UID regrant
// racing the (possibly snapshot) lister scan is still caught.
func recordKeyContactGrant(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, lister, recheck membershipKeyContactLister, uid, membershipUID, username string) error {
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
			if stored.PendingRevoke != nil {
				slog.ErrorContext(ctx, "key_contact pending revoke requires reference-aware manual recovery",
					"uid", uid,
					"membership_uid", stored.PendingRevoke.MembershipUID,
					"fga_revoke_failed_dangling_tuple", true,
					"manual_recovery_required", true)
			}
			// Re-confirming an unchanged pair still advances the index
			// revision: revokeKeyContactGrantIfNoLongerLive claims the entry
			// (a CAS rewrite conditional on the revision it read) before
			// publishing a revoke, specifically to detect a concurrent
			// same-pair re-grant like this one. Without this touch, an
			// unchanged pair would never move the revision, so that claim
			// would see the same stale revision and could not tell a fresh
			// reconfirmation apart from no concurrent activity at all,
			// letting a stale revoke through. A conflict here just means
			// another writer already touched or replaced the entry — this
			// call's job (confirming the pair is live) is already done.
			if putErr := idx.Put(ctx, uid, stored); putErr != nil && !pkgerrors.IsConflict(putErr) {
				slog.WarnContext(ctx, "key_contact grant index touch failed on unchanged pair — a concurrent revoke may not detect this reconfirmation",
					"uid", uid, "membership_uid", membershipUID, "error", putErr)
			}
			return nil
		}

		newGrant := port.KeyContactGrant{
			MembershipUID: membershipUID,
			Username:      username,
			Revision:      stored.Revision,
		}
		var superseded *port.KeyContactGrantRef
		if found && stored.MembershipUID == "" && stored.Username == "" {
			// Marker-only entry (its live pair was already revoked and
			// cleared; only a still-outstanding PendingRevoke for a
			// different, unrelated pair remains). There is no live pair
			// here for this new grant to supersede — carry the marker
			// forward untouched rather than manufacturing a superseded ref
			// from the empty pair, which would overwrite the only address
			// for that still-outstanding revoke with an empty one.
			newGrant.PendingRevoke = stored.PendingRevoke
		} else if found {
			superseded = &port.KeyContactGrantRef{MembershipUID: stored.MembershipUID, Username: stored.Username}
			if stored.PendingRevoke != nil &&
				(stored.PendingRevoke.MembershipUID != membershipUID || stored.PendingRevoke.Username != username) {
				// A second supersede landed before the previous marker's
				// revoke was confirmed. Its slot is needed for the pair
				// superseded now, so drain it first: overwriting it would
				// discard the old pair's only revoke address.
				if drainErr := drainKeyContactPendingRevoke(ctx, p, idx, lister, recheck, uid,
					*stored.PendingRevoke, "key contact grant superseded twice"); drainErr != nil {
					return fmt.Errorf("drain pending revoke before new supersede for %s: %w", uid, drainErr)
				}
			}
			// A marker naming the incoming live pair is re-justified by this
			// very grant: drop it without revoking, the entry's live pair
			// becomes its durable address again.
			newGrant.PendingRevoke = superseded
		}

		putErr := idx.Put(ctx, uid, newGrant)
		if putErr == nil {
			if superseded != nil {
				return revokeSupersededKeyContactGrant(ctx, p, idx, lister, recheck, uid, *superseded)
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
//
// The superseded pair's email is unknown here, so justification runs by
// resolution alone. On an uncertain scan the marker is kept and an error
// returned, holding CDC replay; a justified pair clears the marker unpublished.
//
// On revokeUnneeded, a live sibling justifies the pair, but the marker is the
// pair's only durable address until that sibling's own index entry is
// confirmed to cover it (pairDurablyOwned): otherwise clearing the marker
// would drop the only durable address for a pair that still has a live tuple.
//
// recheck must be a LIVE lister, or nil where none can be constructed; it
// closes the window where a different-UID regrant races this revoke's publish.
func revokeSupersededKeyContactGrant(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, lister, recheck membershipKeyContactLister, uid string, superseded port.KeyContactGrantRef) error {
	if superseded.MembershipUID == "" || superseded.Username == "" {
		return nil
	}
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, p, lister, keyContactPairRevoke{
		membershipUID: superseded.MembershipUID,
		username:      superseded.Username,
		excludeUID:    uid,
		reason:        "superseded by a new grant",
		flush:         true,
		recheck:       recheck,
	})
	switch outcome {
	case revokeUncertain:
		return fmt.Errorf("verify superseded key_contact pair for %s: %w", uid, revokeErr)
	case revokeFailed:
		return fmt.Errorf("pending key_contact revoke for %s: %w", uid, revokeErr)
	case revokeUnneeded:
		if justifiedBy != nil && idx != nil &&
			!pairDurablyOwned(ctx, idx, justifiedBy, superseded.MembershipUID, superseded.Username) {
			return fmt.Errorf("transfer durable revoke address for superseded key_contact %s", uid)
		}
	}

	if err := clearPendingRevoke(ctx, idx, uid, superseded); err != nil {
		slog.WarnContext(ctx, "key_contact grant index pending-revoke marker clear failed — confirmed-delivered marker left in index",
			"uid", uid, "membership_uid", superseded.MembershipUID, "error", err)
	}
	return nil
}

// reassertKeyContactPendingRevokePair publishes a confirmed (flushed)
// member_put reasserting a PendingRevoke marker's pair before that marker's
// ownership transfers or is dropped. A pending-revoke marker means a remove
// may have been published for the pair without a confirmed compensating
// repair, so the tuple's live presence is unknown; reasserting it is
// idempotent and guarantees the repair executes on this retry path instead of
// depending on some future unrelated touch. A failed put or flush returns an
// error so the caller preserves the marker as the retry address.
func reassertKeyContactPendingRevokePair(ctx context.Context, p port.MemberPublisher, excludeUID, membershipUID, username string) error {
	msg := BuildKeyContactFGAPutMessage(membershipUID, username)
	if err := p.Access(ctx, fgaconstants.GenericMemberPutSubject, msg); err != nil {
		slog.ErrorContext(ctx, "key_contact pending revoke marker reassert publish failed",
			"uid", excludeUID, "membership_uid", membershipUID, "error", err, "fga_remove_raced_possible_lost_grant", true)
		return fmt.Errorf("reassert key_contact pending revoke pair for %s: %w", excludeUID, err)
	}
	if err := p.Flush(ctx); err != nil {
		slog.ErrorContext(ctx, "key_contact pending revoke marker reassert flush failed",
			"uid", excludeUID, "membership_uid", membershipUID, "error", err, "fga_remove_raced_possible_lost_grant", true)
		return fmt.Errorf("flush key_contact pending revoke reassert for %s: %w", excludeUID, err)
	}
	slog.InfoContext(ctx, "key_contact pending revoke marker reasserted before drop",
		"uid", excludeUID, "membership_uid", membershipUID)
	return nil
}

// drainKeyContactPendingRevoke revokes marker's pair ahead of an index entry
// clear, so an unrelated PendingRevoke is never dropped unaddressed by
// whichever caller is about to remove or rewrite the entry that carries it.
// It does not touch the index entry itself; the caller decides how to clear
// or preserve it based on the returned error. A nil error means the marker's
// pair is either revoked-and-confirmed or still justified by a durably owned
// sibling; a non-nil error means the marker must be preserved as the retry
// address.
//
// On the justified branch, the marker's pair is reasserted with a confirmed
// member_put before ownership transfers: see reassertKeyContactPendingRevokePair.
//
// recheck must be a LIVE lister, or nil where none can be constructed; unlike
// lister, which may be a snapshot, recheck is what actually closes the
// different-UID regrant race, so callers must not pass a snapshot for it.
func drainKeyContactPendingRevoke(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, lister, recheck membershipKeyContactLister, excludeUID string, marker port.KeyContactGrantRef, reason string) error {
	outcome, justifiedBy, revokeErr := revokeKeyContactPairIfUnjustified(ctx, p, lister, keyContactPairRevoke{
		membershipUID: marker.MembershipUID,
		username:      marker.Username,
		excludeUID:    excludeUID,
		reason:        reason,
		flush:         true,
		recheck:       recheck,
	})
	switch outcome {
	case revokeUncertain, revokeFailed:
		return fmt.Errorf("drain key_contact pending revoke marker for %s: %w", excludeUID, revokeErr)
	case revokeUnneeded:
		if justifiedBy != nil {
			if reassertErr := reassertKeyContactPendingRevokePair(ctx, p, excludeUID, marker.MembershipUID, marker.Username); reassertErr != nil {
				return fmt.Errorf("drain key_contact pending revoke marker for %s: %w", excludeUID, reassertErr)
			}
			if idx != nil && !pairDurablyOwned(ctx, idx, justifiedBy, marker.MembershipUID, marker.Username) {
				return fmt.Errorf("transfer durable revoke address for key_contact %s pending marker", excludeUID)
			}
		}
	}
	return nil
}

// revokeKeyContactGrantIfNoLongerLive revokes a key contact's recorded grant
// and clears the index entry once the revoke is confirmed delivered. reason
// is logged only.
//
// It owns the sibling-justification decision for index-driven revokes: a live
// sibling that still justifies the stored pair clears only this record's entry
// (so a later delete cannot revoke the sibling's access), and an uncertain
// scan leaves the entry exactly as read, publishing nothing. liveUsername and
// liveEmail are the contact's current identity: only when liveUsername is
// known and positively matches the stored username may liveEmail justify the
// pair by direct match; otherwise (including an unresolved liveUsername) the
// pair is checked by resolution alone.
//
// A contact with no recorded grant, or one whose recorded pair is already
// empty, produces no publish — there is nothing to revoke.
//
// Before publishing, this claims the entry with a revision-conditional
// rewrite of the exact pair it just read, then re-reads once more to pin
// down the exact revision that claim committed at. That revision — not
// stored.Revision — is the baseline the final re-read below is compared
// against: the bucket's revision is a sequence shared by every key in it, so
// a write to any *other* key between the claim and that final re-read can
// advance this key's next revision by more than one, and stored.Revision+1
// would then never match even with no concurrent writer touching this key at
// all.
//
// Between that baseline and the final re-read after publish+flush, nothing
// serializes this call against a concurrent writer's own CAS — a writer that
// reasserts the same pair (recordKeyContactGrant's unchanged-pair branch)
// always advances the revision on its own confirmation, so a revision
// mismatch there means exactly that raced this call's member_remove. If the
// pair is still the same one this call just tried to revoke, the remove may
// have undone a grant just reconfirmed live, so this repairs it with a
// compensating member_put rather than only skipping the index clear.
//
// It returns an error whenever an authorization removal may still be needed
// but is not confirmed delivered, or a durable retry address may have been
// lost: an index read failure, an uncertain sibling scan, a hard claim
// failure, or a failed publish/flush. A claim CAS conflict, or the entry
// being overtaken right after the claim, returns nil, another writer already
// owns the entry so there is nothing left for this call to do. A failure
// clearing the index after a confirmed revoke also stays nil (log-only): the
// revoke itself succeeded, only stale bookkeeping remains.
//
// recheck must be a LIVE lister, or nil where none can be constructed: after
// the remove publishes, it is used to re-run justification and catch a
// different-UID regrant racing the (possibly snapshot) lister scan, mirroring
// revokeKeyContactPairIfUnjustified's own post-remove recheck.
func revokeKeyContactGrantIfNoLongerLive(ctx context.Context, p port.MemberPublisher, idx port.KeyContactGrantIndex, lister, recheck membershipKeyContactLister, uid, liveUsername, liveEmail, reason string) error {
	if idx == nil || uid == "" {
		return nil
	}
	stored, found, err := idx.Get(ctx, uid)
	if err != nil {
		slog.WarnContext(ctx, "key_contact grant index read failed — cannot check for a stale grant to revoke",
			"uid", uid, "reason", reason, "error", err)
		return fmt.Errorf("read key_contact grant index for %s: %w", uid, err)
	}
	if !found || stored.MembershipUID == "" || stored.Username == "" {
		return nil
	}

	// Justify before the claim: an uncertain scan must leave the entry
	// exactly as read, and a claim would already advance its revision.
	req := keyContactPairRevoke{
		membershipUID: stored.MembershipUID,
		username:      stored.Username,
		excludeUID:    uid,
		reason:        reason,
		flush:         true,
	}
	if liveUsername != "" && stored.Username == liveUsername {
		req.email = liveEmail
	}
	justifiedBy, justifyErr := keyContactPairJustified(ctx, lister, req)
	if justifyErr != nil {
		slog.WarnContext(ctx, "key_contact sibling scan failed: skipping revoke to avoid stripping a possibly still-justified tuple",
			"uid", uid, "membership_uid", stored.MembershipUID, "reason", reason, "error", justifyErr)
		return fmt.Errorf("sibling scan for key_contact %s: %w", uid, justifyErr)
	}
	if justifiedBy != nil {
		// A live sibling keeps the tuple, but this entry may be the pair's
		// only durable address: clearing it before the sibling durably owns
		// the pair would leave a live tuple with no address at all.
		if !pairDurablyOwned(ctx, idx, justifiedBy, stored.MembershipUID, stored.Username) {
			return fmt.Errorf("transfer durable revoke address for key_contact %s", uid)
		}
		clearRevokedGrant(ctx, idx, uid, stored)
		return nil
	}

	if claimErr := idx.Put(ctx, uid, stored); claimErr != nil {
		if pkgerrors.IsConflict(claimErr) {
			// Another writer already touched or replaced the entry: it now
			// owns whatever revoke is needed, not this call.
			slog.WarnContext(ctx, "key_contact grant changed concurrently — skipping revoke to avoid denying a possibly-reasserted grant",
				"uid", uid, "membership_uid", stored.MembershipUID)
			return nil
		}
		slog.WarnContext(ctx, "key_contact grant index claim failed — cannot safely revoke",
			"uid", uid, "membership_uid", stored.MembershipUID, "error", claimErr)
		return fmt.Errorf("claim key_contact grant index for %s: %w", uid, claimErr)
	}

	claimed, found, err := idx.Get(ctx, uid)
	if err != nil || !found {
		slog.WarnContext(ctx, "key_contact grant index read failed after claim — cannot safely revoke",
			"uid", uid, "membership_uid", stored.MembershipUID, "error", err)
		return fmt.Errorf("read key_contact grant index for %s after claim: %w", uid, err)
	}
	if claimed.MembershipUID != stored.MembershipUID || claimed.Username != stored.Username {
		// Overtaken between the claim committing and this read: treat like a
		// claim conflict — something else already owns this entry.
		slog.WarnContext(ctx, "key_contact grant changed concurrently right after claim — skipping revoke",
			"uid", uid, "membership_uid", stored.MembershipUID)
		return nil
	}

	// On a failed or indeterminate publish the claimed entry is retained as
	// the retry address.
	if pubErr := publishKeyContactRemove(ctx, p, req); pubErr != nil {
		return fmt.Errorf("revoke key_contact grant for %s: %w", uid, pubErr)
	}

	if recheck != nil {
		racedBy, recheckErr := keyContactPairJustified(ctx, recheck, req)
		if recheckErr != nil {
			slog.ErrorContext(ctx, "key_contact post-revoke recheck failed: tuple may be incorrectly absent until the next backfill",
				"uid", uid, "membership_uid", stored.MembershipUID, "reason", reason,
				"error", recheckErr, "fga_remove_raced_possible_lost_grant", true)
			return fmt.Errorf("post-revoke recheck for key_contact %s: %w", uid, recheckErr)
		}
		if racedBy != nil {
			repairMsg := BuildKeyContactFGAPutMessage(stored.MembershipUID, stored.Username)
			repairErr := p.Access(ctx, fgaconstants.GenericMemberPutSubject, repairMsg)
			if repairErr == nil {
				repairErr = p.Flush(ctx)
			}
			if repairErr != nil {
				slog.ErrorContext(ctx, "key_contact post-revoke repair failed: tuple may be incorrectly absent until the next backfill",
					"uid", uid, "membership_uid", stored.MembershipUID, "reason", reason,
					"error", repairErr, "fga_remove_raced_possible_lost_grant", true)
				return fmt.Errorf("post-revoke repair for key_contact %s: %w", uid, repairErr)
			}
			slog.WarnContext(ctx, "key_contact grant repaired: a concurrent grant raced this revoke and was reapplied",
				"uid", uid, "membership_uid", stored.MembershipUID, "reason", reason)
		}
	}

	current, found, err := idx.Get(ctx, uid)
	if err != nil || !found {
		// The revoke itself was already confirmed delivered above; only the
		// index bookkeeping is unverifiable here.
		slog.WarnContext(ctx, "key_contact grant index read failed after confirmed revoke: index bookkeeping unverifiable",
			"uid", uid, "membership_uid", stored.MembershipUID, "error", err)
		return nil
	}
	if current.Revision != claimed.Revision {
		if current.MembershipUID == stored.MembershipUID && current.Username == stored.Username {
			repairMsg := BuildKeyContactFGAPutMessage(stored.MembershipUID, stored.Username)
			repairErr := p.Access(ctx, fgaconstants.GenericMemberPutSubject, repairMsg)
			if repairErr == nil {
				repairErr = p.Flush(ctx)
			}
			if repairErr != nil {
				slog.ErrorContext(ctx, "key_contact grant repair failed after a concurrent same-pair regrant raced this revoke: tuple may be incorrectly absent",
					"uid", uid, "membership_uid", stored.MembershipUID,
					"error", repairErr, "fga_revoke_failed_dangling_tuple", true)
				return fmt.Errorf("repair key_contact grant for %s: %w", uid, repairErr)
			}
			slog.WarnContext(ctx, "key_contact grant repaired — a concurrent regrant for the same pair raced this revoke's publish",
				"uid", uid, "membership_uid", stored.MembershipUID)
			return nil
		}
		// A different pair now occupies this entry: something else
		// superseded it entirely, and that writer's own supersede-revoke
		// already addresses our pair — nothing to repair or clear here.
		return nil
	}
	clearRevokedGrant(ctx, idx, uid, current)
	return nil
}

// pairDurablyOwned reports whether the {membershipUID, username} pair has a
// durable address in the index other than the entry being cleared: either
// sib already owns an entry for that exact pair, it owns no entry at all and
// one is created for it here, or its entry names a different pair and is
// stale (a reparent or rename whose index update was lost) and is reconciled
// to the pair it currently justifies, preserving its prior pair as a
// PendingRevoke marker so that pair's own revoke address is not dropped.
// Only a Get/Put failure, or a sibling entry that already carries an
// unrelated PendingRevoke marker (a slot cannot hold two), returns false: the
// caller must not clear its own entry, since it would then be the only
// address left for a pair that still has a live tuple.
func pairDurablyOwned(ctx context.Context, idx port.KeyContactGrantIndex, sib *model.KeyContact, membershipUID, username string) bool {
	stored, found, err := idx.Get(ctx, sib.UID)
	if err != nil {
		slog.WarnContext(ctx, "key_contact grant index read failed for justifying sibling: retaining this entry as the pair's only durable address",
			"sibling_uid", sib.UID, "membership_uid", membershipUID, "error", err)
		return false
	}
	if found {
		if stored.MembershipUID == membershipUID && stored.Username == username {
			return true
		}
		if stored.PendingRevoke != nil {
			// A marker already occupies the entry; a second cannot be carried
			// in the same slot. Rare double-stale case, stays a stall.
			slog.WarnContext(ctx, "key_contact grant index justifying sibling already owns a different entry with a pending marker: retaining this entry as the pair's only durable address",
				"sibling_uid", sib.UID, "membership_uid", membershipUID)
			return false
		}
		// The sibling's own entry is stale: reconcile it to the pair it
		// currently justifies, carrying its prior pair forward as
		// PendingRevoke so an existing live tuple survives as a durable
		// address rather than being dropped; the marker-drain paths clear it.
		reconciled := port.KeyContactGrant{
			MembershipUID: membershipUID,
			Username:      username,
			Revision:      stored.Revision,
			PendingRevoke: &port.KeyContactGrantRef{MembershipUID: stored.MembershipUID, Username: stored.Username},
		}
		if putErr := idx.Put(ctx, sib.UID, reconciled); putErr != nil {
			slog.WarnContext(ctx, "key_contact grant index reconcile failed for justifying sibling: retaining this entry as the pair's only durable address",
				"sibling_uid", sib.UID, "membership_uid", membershipUID, "error", putErr)
			return false
		}
		slog.WarnContext(ctx, "key_contact grant index reconciled stale justifying sibling entry to its current pair",
			"sibling_uid", sib.UID, "membership_uid", membershipUID)
		return true
	}
	if putErr := idx.Put(ctx, sib.UID, port.KeyContactGrant{MembershipUID: membershipUID, Username: username}); putErr != nil {
		slog.WarnContext(ctx, "key_contact grant index write failed for justifying sibling: retaining this entry as the pair's only durable address",
			"sibling_uid", sib.UID, "membership_uid", membershipUID, "error", putErr)
		return false
	}
	return true
}

// clearRevokedGrant removes the just-revoked, confirmed-delivered grant from
// the index. If revoked (the entry as read before the revoke was published)
// still carried a PendingRevoke marker for an unrelated, still-outstanding
// supersede, that marker's address is preserved rather than discarded: the
// entry is rewritten with its live grant cleared but the marker intact,
// instead of being deleted outright — the marker's own confirmed revoke
// (revokeSupersededKeyContactGrant / clearPendingRevoke) is what eventually
// clears it.
//
// The write is conditional on revoked.Revision — the revision observed
// before the revoke was published and flushed — with no retry on conflict.
// A retry that re-reads and compares by value (MembershipUID/Username) alone
// cannot tell "still my original entry" apart from a different generation
// that happens to carry the same pair: a writer can republish the identical
// {membership_uid, username} for this uid between this call's confirmed
// revoke and its clear (e.g. the contact becomes reachable again and a fresh
// member_put lands), which reasserts the live tuple. Retrying against that
// reread value would delete the index's only address for a tuple another
// writer just made live again, leaving a future delete unable to revoke it.
// A conflict here always means some other writer touched this key after our
// revoke was confirmed delivered, so it is left for that writer's own
// generation to manage.
func clearRevokedGrant(ctx context.Context, idx port.KeyContactGrantIndex, uid string, revoked port.KeyContactGrant) {
	var putErr error
	if revoked.PendingRevoke != nil {
		cleared := revoked
		cleared.MembershipUID = ""
		cleared.Username = ""
		putErr = idx.Put(ctx, uid, cleared)
	} else {
		putErr = idx.Delete(ctx, uid, revoked.Revision)
	}
	if putErr == nil {
		return
	}
	if pkgerrors.IsConflict(putErr) {
		slog.WarnContext(ctx, "key_contact grant index clear skipped — entry changed since confirmed revoke",
			"uid", uid, "membership_uid", revoked.MembershipUID)
		return
	}
	slog.WarnContext(ctx, "key_contact grant index clear failed after confirmed revoke — confirmed-delivered grant left in index",
		"uid", uid, "membership_uid", revoked.MembershipUID, "error", putErr)
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

		var putErr error
		if current.MembershipUID == "" && current.Username == "" {
			// Marker-only entry (clearRevokedGrant left it after revoking
			// its own live pair — see the marker-only Put there): clearing
			// this, its only marker, leaves neither a live pair nor a
			// PendingRevoke, which Put rejects as addressing nothing.
			// Delete the entry outright instead.
			putErr = idx.Delete(ctx, uid, current.Revision)
		} else {
			current.PendingRevoke = nil
			putErr = idx.Put(ctx, uid, current)
		}
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
