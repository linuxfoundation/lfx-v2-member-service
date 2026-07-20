// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package project provides the ProjectResolver infrastructure implementation,
// which translates between v2 project UIDs, project slugs, and Salesforce
// Project__c.Id SFIDs. Resolution results are cached in the membership-cache
// NATS KV bucket to avoid repeated NATS RPC and SOQL round-trips.
package project

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/nats"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/salesforce"
	errs "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

type slugLookupRPC interface {
	GetSlug(ctx context.Context, projectUID string) (string, error)
	SlugToUID(ctx context.Context, slug string) (string, error)
}

type projectSlugRepo interface {
	FetchSFIDBySlug(ctx context.Context, slug string) (string, error)
}

type projectEnricher interface {
	fetchProjectByID(ctx context.Context, sfid string) (*projectMeta, error)
	fetchProjectsByIDs(ctx context.Context, sfids []string) (map[string]projectMeta, error)
}

type projectMeta struct {
	name string
	slug *string
}

type projectMappingCache interface {
	GetProjectSFID(ctx context.Context, projectUID string) (nats.CacheResult[string], error)
	PutProjectSFID(ctx context.Context, projectUID, sfid string) error
	GetProjectUID(ctx context.Context, slug string) (nats.CacheResult[string], error)
	PutProjectUID(ctx context.Context, slug, uid string) error
}

// Resolver implements port.ProjectResolver by chaining NATS RPC calls to the
// project-service with SOQL queries to Salesforce B2B, backed by a NATS KV
// cache for both directions of the mapping.
type Resolver struct {
	rpc      slugLookupRPC
	slugRepo projectSlugRepo
	enricher projectEnricher
	cache    projectMappingCache
}

type sfProjectEnricher struct {
	repo *salesforce.ProjectRepo
}

func (a sfProjectEnricher) fetchProjectByID(ctx context.Context, sfid string) (*projectMeta, error) {
	proj, err := a.repo.FetchProjectByID(ctx, sfid)
	if err != nil || proj == nil {
		return nil, err
	}
	return &projectMeta{name: proj.Name, slug: proj.Slug}, nil
}

func (a sfProjectEnricher) fetchProjectsByIDs(ctx context.Context, sfids []string) (map[string]projectMeta, error) {
	projects, err := a.repo.FetchProjectsByIDs(ctx, sfids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]projectMeta, len(projects))
	for _, p := range projects {
		if p != nil {
			out[p.ID] = projectMeta{name: p.Name, slug: p.Slug}
		}
	}
	return out, nil
}

// Ensure Resolver satisfies the port at compile time.
var _ port.ProjectResolver = (*Resolver)(nil)

// NewProjectResolver creates a new Resolver. All arguments are required.
func NewProjectResolver(
	rpc *nats.ProjectRPC,
	repo *salesforce.ProjectRepo,
	cache *nats.Storage,
) *Resolver {
	return &Resolver{
		rpc:      rpc,
		slugRepo: repo,
		enricher: sfProjectEnricher{repo: repo},
		cache:    cache,
	}
}

// SFIDFromUID resolves a v2 project UID to a Salesforce Project__c.Id.
//
// Resolution chain:
//  1. Check KV cache (project-sfid/{uid}).
//  2. Call project-service NATS RPC to get the slug (lfx.projects-api.get_slug).
//  3. Cache the slug for the UID in KV (project-uid/{slug} → uid).
//  4. Query Salesforce SOQL for the Project__c.Id by Slug__c.
//  5. Cache the SFID in KV (project-sfid/{uid} → sfid) and return it.
//
// Returns a NotFound error if the project does not exist in the project-service
// or in Salesforce, and an internal error if Salesforce returns ambiguous results.
func (r *Resolver) SFIDFromUID(ctx context.Context, projectUID string) (string, error) {
	// Step 1: check KV cache.
	result, err := r.cache.GetProjectSFID(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "failed to read project SFID from cache; proceeding with live lookup",
			"project_uid", projectUID,
			"error", err,
		)
	} else if result.Status == nats.CacheStatusFresh || result.Status == nats.CacheStatusStale {
		if result.Status == nats.CacheStatusStale {
			slog.DebugContext(ctx, "serving stale project SFID from cache",
				"project_uid", projectUID,
			)
		}
		return result.Value, nil
	}

	// Step 2: resolve UID → slug via project-service NATS RPC.
	slug, err := r.rpc.GetSlug(ctx, projectUID)
	if err != nil {
		return "", fmt.Errorf("resolving slug for project UID %s: %w", projectUID, err)
	}
	if slug == "" {
		return "", errs.NewNotFound("project not found", fmt.Errorf("uid: %s", projectUID))
	}

	// Step 3: also cache the slug → UID mapping as a side effect. The cache key
	// is normalized to match UIDFromSlug's lookup key; the unnormalized slug is
	// still used below for the Salesforce Slug__c query.
	if putErr := r.cache.PutProjectUID(ctx, normalizeProjectSlug(slug), projectUID); putErr != nil {
		slog.WarnContext(ctx, "failed to cache project UID by slug",
			"slug", slug,
			"project_uid", projectUID,
			"error", putErr,
		)
	}

	// Step 4: query Salesforce for the Project__c.Id by Slug__c.
	sfid, err := r.slugRepo.FetchSFIDBySlug(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("resolving Salesforce SFID for project slug %q: %w", slug, err)
	}
	if sfid == "" {
		return "", errs.NewNotFound(
			"project exists in v2 but not found in Salesforce",
			fmt.Errorf("uid: %s, slug: %s", projectUID, slug),
		)
	}

	// Step 5: cache the UID → SFID mapping and return.
	if putErr := r.cache.PutProjectSFID(ctx, projectUID, sfid); putErr != nil {
		slog.WarnContext(ctx, "failed to cache project SFID",
			"project_uid", projectUID,
			"sfid", sfid,
			"error", putErr,
		)
	}

	return sfid, nil
}

// normalizeProjectSlug lowercases a slug for v2 project-service lookup. V2 API
// slugs are lowercase-only; Salesforce Project__c.Slug__c may use mixed case
// (e.g. ToIP vs toip).
func normalizeProjectSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// UIDFromSlug resolves a project slug to a v2 project UID.
//
// Resolution chain:
//  1. Check KV cache (project-uid/{slug}).
//  2. Call project-service NATS RPC to get the UID (lfx.projects-api.slug_to_uid).
//  3. Cache the slug → UID mapping in KV and return.
//
// Returns a NotFound error if the slug is not known to the project-service.
func (r *Resolver) UIDFromSlug(ctx context.Context, slug string) (string, error) {
	slug = normalizeProjectSlug(slug)
	if slug == "" {
		return "", errs.NewNotFound("project not found", fmt.Errorf("empty slug"))
	}

	// Step 1: check KV cache.
	result, err := r.cache.GetProjectUID(ctx, slug)
	if err != nil {
		slog.WarnContext(ctx, "failed to read project UID from cache; proceeding with live lookup",
			"slug", slug,
			"error", err,
		)
	} else if result.Status == nats.CacheStatusFresh || result.Status == nats.CacheStatusStale {
		if result.Status == nats.CacheStatusStale {
			slog.DebugContext(ctx, "serving stale project UID from cache",
				"slug", slug,
			)
		}
		return result.Value, nil
	}

	// Step 2: resolve slug → UID via project-service NATS RPC.
	uid, err := r.rpc.SlugToUID(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("resolving UID for project slug %q: %w", slug, err)
	}
	if uid == "" {
		return "", errs.NewNotFound("project not found", fmt.Errorf("slug: %s", slug))
	}

	// Step 3: cache the slug → UID mapping and return.
	if putErr := r.cache.PutProjectUID(ctx, slug, uid); putErr != nil {
		slog.WarnContext(ctx, "failed to cache project UID by slug",
			"slug", slug,
			"uid", uid,
			"error", putErr,
		)
	}

	return uid, nil
}

// ResolveProject resolves a project identifier (v2 UUID or slug) to a fully-enriched
// model.ProjectInfo{UID, SFID, Name, Slug}. Used when adding a project to a workspace
// to capture a write-time snapshot of project metadata.
//
// Resolution:
//   - UUID input:  SFIDFromUID → FetchProjectByID(sfid) → assemble ProjectInfo.
//   - Slug input:  UIDFromSlug → SFIDFromUID → FetchProjectByID(sfid) → assemble ProjectInfo.
//
// Returns a Validation error (→ HTTP 400) only when the project is definitively not
// found. Infrastructure failures (NATS RPC, Salesforce) propagate as internal errors
// (→ HTTP 500) so callers can distinguish "bad input" from "dependency down".
func (r *Resolver) ResolveProject(ctx context.Context, idOrSlug string) (model.ProjectInfo, error) {
	var projectUID, sfid string
	var err error

	if isUUID(idOrSlug) {
		projectUID = idOrSlug
		sfid, err = r.SFIDFromUID(ctx, idOrSlug)
		if err != nil {
			return model.ProjectInfo{}, fmt.Errorf("resolving project %q: %w", idOrSlug, err)
		}
	} else {
		// Treat as slug.
		projectUID, err = r.UIDFromSlug(ctx, idOrSlug)
		if err != nil {
			if errs.IsNotFound(err) {
				return model.ProjectInfo{}, errs.NewValidation(fmt.Sprintf("unknown project slug %q: project not found", idOrSlug))
			}
			return model.ProjectInfo{}, fmt.Errorf("resolving project slug %q: %w", idOrSlug, err)
		}
		sfid, err = r.SFIDFromUID(ctx, projectUID)
		if err != nil {
			return model.ProjectInfo{}, fmt.Errorf("resolving project %q: %w", projectUID, err)
		}
	}

	if sfid == "" {
		return model.ProjectInfo{}, errs.NewValidation(fmt.Sprintf("unknown project %q: not found in Salesforce", idOrSlug))
	}

	meta, err := r.enricher.fetchProjectByID(ctx, sfid)
	if err != nil {
		return model.ProjectInfo{}, fmt.Errorf("enriching project %q: %w", sfid, err)
	}
	if meta == nil {
		return model.ProjectInfo{}, errs.NewValidation(fmt.Sprintf("unknown project %q: not found in Salesforce", idOrSlug))
	}

	info := model.ProjectInfo{
		UID:  projectUID,
		SFID: sfid,
		Name: meta.name,
	}
	if meta.slug != nil {
		info.Slug = *meta.slug
	}
	return info, nil
}

// ResolveProjectsBatch resolves a slice of project identifiers (UIDs or slugs) to
// enriched ProjectInfo values. Only the Salesforce enrichment (step 3) is batched;
// step 1 issues one NATS RPC per item and is O(N).
//
// Algorithm:
//  1. Per-item: resolve id → {uid, sfid} via N cache/RPC calls (SFIDFromUID /
//     UIDFromSlug — one NATS round-trip per input; not batched).
//  2. Collect all sfids from successful per-item resolutions.
//  3. ONE FetchProjectsByIDs SOQL call for name+slug enrichment (batched).
//  4. Merge results back by sfid; return parallel ([]ProjectInfo, []error) slices.
//
// Each index in the returned slices corresponds to the same index in idsOrSlugs.
// A nil error at index i means info[i] is populated; a non-nil error means info[i]
// is the zero value.
func (r *Resolver) ResolveProjectsBatch(ctx context.Context, idsOrSlugs []string) ([]model.ProjectInfo, []error) {
	n := len(idsOrSlugs)
	infos := make([]model.ProjectInfo, n)
	errsOut := make([]error, n)

	// Step 1: per-item uid+sfid resolution.
	type resolved struct {
		uid  string
		sfid string
	}
	items := make([]resolved, n)
	sfids := make([]string, 0, n)

	for i, id := range idsOrSlugs {
		var projectUID, sfid string
		var resolveErr error

		if isUUID(id) {
			projectUID = id
			sfid, resolveErr = r.SFIDFromUID(ctx, id)
		} else {
			projectUID, resolveErr = r.UIDFromSlug(ctx, id)
			if resolveErr == nil && projectUID != "" {
				sfid, resolveErr = r.SFIDFromUID(ctx, projectUID)
			}
		}

		if resolveErr != nil || sfid == "" {
			if resolveErr == nil {
				resolveErr = errs.NewValidation(fmt.Sprintf("unknown project %q: not found in Salesforce", id))
			} else if errs.IsNotFound(resolveErr) {
				resolveErr = errs.NewValidation(fmt.Sprintf("unknown project %q: project not found", id))
			}
			// Non-NotFound errors (infrastructure failures) are preserved without a
			// Validation wrapper so AddProjectsBulk can detect and fail the whole request.
			errsOut[i] = resolveErr
			continue
		}

		items[i] = resolved{uid: projectUID, sfid: sfid}
		sfids = append(sfids, sfid)
	}

	if len(sfids) == 0 {
		return infos, errsOut
	}

	// Step 2: batch SOQL fetch for name+slug.
	byID, batchErr := r.enricher.fetchProjectsByIDs(ctx, sfids)
	if batchErr != nil {
		// Mark all not-yet-failed items as failed.
		for i := range idsOrSlugs {
			if errsOut[i] == nil {
				errsOut[i] = fmt.Errorf("batch enrichment failed: %w", batchErr)
			}
		}
		return infos, errsOut
	}

	// Step 3: assemble per-item results.
	for i := range idsOrSlugs {
		if errsOut[i] != nil {
			continue
		}
		sfid := items[i].sfid
		enrichment, ok := byID[sfid]
		if !ok {
			errsOut[i] = errs.NewValidation(fmt.Sprintf("unknown project %q: not found in Salesforce", idsOrSlugs[i]))
			continue
		}
		info := model.ProjectInfo{
			UID:  items[i].uid,
			SFID: sfid,
			Name: enrichment.name,
		}
		if enrichment.slug != nil {
			info.Slug = *enrichment.slug
		}
		infos[i] = info
	}
	return infos, errsOut
}

// isUUID reports whether s is a valid UUID (any version).
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
