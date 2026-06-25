// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/redaction"
)

// AvatarBackfiller re-enriches b2b_org_settings writer/auditor avatars from auth-service and
// republishes the indexer doc. Idempotent: an org with no avatar change is skipped (no write, no
// republish), so it doubles as the recurring refresh.
type AvatarBackfiller struct {
	settingsReader port.B2BOrgSettingsReader
	settingsWriter port.B2BOrgSettingsWriter
	b2bReader      port.B2BOrgReader
	userReader     port.UserReader
	publisher      port.MemberPublisher
}

// NewAvatarBackfiller constructs an AvatarBackfiller.
func NewAvatarBackfiller(
	settingsReader port.B2BOrgSettingsReader,
	settingsWriter port.B2BOrgSettingsWriter,
	b2bReader port.B2BOrgReader,
	userReader port.UserReader,
	publisher port.MemberPublisher,
) *AvatarBackfiller {
	return &AvatarBackfiller{
		settingsReader: settingsReader,
		settingsWriter: settingsWriter,
		b2bReader:      b2bReader,
		userReader:     userReader,
		publisher:      publisher,
	}
}

// AvatarBackfillOptions configures a backfill run.
type AvatarBackfillOptions struct {
	DryRun bool
	// MissingOnly only enriches principals whose avatar is currently empty.
	MissingOnly bool
	// Sleep waits between each auth-service lookup to respect Auth0 rate limits.
	Sleep time.Duration
}

// Run executes the backfill across every org-settings record, returning an error only when at least
// one org failed to write/enrich (so a non-zero exit surfaces to the operator).
func (r *AvatarBackfiller) Run(ctx context.Context, opts AvatarBackfillOptions) error {
	uids, err := r.settingsReader.ListSettingsOrgUIDs(ctx)
	if err != nil {
		return fmt.Errorf("listing org-settings keys: %w", err)
	}

	log := slog.With("component", "avatar_backfill", "dry_run", opts.DryRun, "missing_only", opts.MissingOnly)
	log.InfoContext(ctx, "avatar backfill started", "org_count", len(uids))

	stats := &avatarBackfillStats{}

	for _, uid := range uids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		settings, revision, getErr := r.settingsReader.GetSettings(ctx, uid)
		if getErr != nil {
			log.WarnContext(ctx, "failed to read settings; skipping", "uid", uid, "error", getErr)
			stats.failed++
			continue
		}
		if settings == nil {
			continue
		}
		stats.orgsScanned++

		// Two statements, not `||`, so the second list is always enriched.
		writersChanged := r.enrich(ctx, uid, settings.Writers, opts, stats)
		auditorsChanged := r.enrich(ctx, uid, settings.Auditors, opts, stats)
		if !writersChanged && !auditorsChanged {
			continue
		}

		if opts.DryRun {
			stats.orgsWritten++
			continue
		}

		if wErr := r.settingsWriter.UpdateSettings(ctx, settings, revision); wErr != nil {
			// A concurrent write is benign (the next run picks it up) — don't fail the Job over it.
			if errs.IsConflict(wErr) {
				log.InfoContext(ctx, "settings changed concurrently; skipping (will retry next run)", "uid", uid)
				continue
			}
			log.WarnContext(ctx, "failed to persist avatar updates", "uid", uid, "error", wErr)
			stats.failed++
			continue
		}

		if r.b2bReader == nil {
			log.WarnContext(ctx, "avatars written but indexer republish skipped (org reader unavailable)", "uid", uid)
		} else if org, orgErr := r.b2bReader.GetB2BOrg(ctx, uid); orgErr == nil {
			PublishB2BOrgSettingsIndexer(ctx, r.publisher, org, settings, indexerConstants.ActionUpdated)
		} else if !errs.IsNotFound(orgErr) {
			log.WarnContext(ctx, "settings avatars written but indexer republish skipped (org fetch failed)", "uid", uid, "error", orgErr)
		}
		stats.orgsWritten++
	}

	log.InfoContext(ctx, "avatar backfill complete",
		"orgs_scanned", stats.orgsScanned,
		"users_scanned", stats.usersScanned,
		"users_updated", stats.usersUpdated,
		"orgs_written", stats.orgsWritten,
		"failed", stats.failed,
	)

	if stats.failed > 0 {
		return fmt.Errorf("avatar backfill completed with %d failure(s)", stats.failed)
	}
	return nil
}

// avatarBackfillStats accumulates run counters across orgs and the per-list enrich pass.
type avatarBackfillStats struct {
	orgsScanned  int
	usersScanned int
	usersUpdated int
	orgsWritten  int
	failed       int
}

// enrich refreshes each accepted principal's avatar in place and reports whether any value changed.
// A miss leaves the existing value untouched; a transport error is counted and isolated.
func (r *AvatarBackfiller) enrich(ctx context.Context, uid string, users []model.B2BOrgUser, opts AvatarBackfillOptions, stats *avatarBackfillStats) bool {
	changed := false
	for i := range users {
		u := &users[i]
		status := u.EffectiveStatus()
		if status == model.InviteStatusRevoked || status == model.InviteStatusExpired {
			continue
		}
		username := strings.TrimSpace(u.Username)
		if username == "" {
			continue
		}
		if opts.MissingOnly && u.Avatar != "" {
			continue
		}
		stats.usersScanned++

		meta, err := r.userReader.UserMetadataByPrincipal(ctx, username)
		if err != nil {
			if !errs.IsNotFound(err) {
				stats.failed++
				slog.WarnContext(ctx, "avatar backfill metadata lookup failed", "uid", uid, "username", redaction.Redact(username), "error", err)
			}
		} else if u.Avatar != meta.Picture {
			// Apply the just-fetched value before the rate-limit sleep so a mid-run cancel doesn't drop it.
			u.Avatar = meta.Picture
			stats.usersUpdated++
			changed = true
		}

		if opts.Sleep > 0 {
			if sleepErr := sleepWithContext(ctx, opts.Sleep); sleepErr != nil {
				return changed
			}
		}
	}
	return changed
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
