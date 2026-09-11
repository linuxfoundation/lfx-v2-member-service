// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// MaxMemberTierCandidates caps how many unique reverse-index UIDs
// HighestActiveTiers resolves per user. Each can be a Salesforce read, so past
// the cap it fails closed rather than fan out or truncate to a wrong top tier.
const MaxMemberTierCandidates = 200

// MemberTiers is the read use-case behind GET /b2b_orgs/member-tiers/{username}:
// it resolves the highest active membership tier per B2B organization for the
// organizations a user is a key contact of.
type MemberTiers struct {
	storage              port.MemberReader
	userMembershipReader port.UserMembershipReader
}

// NewMemberTiers constructs the member-tiers read use-case.
func NewMemberTiers(storage port.MemberReader, userMembershipReader port.UserMembershipReader) *MemberTiers {
	return &MemberTiers{storage: storage, userMembershipReader: userMembershipReader}
}

// HighestActiveTiers lists each B2B organization's highest active membership
// for the organizations the given user is a key contact of, ordered highest
// tier first so the leading entry is the user's top tier. The FGA tuples are a
// reverse index only; each candidate is verified against the authoritative
// membership record, and the user's key-contact standing is revalidated
// against the membership's Project_Role__c records, since a published grant
// can outlive the contact going inactive or being reassigned. Unknown users
// yield an empty list, not a not-found error, so callers cannot probe which
// usernames exist.
//
// Candidates read through the cached GetMembership, not the always-revalidating
// AssembleProjectMembership, to spare Salesforce calls. Eligibility (the
// key-contact edge) is read live from fga-sync each call and never cached
// here; only status, end date, and tier come from the soft-TTL cache. The CDC
// consumer evicts a cached record on each Asset change as a best-effort
// freshness aid (it is absent in mock mode and can lag or drop events), so a
// read may still serve a record up to its soft TTL out of date.
func (u *MemberTiers) HighestActiveTiers(ctx context.Context, username string) ([]*model.ProjectMembership, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, pkgerrors.NewValidation("username must not be blank")
	}

	uids, err := u.userMembershipReader.MembershipUIDsForUser(ctx, username)
	if err != nil {
		return nil, err
	}
	// Deduplicate before applying the cap: only unique memberships fan out to
	// reads, so duplicate tuple rows must not count against the cap.
	unique := make([]string, 0, len(uids))
	seen := make(map[string]bool, len(uids))
	for _, uid := range uids {
		if seen[uid] {
			continue
		}
		seen[uid] = true
		unique = append(unique, uid)
	}
	if len(unique) > MaxMemberTierCandidates {
		slog.WarnContext(ctx, "member-tiers candidate set exceeds cap; refusing to fan out",
			"username", redaction.Redact(username), "candidates", len(unique), "cap", MaxMemberTierCandidates)
		return nil, pkgerrors.NewServiceUnavailable("too many candidate memberships to resolve safely")
	}

	now := time.Now().UTC()
	best := make(map[string]*model.ProjectMembership, len(unique))
	for _, uid := range unique {
		membership, err := u.storage.GetMembership(ctx, uid)
		if err != nil {
			var notFound pkgerrors.NotFound
			if errors.As(err, &notFound) {
				// Dangling reverse-index tuple: FGA references a membership
				// that no longer resolves. Skip it rather than failing the
				// whole lookup.
				slog.WarnContext(ctx, "skipping dangling membership tuple",
					"membership_uid", uid, "username", redaction.Redact(username))
				continue
			}
			// Any other failure would silently omit organizations the user
			// belongs to, so fail the whole lookup closed. The Salesforce-
			// backed reader reports outages as untyped wrapped errors; coerce
			// those to ServiceUnavailable so callers see the documented 503
			// rather than a generic 500.
			var unavailable pkgerrors.ServiceUnavailable
			if !errors.As(err, &unavailable) {
				err = pkgerrors.NewServiceUnavailable("reading membership record", err)
			}
			return nil, err
		}

		if !membershipCountsAsActive(membership, now) {
			continue
		}
		if membership.B2BOrgUID == "" {
			slog.WarnContext(ctx, "skipping membership without b2b_org_uid", "membership_uid", uid)
			continue
		}

		// The FGA tuple only records that a key_contact grant was once
		// published; the grant lifecycle can lag a contact going inactive or
		// being reassigned. Revalidate against the authoritative
		// Project_Role__c records before counting the organization.
		isContact, err := u.userIsActiveKeyContact(ctx, uid, username)
		if err != nil {
			// Same fail-closed reasoning as the membership read above: a
			// silent skip would omit organizations the user belongs to.
			var unavailable pkgerrors.ServiceUnavailable
			if !errors.As(err, &unavailable) {
				err = pkgerrors.NewServiceUnavailable("verifying key-contact status", err)
			}
			return nil, err
		}
		if !isContact {
			slog.InfoContext(ctx, "skipping membership: no active key-contact record for user",
				"membership_uid", uid, "username", redaction.Redact(username))
			continue
		}

		if current, ok := best[membership.B2BOrgUID]; !ok || membershipOutranks(membership, current) {
			best[membership.B2BOrgUID] = membership
		}
	}

	ordered := make([]*model.ProjectMembership, 0, len(best))
	for _, m := range best {
		ordered = append(ordered, m)
	}
	// Highest tier first, so a caller can take the leading entry as the user's
	// top tier across all their organizations without carrying the rank order
	// itself. Equal tiers break by company name, then b2b_org_uid, for a stable
	// and human-legible order.
	sort.Slice(ordered, func(i, j int) bool {
		ri := model.TierClassRank(model.TierClass(ordered[i].TierName))
		rj := model.TierClassRank(model.TierClass(ordered[j].TierName))
		if ri != rj {
			return ri > rj
		}
		if ordered[i].CompanyName != ordered[j].CompanyName {
			return ordered[i].CompanyName < ordered[j].CompanyName
		}
		return ordered[i].B2BOrgUID < ordered[j].B2BOrgUID
	})
	return ordered, nil
}

// userIsActiveKeyContact reports whether the username is listed as an active
// key contact on the membership's authoritative Project_Role__c records. It
// reads through the soft-TTL key-contacts cache, so it costs one cached read
// per candidate and one Salesforce query on a cold membership.
func (u *MemberTiers) userIsActiveKeyContact(ctx context.Context, membershipUID, username string) (bool, error) {
	contacts, err := u.storage.ListKeyContactsForMembership(ctx, membershipUID)
	if err != nil {
		return false, err
	}
	for _, kc := range contacts {
		if kc == nil {
			continue
		}
		// Username match is exact, mirroring the tuple parse in
		// MembershipUIDsForUser: both compare the LFID verbatim.
		if kc.Username == username && strings.EqualFold(kc.Status, constants.RoleStatusActive) {
			return true, nil
		}
	}
	return false, nil
}

// membershipCountsAsActive reports whether a membership counts towards the
// member-tiers lookup: Status is Active and the end date, when parseable, has
// not passed. A membership stays active through its end date.
func membershipCountsAsActive(m *model.ProjectMembership, now time.Time) bool {
	if !strings.EqualFold(m.Status, "Active") {
		return false
	}
	if end, ok := parseMembershipDate(m.EndDate); ok && end.Before(now.Truncate(24*time.Hour)) {
		return false
	}
	return true
}

// membershipOutranks reports whether candidate should replace current as an
// organization's winning membership: a strictly higher normalized tier class,
// or the same class with a later (or open-ended) end date.
func membershipOutranks(candidate, current *model.ProjectMembership) bool {
	candRank := model.TierClassRank(model.TierClass(candidate.TierName))
	curRank := model.TierClassRank(model.TierClass(current.TierName))
	if candRank != curRank {
		return candRank > curRank
	}
	return membershipEndForCompare(candidate).After(membershipEndForCompare(current))
}

// membershipEndCompareMax stands in for an absent or unparseable end date
// during tie-breaking, treating such memberships as open-ended.
var membershipEndCompareMax = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

func membershipEndForCompare(m *model.ProjectMembership) time.Time {
	if end, ok := parseMembershipDate(m.EndDate); ok {
		return end
	}
	return membershipEndCompareMax
}

// parseMembershipDate parses a Salesforce date or datetime string.
func parseMembershipDate(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
