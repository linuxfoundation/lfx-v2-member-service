// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"fmt"
	"time"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/sfuuid"
)

// BackfillRequest carries the validated, normalised parameters for a single run.
//
// Every request has exactly one Type (BREAKING: there is no all-types shortcut).
// Targeted requests carry Items (UIDs of that one Type). CDCRepair drains the
// quota-repair queue for that Type.
type BackfillRequest struct {
	RunID     string
	Type      string     // required: the single entity type for this run
	Since     *time.Time // nil = full reindex
	Until     *time.Time // optional inclusive upper bound; requires Since
	Items     []string   // targeted UIDs, all of Type
	CDCRepair bool       // drain the CDC quota-repair queue for Type
	DryRun    bool

	// EnrichAvatars re-enriches b2b_org_settings writer/auditor avatars from the auth-service before
	// republishing. Set by the avatar-backfill Job; not exposed on the HTTP /admin/reindex payload.
	EnrichAvatars bool
	// AvatarMissingOnly limits enrichment to principals with an empty avatar.
	AvatarMissingOnly bool
	// AvatarSleep waits between auth-service lookups to respect Auth0 rate limits.
	AvatarSleep time.Duration
}

// AvatarBackfillRequest builds a full-mode b2b_org_settings request with avatar enrichment enabled —
// the request the avatar-backfill Job hands to Runner.Run. It reuses the Runner's control plane
// (run_id, dry-run, full-run lock) rather than a separate backfill path.
func AvatarBackfillRequest(runID string, dryRun, missingOnly bool, sleep time.Duration) BackfillRequest {
	return BackfillRequest{
		RunID:             runID,
		Type:              entityTypeB2BOrgSettings,
		DryRun:            dryRun,
		EnrichAvatars:     true,
		AvatarMissingOnly: missingOnly,
		AvatarSleep:       sleep,
	}
}

// ValidateAndBuildRequest validates the payload and returns a BackfillRequest.
func ValidateAndBuildRequest(p *membershipservice.AdminReindexPayload) (BackfillRequest, error) {
	// Validate the single required top-level type.
	if p.Type == "membership_tier" {
		return BackfillRequest{}, pkgerrors.NewValidation(
			"membership_tier is not currently supported")
	}
	if !validBackfillTypes[p.Type] {
		return BackfillRequest{}, pkgerrors.NewValidation(
			fmt.Sprintf("unknown type %q; supported types: b2b_org, project_membership, key_contact, b2b_org_settings", p.Type))
	}

	// cdc_repair drains the queue for one CDC-backed type; b2b_org_settings has
	// no CDC skip path, and since/items make no sense for a queue drain.
	if p.CdcRepair {
		if p.Type == entityTypeB2BOrgSettings {
			return BackfillRequest{}, pkgerrors.NewValidation(
				"cdc_repair supports only b2b_org, project_membership, key_contact")
		}
		if p.Since != nil || p.Until != nil || len(p.Items) > 0 {
			return BackfillRequest{}, pkgerrors.NewValidation(
				"cdc_repair is mutually exclusive with since, until, and items")
		}
		if p.DryRun {
			// reindexItem returns outcomeIssued for a dry-run without publishing,
			// and RunRepair conditionally deletes the marker on outcomeIssued —
			// so dry_run+cdc_repair would delete real pending markers with no
			// republish. Reject rather than silently drop repair state.
			return BackfillRequest{}, pkgerrors.NewValidation(
				"cdc_repair does not support dry_run")
		}
	}

	// Mutual exclusivity: items vs since/until.
	if len(p.Items) > 0 && (p.Since != nil || p.Until != nil) {
		return BackfillRequest{}, pkgerrors.NewValidation("items mode is mutually exclusive with since and until")
	}

	// until requires since (a bounded window always has a floor); since alone
	// stays valid. This also keeps ClassifyMode unchanged: since != nil routes to
	// filtered, so a until-only request never wrongly takes the full-run lock.
	if p.Until != nil && p.Since == nil {
		return BackfillRequest{}, pkgerrors.NewValidation("until requires since")
	}

	// Validate, normalise, and de-duplicate items (UID-only; the type is the
	// top-level type). Normalize18 both validates and canonicalises to 18-char,
	// matching the UIDs the batch readers return — so countAbsent reconciles a
	// 15-char request against an 18-char result instead of misreporting a published
	// record as not_found. De-duplication (including 15/18-char forms of the same
	// ID, which collapse after normalisation) keeps published + not_found +
	// conversion_error reconciling against len(Items), since Salesforce returns each
	// matching record only once.
	seen := make(map[string]struct{}, len(p.Items))
	items := make([]string, 0, len(p.Items))
	for _, item := range p.Items {
		if item == nil {
			return BackfillRequest{}, pkgerrors.NewValidation("items must not contain a null entry")
		}
		uid, normErr := sfuuid.Normalize18(item.UID)
		if normErr != nil {
			return BackfillRequest{}, pkgerrors.NewValidation(
				fmt.Sprintf("invalid Salesforce ID %q for type %q", item.UID, p.Type))
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		items = append(items, uid)
	}

	// Validate and normalise since.
	var since *time.Time
	if p.Since != nil {
		t, parseErr := time.Parse(time.RFC3339, *p.Since)
		if parseErr != nil {
			return BackfillRequest{}, pkgerrors.NewValidation(
				fmt.Sprintf("since must be a valid RFC 3339 timestamp with an explicit zone offset (e.g. 2026-05-20T00:00:00Z): %v", parseErr))
		}
		utc := t.UTC()
		since = &utc
	}

	// Validate and normalise until (inclusive upper bound).
	var until *time.Time
	if p.Until != nil {
		t, parseErr := time.Parse(time.RFC3339, *p.Until)
		if parseErr != nil {
			return BackfillRequest{}, pkgerrors.NewValidation(
				fmt.Sprintf("until must be a valid RFC 3339 timestamp with an explicit zone offset (e.g. 2026-05-20T00:00:00Z): %v", parseErr))
		}
		utc := t.UTC()
		until = &utc
	}

	// Reject an inverted window so it returns 400 rather than silently matching
	// zero records.
	if since != nil && until != nil && since.After(*until) {
		return BackfillRequest{}, pkgerrors.NewValidation("since must not be after until")
	}

	return BackfillRequest{
		Type:      p.Type,
		Since:     since,
		Until:     until,
		Items:     items,
		CDCRepair: p.CdcRepair,
		DryRun:    p.DryRun,
	}, nil
}
