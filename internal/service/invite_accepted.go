// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// InviteAcceptedService handles lfx.invite-service.invite_accepted events.
// It filters to b2b_org events, then scans all org settings for pending entries
// matching the accepted invite's email and promotes them to accepted — triggering
// FGA + indexer republish. This mirrors the committee/project scan pattern.
type InviteAcceptedService struct {
	settingsReader    port.B2BOrgSettingsReader
	orgSettingsWriter OrgSettingsUpdater
	keyContactReader  KeyContactOrgReader
	publisher         port.MemberPublisher
	grantIndex        port.KeyContactGrantIndex
}

// InviteAcceptedServiceOption configures an InviteAcceptedService.
type InviteAcceptedServiceOption func(*InviteAcceptedService)

// WithInviteAcceptedSettingsReader sets the settingsReader.
func WithInviteAcceptedSettingsReader(r port.B2BOrgSettingsReader) InviteAcceptedServiceOption {
	return func(s *InviteAcceptedService) { s.settingsReader = r }
}

// WithInviteAcceptedOrgSettingsWriter sets the orgSettingsWriter. Any value that
// satisfies OrgSettingsWriter (the full use-case interface) also satisfies the
// narrower OrgSettingsUpdater — no adapter needed.
func WithInviteAcceptedOrgSettingsWriter(w OrgSettingsWriter) InviteAcceptedServiceOption {
	return func(s *InviteAcceptedService) { s.orgSettingsWriter = w }
}

// WithInviteAcceptedKeyContactReader sets the key-contact org reader.
func WithInviteAcceptedKeyContactReader(r KeyContactOrgReader) InviteAcceptedServiceOption {
	return func(s *InviteAcceptedService) { s.keyContactReader = r }
}

// WithInviteAcceptedPublisher sets the publisher for FGA grants.
func WithInviteAcceptedPublisher(p port.MemberPublisher) InviteAcceptedServiceOption {
	return func(s *InviteAcceptedService) { s.publisher = p }
}

// WithInviteAcceptedKeyContactGrantIndex sets the durable record of published
// key_contact FGA grants, so a grant made on invite acceptance is revocable when
// the contact is later deleted.
func WithInviteAcceptedKeyContactGrantIndex(i port.KeyContactGrantIndex) InviteAcceptedServiceOption {
	return func(s *InviteAcceptedService) { s.grantIndex = i }
}

// NewInviteAcceptedService creates a new InviteAcceptedService.
func NewInviteAcceptedService(opts ...InviteAcceptedServiceOption) *InviteAcceptedService {
	s := &InviteAcceptedService{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handle processes an InviteServiceAcceptedEvent.
//
// Early exits (no KV access):
//   - ev.Recipient.Email or ev.AcceptedBy is empty — malformed, drop + warn.
//   - ev.Resource.Type != "b2b_org" — belongs to committee/project, not us; drop silently.
//
// Otherwise, scans all org settings for pending entries whose email matches
// ev.Recipient.Email and promotes them. Errors per org are logged but not returned —
// the handler is best-effort because the upstream NATS subscription uses core
// QueueSubscribe with no ACK/NAK.
func (s *InviteAcceptedService) Handle(ctx context.Context, ev inviteapi.InviteServiceAcceptedEvent) error {
	if s.settingsReader == nil || s.orgSettingsWriter == nil {
		slog.ErrorContext(ctx, "InviteAcceptedService not fully initialized",
			"settingsReader", s.settingsReader != nil,
			"orgSettingsWriter", s.orgSettingsWriter != nil,
		)
		return nil
	}

	// Validate required fields first so we never scan on a malformed event.
	if strings.TrimSpace(ev.Recipient.Email) == "" || ev.AcceptedBy == "" {
		slog.WarnContext(ctx, "invite_accepted event missing required fields — dropping",
			"accepted_by", redaction.Redact(ev.AcceptedBy),
			"recipient_email", redaction.RedactEmail(ev.Recipient.Email),
		)
		return nil
	}

	// Filter to org events only. Member-service receives every invite_accepted event
	// (no resource-type filter on the subscription), so committee and project
	// acceptances arrive here too. Drop them with zero KV access.
	if ev.Resource.Type != "b2b_org" {
		slog.DebugContext(ctx, "invite_accepted: skipping non-org resource type",
			"resource_type", ev.Resource.Type,
		)
		return nil
	}

	orgUIDs, err := s.settingsReader.ListSettingsOrgUIDs(ctx)
	if err != nil {
		slog.WarnContext(ctx, "invite_accepted: failed to list org settings UIDs", "error", err)
		return nil
	}

	normalizedEmail := normalizeSettingsEmail(ev.Recipient.Email)

	// fgaOrgs collects all org UIDs for which key_contact FGA + indexer events
	// must be published. Seeded with ev.Resource.UID (the org the invite was sent
	// for) so contacts there are resolved even when no pending org-settings entry
	// exists (e.g. contact added via CDC after the invite was created).
	// promoteInviteInOrg adds any org where a pending entry was found and promoted
	// but that org differs from the invite's source (cross-org acceptance fix).
	fgaOrgs := make(map[string]struct{})
	if ev.Resource.UID != "" {
		fgaOrgs[ev.Resource.UID] = struct{}{}
	}
	for _, orgUID := range orgUIDs {
		if s.promoteInviteInOrg(ctx, orgUID, normalizedEmail, ev) {
			fgaOrgs[orgUID] = struct{}{}
		}
	}

	if s.keyContactReader != nil && s.publisher != nil {
		for orgUID := range fgaOrgs {
			s.resolveKeyContactsInOrg(ctx, orgUID, normalizedEmail, ev.AcceptedBy)
		}
	}
	return nil
}

// resolveKeyContactsInOrg grants key_contact FGA on every membership where a
// key contact's email matches the accepted invite. Resolves all matches — does
// not stop at the first one (an org can have the same person as a key contact
// on multiple memberships).
func (s *InviteAcceptedService) resolveKeyContactsInOrg(ctx context.Context, orgUID, normalizedEmail, acceptedBy string) {
	contacts, err := s.keyContactReader.ListKeyContactsForOrg(ctx, orgUID)
	if err != nil {
		slog.WarnContext(ctx, "invite_accepted: list key contacts for org failed",
			"org_uid", orgUID, "error", err)
		return
	}
	for _, kc := range contacts {
		if normalizeSettingsEmail(kc.Email) != normalizedEmail {
			continue
		}
		kc.Username = strings.TrimPrefix(acceptedBy, legacyAuth0UsernamePrefix)
		PublishKeyContactFGA(ctx, s.publisher, s.grantIndex, kc)
		PublishKeyContactIndexer(ctx, s.publisher, kc, indexerConstants.ActionUpdated)
	}
}

// promoteInviteInOrg attempts to find and promote all pending entries matching
// normalizedEmail in one org. Uses a list-authoritative + role tie-break strategy:
//   - Entries in exactly one list → promote all of them.
//   - Entries in both lists → ev.Role selects: Manage→writers, View→auditors.
//     Unknown/empty role → skip + warn (no over-grant).
//
// Returns true only when a pending entry was found AND the promotion write
// succeeded. Returns false on write failure or after exhausting CAS retries,
// so the caller only adds the org to the FGA/indexer resolution set when
// the promotion actually committed.
//
// Uses an optimistic-CAS retry loop (up to 3 attempts) to handle concurrent writes.
// Errors are logged but not returned.
func (s *InviteAcceptedService) promoteInviteInOrg(ctx context.Context, orgUID, normalizedEmail string, ev inviteapi.InviteServiceAcceptedEvent) bool {
	const maxRetries = 3

	for retry := range maxRetries {
		settings, _, err := s.settingsReader.GetSettings(ctx, orgUID)
		if err != nil {
			slog.WarnContext(ctx, "invite_accepted: failed to get org settings",
				"org_uid", orgUID, "error", err)
			return false
		}
		if settings == nil {
			return false
		}

		// Snapshot the ETag now so Update's IfMatch check catches any concurrent
		// write that changes the lists between this read and Update's own read.
		// On mismatch Update returns PreconditionFailed, which the retry loop handles.
		ifMatch, etagErr := etag.LFXEtag(settings)
		if etagErr != nil {
			slog.WarnContext(ctx, "invite_accepted: failed to compute settings ETag, skipping org",
				"org_uid", orgUID, "error", etagErr)
			return false
		}

		writerIdxs := pendingEmailIndices(settings.Writers, normalizedEmail)
		auditorIdxs := pendingEmailIndices(settings.Auditors, normalizedEmail)

		if len(writerIdxs) == 0 && len(auditorIdxs) == 0 {
			return false // no match in this org
		}

		// Determine which list(s) to promote.
		var promoteWriters, promoteAuditors bool
		switch {
		case len(writerIdxs) > 0 && len(auditorIdxs) == 0:
			promoteWriters = true
		case len(writerIdxs) == 0 && len(auditorIdxs) > 0:
			promoteAuditors = true
		default:
			// Email appears in both lists — use ev.Role as tie-breaker.
			switch ev.Role {
			case string(inviteapi.InviteRoleManage):
				promoteWriters = true
			case string(inviteapi.InviteRoleView):
				promoteAuditors = true
			default:
				slog.WarnContext(ctx, "invite_accepted: email in both writers and auditors with unresolvable role — skipping org",
					"org_uid", orgUID,
					"email", redaction.RedactEmail(ev.Recipient.Email),
					"role", ev.Role,
				)
				return true
			}
		}

		now := time.Now().UTC()
		update := B2BOrgSettingsUpdate{OrgUID: orgUID, IfMatch: ifMatch}

		if promoteWriters {
			writers := slices.Clone(settings.Writers)
			for _, i := range writerIdxs {
				writers[i].Username = strings.TrimPrefix(ev.AcceptedBy, legacyAuth0UsernamePrefix)
				writers[i].InviteStatus = model.InviteStatusAccepted
				writers[i].AcceptedAt = &now
				writers[i].InviteUUID = ""
				writers[i].UpdatedAt = now
			}
			update.Writers = writers
		}
		if promoteAuditors {
			auditors := slices.Clone(settings.Auditors)
			for _, i := range auditorIdxs {
				auditors[i].Username = strings.TrimPrefix(ev.AcceptedBy, legacyAuth0UsernamePrefix)
				auditors[i].InviteStatus = model.InviteStatusAccepted
				auditors[i].AcceptedAt = &now
				auditors[i].InviteUUID = ""
				auditors[i].UpdatedAt = now
			}
			update.Auditors = auditors
		}

		_, err = s.orgSettingsWriter.Update(ctx, update)
		if err == nil {
			return true
		}
		if pkgerrors.IsConflict(err) || pkgerrors.IsPreconditionFailed(err) {
			if retry < maxRetries-1 {
				slog.DebugContext(ctx, "invite_accepted: revision conflict, retrying",
					"org_uid", orgUID, "attempt", retry+1)
				continue
			}
			slog.WarnContext(ctx, "invite_accepted: revision conflict after 3 retries",
				"org_uid", orgUID)
			return false
		}
		slog.WarnContext(ctx, "invite_accepted: failed to update org settings",
			"org_uid", orgUID, "error", err)
		return false
	}
	return false
}

// pendingEmailIndices returns the indices of pending entries (no username set)
// in users whose normalised email matches normalizedEmail.
func pendingEmailIndices(users []model.B2BOrgUser, normalizedEmail string) []int {
	var out []int
	for i, u := range users {
		if u.EffectiveStatus() == model.InviteStatusPending &&
			u.Username == "" &&
			normalizeSettingsEmail(u.Email) == normalizedEmail {
			out = append(out, i)
		}
	}
	return out
}
