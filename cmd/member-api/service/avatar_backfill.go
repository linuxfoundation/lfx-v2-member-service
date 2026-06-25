// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
)

// RunAvatarBackfill builds the avatar backfiller from env-configured providers and runs it once
// (RUN_MODE=avatar-backfill), then returns. Intended to run as a per-env Kubernetes Job.
//
// Config via env:
//   - AVATAR_BACKFILL_DRY_RUN       (default "true"; set "false" to actually write)
//   - AVATAR_BACKFILL_MISSING_ONLY  (default "false"; only fill empty avatars)
//   - AVATAR_BACKFILL_SLEEP         (Go duration between auth-service lookups, default "0")
func RunAvatarBackfill(ctx context.Context) error {
	opts := usecaseSvc.AvatarBackfillOptions{
		DryRun:      os.Getenv("AVATAR_BACKFILL_DRY_RUN") != "false",
		MissingOnly: os.Getenv("AVATAR_BACKFILL_MISSING_ONLY") == "true",
	}
	if raw := os.Getenv("AVATAR_BACKFILL_SLEEP"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid AVATAR_BACKFILL_SLEEP %q: %w", raw, err)
		}
		opts.Sleep = d
	}

	slog.InfoContext(ctx, "Starting membership service (avatar-backfill mode)",
		"dry_run", opts.DryRun, "missing_only", opts.MissingOnly, "sleep", opts.Sleep.String())

	// The B2BOrg reader is only used for the indexer republish on the write path, and constructing it
	// eagerly authenticates Salesforce (fatal on failure). Skip it for a dry-run so a preview never
	// depends on SF; the backfiller logs and skips republish when the reader is nil.
	var b2bReader port.B2BOrgReader
	if !opts.DryRun {
		b2bReader = B2BOrgReaderImpl(ctx)
	}

	backfiller := usecaseSvc.NewAvatarBackfiller(
		B2BOrgSettingsReaderImpl(ctx),
		B2BOrgSettingsWriterImpl(ctx),
		b2bReader,
		UserReaderImpl(ctx),
		MemberPublisherImpl(ctx),
	)
	return backfiller.Run(ctx, opts)
}
