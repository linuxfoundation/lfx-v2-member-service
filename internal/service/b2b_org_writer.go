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
}

type b2bOrgWriterOrchestrator struct {
	b2bOrgReader          port.B2BOrgReader
	b2bOrgWriter          port.B2BOrgWriter
	memberPublisher       port.MemberPublisher
	globalOrgAdminTeamUID string
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

func WithGlobalOrgAdminTeamUID(uid string) B2BOrgWriterOption {
	return func(o *b2bOrgWriterOrchestrator) { o.globalOrgAdminTeamUID = uid }
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
// publishes the indexer + FGA fan-out. orgAdminTeamUID is included in the
// Create FGA body only.
func (o *b2bOrgWriterOrchestrator) Create(ctx context.Context, sfid string) (*model.B2BOrg, error) {
	org, err := o.b2bOrgWriter.CreateB2BOrg(ctx, sfid, model.B2BOrgInput{})
	if err != nil {
		return nil, err
	}
	o.publishEvents(ctx, nil, org, indexerConstants.ActionCreated)
	return org, nil
}

// Update updates an existing B2BOrg. No-op (returns current) when input.HasChanges() == false.
// Validates the optional ETag before writing.
func (o *b2bOrgWriterOrchestrator) Update(ctx context.Context, uid string, input model.B2BOrgInput, ifMatch string) (*model.B2BOrg, error) {
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
		input.IfUnmodifiedSince = current.UpdatedAt.UTC().Format(constants.HTTPDateFormat)
	}

	if !input.HasChanges() {
		return current, nil
	}

	org, err := o.b2bOrgWriter.UpdateB2BOrg(ctx, uid, input)
	if err != nil {
		return nil, err
	}

	o.publishEvents(ctx, current, org, indexerConstants.ActionUpdated)
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

	orgAdminTeamUID := ""
	if action == indexerConstants.ActionCreated {
		orgAdminTeamUID = o.globalOrgAdminTeamUID
	}
	publishB2BOrgUpsertEvents(ctx, o.b2bOrgReader, o.memberPublisher, current, org, action, orgAdminTeamUID)
}

// publishB2BOrgUpsertEvents fans out an indexer message (sequential) then an
// FGA errgroup (update_access + reparenting child-list messages). It is shared
// by the writer orchestrator and the CDC consumer. orgAdminTeamUID controls
// whether the global org-admin team relation is included in the FGA message
// (writers pass it only on ActionCreated; CDC always passes it for ActionUpdated).
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
	orgAdminTeamUID string,
) {
	// Indexer first — must be sequential (before the errgroup).
	PublishB2BOrgIndexer(ctx, publisher, org, action)

	fgaMsg := BuildB2BOrgFGAMessage(org, orgAdminTeamUID, nil, nil, nil)

	// Pre-fetch child lists before starting the errgroup (immutable inputs).
	oldParentChildren, newParentChildren := fetchChildListsForReparent(ctx, reader, current, org)

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return publisher.Access(gCtx, constants.FGASyncUpdateAccessSubject, fgaMsg, false)
	})
	for _, reparentMsg := range BuildB2BOrgReparentingMessages(current, org, oldParentChildren, newParentChildren) {
		msg := reparentMsg
		g.Go(func() error {
			return publisher.Access(gCtx, constants.FGASyncUpdateAccessSubject, msg, false)
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
