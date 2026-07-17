// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// resolveProjectUID resolves the v2 project UID from its slug via the resolver.
// It returns (uid, true) when the UID is already set, when there is nothing to
// resolve (empty slug or nil resolver), or on a successful lookup. It returns
// ("", false) only when the resolver returns an error — a transient failure.
//
// Publish-path callers (CDC consumer, backfill Runner) must skip publishing a
// record when ok is false rather than publishing the empty UID: a full index
// update with an empty project_uid would overwrite an existing project_uid tag /
// parent_ref (and reconcile away the FGA project tuple), re-creating the
// missing-project state whenever project-service is briefly unavailable. Skipped
// records are repaired by the next CDC event or POST /admin/reindex.
func resolveProjectUID(ctx context.Context, resolver port.ProjectResolver, slug, current string) (string, bool) {
	if current != "" || slug == "" || resolver == nil {
		return current, true
	}
	uid, err := resolver.UIDFromSlug(ctx, slug)
	if err != nil {
		slog.WarnContext(ctx, "failed to resolve project UID", "slug", slug, "error", err)
		return "", false
	}
	return uid, true
}
