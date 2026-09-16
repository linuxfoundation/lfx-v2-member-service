// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"slices"

	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"golang.org/x/sync/errgroup"
)

// B2BOrgWriter orchestrates Create/Update for b2b_org records.
type B2BOrgWriter interface {
	Create(ctx context.Context, sfid string) (*model.B2BOrg, error)
	Update(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error)
	// UpdateWithoutPublish persists a transient internal state without exposing
	// it to the indexer or FGA consumers.
	UpdateWithoutPublish(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error)
	// ValidatePrecondition confirms uid exists and, if ifMatch is set, matches
	// the org's current ETag — without writing. The logo-upload path
	// (LFXV2-2016) calls this before uploading bytes to object storage, so a
	// 404/412 is rejected before any bytes are written, not after. It returns
	// the org as currently persisted, which that path also uses to capture the
	// pre-upload Logo_URL__c it may need to roll back to; the read happens
	// either way, so returning it costs no extra round trip.
	ValidatePrecondition(ctx context.Context, uid, ifMatch string) (*model.B2BOrg, error)
	// PublishOrgUpdated publishes indexer and access events for an updated org.
	PublishOrgUpdated(ctx context.Context, current, org *model.B2BOrg)
}

type b2bOrgWriterOrchestrator struct {
	b2bOrgReader           port.B2BOrgReader
	b2bOrgWriter           port.B2BOrgWriter
	memberPublisher        port.MemberPublisher
	globalOrgAdminTeamName string
	auditorTeams           []string
}

// B2BOrgWriterOption configures a b2bOrgWriterOrchestrator.
type B2BOrgWriterOption func(*b2bOrgWriterOrchestrator)

func WithB2BOrgReader(r port.B2BOrgReader) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.b2bOrgReader = r }
}

func WithB2BOrgWriter(w port.B2BOrgWriter) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.b2bOrgWriter = w }
}

func WithB2BOrgPublisher(p port.MemberPublisher) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.memberPublisher = p }
}

func WithGlobalOrgAdminTeamName(name string) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.globalOrgAdminTeamName = name }
}

// WithB2BOrgAuditorTeams sets the LF team names granted blanket auditor access
// on every org this writer publishes.
func WithB2BOrgAuditorTeams(teams []string) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.auditorTeams = teams }
}

// NewB2BOrgWriter constructs a B2BOrgWriter.
func NewB2BOrgWriter(opts ...B2BOrgWriterOption) B2BOrgWriter {
	o := &b2bOrgWriterOrchestrator{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// Create creates a new B2BOrg from the given Salesforce Account SFID and
// publishes the indexer + FGA fan-out. orgAdminTeamName is included in the
// Create FGA body only.
func (o *b2bOrgWriterOrchestrator) Create(ctx context.Context, sfid string) (*model.B2BOrg, error) {
	org, err := o.b2bOrgWriter.CreateB2BOrg(ctx, sfid, model.B2BOrgInput{})
	if err != nil {
		return nil, err
	}
	o.publishEvents(ctx, nil, org, indexerConstants.ActionCreated)
	return org, nil
}

// validateForUpdate fetches the current org and, if ifMatch is set, verifies
// it against the org's current ETag. Shared by Update and ValidatePrecondition
// so the logo-upload preflight check (LFXV2-2016) enforces the identical rule
// a write would.
func (o *b2bOrgWriterOrchestrator) validateForUpdate(ctx context.Context, uid, ifMatch string) (*model.B2BOrg, error) {
	current, err := o.b2bOrgReader.GetB2BOrg(ctx, uid)
	if err != nil {
		return nil, err
	}

	if ifMatch != "" {
		currentETag, etagErr := etag.LFXEtag(current)
		if etagErr != nil {
			return nil, pkgerrors.NewUnexpected("failed to compute etag for b2b org", etagErr)
		}
		if currentETag != ifMatch {
			return nil, pkgerrors.NewPreconditionFailed("b2b org has been modified since last read — refresh and retry")
		}
	}

	return current, nil
}

// ValidatePrecondition implements B2BOrgWriter.
func (o *b2bOrgWriterOrchestrator) ValidatePrecondition(ctx context.Context, uid, ifMatch string) (*model.B2BOrg, error) {
	return o.validateForUpdate(ctx, uid, ifMatch)
}

// Update updates an existing B2BOrg. No-op (returns current) when input.HasChanges() == false.
// Validates the optional ETag before writing.
func (o *b2bOrgWriterOrchestrator) Update(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	return o.update(ctx, uid, input, ifMatch, true)
}

// UpdateWithoutPublish persists a transient internal update without publishing it.
func (o *b2bOrgWriterOrchestrator) UpdateWithoutPublish(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
	return o.update(ctx, uid, input, ifMatch, false)
}

// PublishOrgUpdated publishes indexer and access events for an updated org.
func (o *b2bOrgWriterOrchestrator) PublishOrgUpdated(ctx context.Context, current, org *model.B2BOrg) {
	o.publishEvents(ctx, current, org, indexerConstants.ActionUpdated)
}

func (o *b2bOrgWriterOrchestrator) update(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string, publish bool) (*model.B2BOrg, error) {
	current, err := o.validateForUpdate(ctx, uid, ifMatch)
	if err != nil {
		return nil, err
	}
	if ifMatch != "" {
		input.IfUnmodifiedSince = current.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	}

	if !input.HasChanges() {
		return current, nil
	}

	org, err := o.b2bOrgWriter.UpdateB2BOrg(ctx, uid, input)
	if err != nil {
		return nil, err
	}

	if publish {
		o.publishEvents(ctx, current, org, indexerConstants.ActionUpdated)
	}
	return org, nil
}

// publishEvents fans out an indexer message (sequential) then an FGA errgroup
// (update_access + reparenting child-list messages). Publish failures are
// swallowed and logged — /admin/reindex recovers missed records.
func (o *b2bOrgWriterOrchestrator) publishEvents(ctx context.Context, current, org *model.B2BOrg, action indexerConstants.MessageAction) {
	// Writer path is single-org (one HTTP request) — use the single-record fetch.
	// CDC uses FetchChildUIDsByParentUIDs in batch before calling publishB2BOrgUpsertEvents.
	childUIDs, err := o.b2bOrgReader.FetchChildUIDsByParentUID(ctx, org.UID)
	if err != nil {
		slog.WarnContext(ctx, "failed to fetch child UIDs for indexer", "org_uid", org.UID, "err", err)
	} else {
		org.IsParent = len(childUIDs) > 0
	}

	orgAdminTeamName := ""
	if action == indexerConstants.ActionCreated {
		orgAdminTeamName = o.globalOrgAdminTeamName
	}
	// auditorTeams is deliberately not gated on ActionCreated the way
	// orgAdminTeamName is: the grant is required on update too, so an org that
	// existed before this change picks it up on its next write.
	publishB2BOrgUpsertEvents(ctx, o.b2bOrgReader, o.memberPublisher, current, org, action, orgAdminTeamName, o.auditorTeams)
}

// publishB2BOrgUpsertEvents fans out an indexer message (sequential) then an
// FGA errgroup (update_access + reparenting child-list messages). It is shared
// by the writer orchestrator and the CDC consumer. orgAdminTeamName controls
// whether the global org-admin team relation is included in the FGA message
// (writers pass it only on ActionCreated; CDC always passes it for ActionUpdated).
// auditorTeams, by contrast, is passed unconditionally by both callers — the
// blanket auditor grant applies on create and update alike.
// Publish failures are swallowed — /admin/reindex recovers missed records.
//
// Precondition: callers must set org.IsParent before calling. The writer uses
// FetchChildUIDsByParentUID (single-org); the CDC consumer uses
// FetchChildUIDsByParentUIDs (batched, one call per batch) and derives the bool
// inline. This function publishes the pre-computed value.
func publishB2BOrgUpsertEvents(
	ctx context.Context,
	reader port.B2BOrgReader,
	publisher port.MemberPublisher,
	current, org *model.B2BOrg,
	action indexerConstants.MessageAction,
	orgAdminTeamName string,
	auditorTeams []string,
) {
	// Indexer first — must be sequential (before the errgroup).
	PublishB2BOrgIndexer(ctx, publisher, org, action)

	fgaMsg := BuildB2BOrgFGAMessage(org, B2BOrgFGARefs{
		GlobalOrgAdminTeamName: orgAdminTeamName,
		AuditorTeams:           auditorTeams,
	})

	// Pre-fetch child lists before starting the errgroup (immutable inputs).
	oldParentChildren, newParentChildren := fetchChildListsForReparent(ctx, reader, current, org)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return publisher.Access(gCtx, constants.FGASyncUpdateAccessSubject, fgaMsg)
	})
	for _, reparentMsg := range BuildB2BOrgReparentingMessages(current, org, oldParentChildren, newParentChildren) {
		msg := reparentMsg
		g.Go(func() error {
			return publisher.Access(gCtx, constants.FGASyncUpdateAccessSubject, msg)
		})
	}

	if pubErr := g.Wait(); pubErr != nil {
		slog.WarnContext(ctx, "b2b org FGA publish failed",
			"uid", org.UID, "error", pubErr, "publish_failed_for_backfill_repair", true)
	}
}

// fetchChildListsForReparent computes post-move child-UID slices for the old
// and new parent when a b2b_org's ParentUID changes. Returns (nil, nil) when
// the parent is unchanged — BuildB2BOrgReparentingMessages treats nil as "skip".
// Shared by the writer orchestrator and the CDC consumer.
func fetchChildListsForReparent(ctx context.Context, reader port.B2BOrgReader, current, org *model.B2BOrg) (oldChildren, newChildren []string) {
	oldParent := ""
	if current != nil {
		oldParent = current.ParentUID
	}
	newParent := org.ParentUID
	if oldParent == newParent {
		return nil, nil
	}

	g, gCtx := errgroup.WithContext(ctx)

	if oldParent != "" {
		g.Go(func() error {
			uids, err := reader.FetchChildUIDsByParentUID(gCtx, oldParent)
			if err != nil {
				slog.WarnContext(ctx, "failed to fetch children of old parent for FGA child-list update",
					"old_parent_uid", oldParent, "org_uid", org.UID, "error", err,
					"publish_failed_for_backfill_repair", true)
				return nil
			}
			for _, u := range uids {
				if u != org.UID {
					oldChildren = append(oldChildren, u)
				}
			}
			if oldChildren == nil {
				oldChildren = []string{} // non-nil empty = emit clear
			}
			return nil
		})
	}

	if newParent != "" {
		g.Go(func() error {
			uids, err := reader.FetchChildUIDsByParentUID(gCtx, newParent)
			if err != nil {
				slog.WarnContext(ctx, "failed to fetch children of new parent for FGA child-list update",
					"new_parent_uid", newParent, "org_uid", org.UID, "error", err,
					"publish_failed_for_backfill_repair", true)
				return nil
			}
			newChildren = uids
			if !slices.Contains(newChildren, org.UID) {
				newChildren = append(newChildren, org.UID)
			}
			return nil
		})
	}

	_ = g.Wait()
	return oldChildren, newChildren
}
