// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
)

// RunAvatarBackfill runs a one-off b2b_org_settings avatar enrichment (RUN_MODE=avatar-backfill) by
// handing a full-mode, avatar-enriching BackfillRequest to the shared backfill Runner, then returns
// its error so the Job exits non-zero on a systemic failure. Intended as a per-env Kubernetes Job.
//
// Config via env:
//   - AVATAR_BACKFILL_DRY_RUN       (default "true"; set "false" to actually write)
//   - AVATAR_BACKFILL_MISSING_ONLY  (default "false"; only fill empty avatars)
//   - AVATAR_BACKFILL_SLEEP         (Go duration between auth-service lookups, default "0")
//
// REPOSITORY_SOURCE=mock runs it end to end without Salesforce creds (mock readers/writers).
func RunAvatarBackfill(ctx context.Context) error {
	dryRun := os.Getenv("AVATAR_BACKFILL_DRY_RUN") != "false"
	missingOnly := os.Getenv("AVATAR_BACKFILL_MISSING_ONLY") == "true"

	var sleep time.Duration
	if raw := os.Getenv("AVATAR_BACKFILL_SLEEP"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid AVATAR_BACKFILL_SLEEP %q: %w", raw, err)
		}
		sleep = d
	}

	req := usecaseSvc.AvatarBackfillRequest(uuid.New().String(), dryRun, missingOnly, sleep)
	slog.InfoContext(ctx, "Starting membership service (avatar-backfill mode)",
		"run_id", req.RunID, "dry_run", dryRun, "missing_only", missingOnly, "sleep", sleep.String())

	return BackfillRunnerImpl(ctx).Run(ctx, req)
}
