// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
)

// resolveProjectUID resolves the v2 project UID from its slug via the resolver,
// logging a warning on failure. Returns current when it is already set, the slug
// is empty, or the resolver is nil; returns "" when resolution fails. Shared by
// the backfill Runner and the CDC consumer so all publish paths populate
// ProjectUID identically.
func resolveProjectUID(ctx context.Context, resolver port.ProjectResolver, slug, current string) string {
	if current != "" || slug == "" || resolver == nil {
		return current
	}
	uid, err := resolver.UIDFromSlug(ctx, slug)
	if err != nil {
		slog.WarnContext(ctx, "failed to resolve project UID", "slug", slug, "error", err)
		return ""
	}
	return uid
}
