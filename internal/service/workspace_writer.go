// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// ── Input types ───────────────────────────────────────────────────────────────

// WorkspaceCreate carries the validated parameters for creating a new workspace.
type WorkspaceCreate struct {
	OrgUID    string
	Name      string
	CreatedBy string
	IfMatch   string // optional ETag pre-check on the parent OrgWorkspaces doc
}

// WorkspaceUpdate carries the validated parameters for renaming a workspace.
type WorkspaceUpdate struct {
	OrgUID       string
	WorkspaceUID string
	Name         string
	UpdatedBy    string
	IfMatch      string
}

// WorkspaceDelete carries the validated parameters for deleting a workspace.
type WorkspaceDelete struct {
	OrgUID       string
	WorkspaceUID string
	IfMatch      string
}

// WorkspaceProjectAdd carries the validated parameters for adding a single
// project to a workspace.
type WorkspaceProjectAdd struct {
	OrgUID       string
	WorkspaceUID string
	ProjectID    string // UID or slug
	CreatedBy    string
	IfMatch      string
}

// WorkspaceProjectsBulkAdd carries the validated parameters for adding multiple
// projects to a workspace in one operation.
type WorkspaceProjectsBulkAdd struct {
	OrgUID       string
	WorkspaceUID string
	ProjectIDs   []string // UIDs or slugs
	CreatedBy    string
	IfMatch      string
}

// WorkspaceProjectRemove carries the validated parameters for removing a project
// from a workspace.
type WorkspaceProjectRemove struct {
	OrgUID       string
	WorkspaceUID string
	ProjectUID   string
	IfMatch      string
}

// WorkspaceMetaResult is returned by CreateWorkspace and UpdateWorkspace.
// Registry is the updated OrgWorkspaces document — the same type the
// orchestrator validates If-Match against (following the OrgSettings pattern:
// "return the same type you validate IfMatch against").
type WorkspaceMetaResult struct {
	// Workspace is the specific workspace that was created or updated.
	Workspace *model.Workspace
	// Registry is the updated org-level registry document (OrgWorkspaces).
	Registry *model.OrgWorkspaces
}

// WorkspaceProjectResult is returned by AddProject and RemoveProject.
type WorkspaceProjectResult struct {
	// Workspace is the workspace metadata.
	Workspace *model.Workspace
	// Projects is the updated projects document for the workspace.
	Projects *model.WorkspaceProjects
}

// WorkspaceBulkResult is returned by AddProjectsBulk.
type WorkspaceBulkResult struct {
	// Workspace is the updated workspace after all successful additions.
	Workspace *model.Workspace
	// Projects is the updated projects document for the workspace.
	Projects *model.WorkspaceProjects
	// Succeeded holds the ProjectInfo for each successfully added project.
	Succeeded []model.ProjectInfo
	// Failed holds per-item errors aligned to the input ProjectIDs slice.
	// Indices where addition succeeded carry a nil error.
	Failed []error
}

// ── Interface ─────────────────────────────────────────────────────────────────

// WorkspaceWriter orchestrates workspace mutation use cases.
type WorkspaceWriter interface {
	CreateWorkspace(ctx context.Context, in WorkspaceCreate) (*WorkspaceMetaResult, error)
	UpdateWorkspace(ctx context.Context, in WorkspaceUpdate) (*WorkspaceMetaResult, error)
	DeleteWorkspace(ctx context.Context, in WorkspaceDelete) error
	AddProject(ctx context.Context, in WorkspaceProjectAdd) (*WorkspaceProjectResult, error)
	AddProjectsBulk(ctx context.Context, in WorkspaceProjectsBulkAdd) (*WorkspaceBulkResult, error)
	RemoveProject(ctx context.Context, in WorkspaceProjectRemove) (*WorkspaceProjectResult, error)
}

// ── Orchestrator ──────────────────────────────────────────────────────────────

type workspaceWriterOrchestrator struct {
	workspacesReader        port.OrgWorkspacesReader
	workspacesWriter        port.OrgWorkspacesWriter
	workspaceProjectsReader port.WorkspaceProjectsReader
	workspaceProjectsWriter port.WorkspaceProjectsWriter
	b2bOrgReader            port.B2BOrgReader
	projectResolver         port.ProjectResolver
	publisher               port.MemberPublisher
}

// WorkspaceWriterOption configures a workspaceWriterOrchestrator.
type WorkspaceWriterOption func(*workspaceWriterOrchestrator)

func WithWorkspacesReader(r port.OrgWorkspacesReader) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.workspacesReader = r }
}

func WithWorkspacesWriter(w port.OrgWorkspacesWriter) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.workspacesWriter = w }
}

func WithWorkspaceProjectsReader(r port.WorkspaceProjectsReader) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.workspaceProjectsReader = r }
}

func WithWorkspaceProjectsWriter(w port.WorkspaceProjectsWriter) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.workspaceProjectsWriter = w }
}

func WithWorkspacesB2BOrgReader(r port.B2BOrgReader) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.b2bOrgReader = r }
}

func WithWorkspacesProjectResolver(r port.ProjectResolver) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.projectResolver = r }
}

func WithWorkspacesPublisher(p port.MemberPublisher) WorkspaceWriterOption {
	return func(o *workspaceWriterOrchestrator) { o.publisher = p }
}

// NewWorkspaceWriter constructs a WorkspaceWriter.
func NewWorkspaceWriter(opts ...WorkspaceWriterOption) WorkspaceWriter {
	o := &workspaceWriterOrchestrator{}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// ── Use-case methods ──────────────────────────────────────────────────────────

// CreateWorkspace creates a new workspace in the org's registry document.
// Name must be unique within the org (case-sensitive). Returns Conflict on
// duplicate name and NotFound if the parent org does not exist.
func (o *workspaceWriterOrchestrator) CreateWorkspace(ctx context.Context, in WorkspaceCreate) (*WorkspaceMetaResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, pkgerrors.NewValidation("workspace name is required")
	}

	existing, revision, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if err := checkWorkspacesIfMatch(existing, in.IfMatch); err != nil {
		return nil, err
	}

	// Guard: verify parent org exists on first-write.
	if existing == nil && o.b2bOrgReader != nil {
		if _, err := o.guardOrg(ctx, in.OrgUID); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	updated := model.CloneOrgWorkspaces(existing, in.OrgUID, now)

	if updated.FindWorkspaceByName(name) != nil {
		return nil, pkgerrors.NewConflict("a workspace with that name already exists in this organization")
	}

	ws := model.Workspace{
		UID:       uuid.New().String(),
		Name:      name,
		CreatedBy: in.CreatedBy,
		UpdatedBy: in.CreatedBy,
		CreatedAt: now,
		UpdatedAt: now,
	}
	updated.Workspaces = append(updated.Workspaces, ws)
	updated.UpdatedAt = now

	if err := o.persistAndPublishRegistry(ctx, existing, updated, revision, &ws, indexerConstants.ActionCreated); err != nil {
		return nil, err
	}
	return &WorkspaceMetaResult{Workspace: &ws, Registry: updated}, nil
}

// UpdateWorkspace renames an existing workspace. Name must be unique within the org.
func (o *workspaceWriterOrchestrator) UpdateWorkspace(ctx context.Context, in WorkspaceUpdate) (*WorkspaceMetaResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, pkgerrors.NewValidation("workspace name is required")
	}

	existing, revision, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}
	if err := checkWorkspacesIfMatch(existing, in.IfMatch); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	updated := model.CloneOrgWorkspaces(existing, in.OrgUID, now)

	ws := updated.FindWorkspace(in.WorkspaceUID)
	if ws == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	// Disallow rename to an existing name (unless it's the same workspace).
	if conflict := updated.FindWorkspaceByName(name); conflict != nil && conflict.UID != in.WorkspaceUID {
		return nil, pkgerrors.NewConflict("a workspace with that name already exists in this organization")
	}

	ws.Name = name
	ws.UpdatedBy = in.UpdatedBy
	ws.UpdatedAt = now
	updated.UpdatedAt = now

	if err := o.persistAndPublishRegistry(ctx, existing, updated, revision, ws, indexerConstants.ActionUpdated); err != nil {
		return nil, err
	}
	// Return a copy of the workspace alongside the updated registry (caller hashes registry for ETag).
	wsCopy := *ws
	return &WorkspaceMetaResult{Workspace: &wsCopy, Registry: updated}, nil
}

// DeleteWorkspace removes a workspace and all its project associations (cascade).
// Returns nil on success.
func (o *workspaceWriterOrchestrator) DeleteWorkspace(ctx context.Context, in WorkspaceDelete) error {
	existing, revision, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return err
	}
	if existing == nil {
		return pkgerrors.NewNotFound("workspace not found")
	}
	if err := checkWorkspacesIfMatch(existing, in.IfMatch); err != nil {
		return err
	}

	// Verify the workspace exists before publishing a delete.
	found := existing.FindWorkspace(in.WorkspaceUID)
	if found == nil {
		// Already-missing sweep: the registry read succeeded but this workspace
		// UID is gone, yet a stale indexed document may survive. Best-effort
		// tombstone using path params, then preserve the 404. Association IDs are
		// unavailable here, so only the workspace document is swept.
		o.sweepAlreadyMissingWorkspace(ctx, in)
		return pkgerrors.NewNotFound("workspace not found")
	}

	// Read and capture projects before deletion (for indexer delete publishing).
	// Error is intentionally swallowed — project delete events are fire-and-forget;
	// missing them is recoverable by backfill repair.
	var projects *model.WorkspaceProjects
	if o.workspaceProjectsReader != nil {
		var readErr error
		projects, _, readErr = o.workspaceProjectsReader.GetWorkspaceProjects(ctx, in.WorkspaceUID)
		if readErr != nil {
			slog.DebugContext(ctx, "could not read workspace projects for delete event publishing",
				"workspace_uid", in.WorkspaceUID,
				"error", readErr,
			)
		}
	}

	now := time.Now().UTC()
	updated := model.CloneOrgWorkspaces(existing, in.OrgUID, now)
	// Remove the workspace by UID.
	filtered := updated.Workspaces[:0]
	for _, w := range updated.Workspaces {
		if w.UID != in.WorkspaceUID {
			filtered = append(filtered, w)
		}
	}
	updated.Workspaces = filtered
	updated.UpdatedAt = now

	// Delete the projects document before committing the registry removal so that
	// a failed delete leaves the workspace UID intact in the registry and a retried
	// DELETE can complete the cascade. DeleteWorkspaceProjects is idempotent (safe
	// to retry), so returning an error here is the correct failure mode.
	if o.workspaceProjectsWriter != nil {
		if delErr := o.workspaceProjectsWriter.DeleteWorkspaceProjects(ctx, in.WorkspaceUID); delErr != nil {
			return delErr
		}
	}

	if err := o.workspacesWriter.UpdateWorkspaces(ctx, updated, revision); err != nil {
		return err
	}

	// Fire-and-forget: one GetB2BOrg fetch shared by workspace + project indexer deletes.
	if o.b2bOrgReader != nil && o.publisher != nil {
		org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
		if orgErr == nil && org != nil {
			PublishWorkspaceIndexer(ctx, o.publisher, org, found, indexerConstants.ActionDeleted)
			if projects != nil {
				for _, wp := range projects.Projects {
					PublishWorkspaceProjectIndexer(ctx, o.publisher, org, found, wp, *projects, indexerConstants.ActionDeleted)
				}
			}
		}
	}

	return nil
}

// AddProject enriches and appends a single project to a workspace's projects document.
// Idempotent: if the project is already associated, returns the current state
// without error (BR-6). Returns NotFound if the workspace does not exist and
// Validation (→ HTTP 400) if the project cannot be resolved.
func (o *workspaceWriterOrchestrator) AddProject(ctx context.Context, in WorkspaceProjectAdd) (*WorkspaceProjectResult, error) {
	existing, _, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	ws := existing.FindWorkspace(in.WorkspaceUID)
	if ws == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	if o.projectResolver == nil {
		return nil, pkgerrors.NewUnexpected("project resolver not configured")
	}

	// Enrich project (write-time snapshot).
	info, resolveErr := o.projectResolver.ResolveProject(ctx, in.ProjectID)
	if resolveErr != nil {
		return nil, resolveErr
	}

	// Read the projects document (lazy create if not exists).
	existingProjects, projectsRevision, err := o.workspaceProjectsReader.GetWorkspaceProjects(ctx, in.WorkspaceUID)
	if err != nil {
		return nil, err
	}

	// Check If-Match against the projects document — the doc actually being
	// mutated. Matches OrgSettings pattern: validate against the same type
	// the HTTP layer hashes for the response ETag.
	if err := checkWorkspaceProjectsIfMatch(existingProjects, in.IfMatch); err != nil {
		return nil, err
	}

	// Idempotency: already associated → return current state without cloning.
	if existingProjects != nil && existingProjects.WorkspaceProjectByUID(info.UID) != nil {
		wsCopy := *ws
		return &WorkspaceProjectResult{
			Workspace: &wsCopy,
			Projects:  existingProjects,
		}, nil
	}

	now := time.Now().UTC()
	updatedProjects := model.CloneWorkspaceProjects(existingProjects, in.WorkspaceUID, in.OrgUID, now)
	updatedProjects.Projects = append(updatedProjects.Projects, model.WorkspaceProject{
		ProjectUID:  info.UID,
		ProjectSFID: info.SFID,
		ProjectSlug: info.Slug,
		ProjectName: info.Name,
		CreatedBy:   in.CreatedBy,
		UpdatedBy:   in.CreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	updatedProjects.UpdatedAt = now

	// CAS write projects document (lazy create: projectsRevision==0 on first add).
	if err := o.workspaceProjectsWriter.UpdateWorkspaceProjects(ctx, updatedProjects, projectsRevision); err != nil {
		return nil, err
	}

	// Fire-and-forget: publish indexer create for the new association.
	if o.b2bOrgReader != nil && o.publisher != nil {
		org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
		if orgErr == nil && org != nil {
			wp := updatedProjects.WorkspaceProjectByUID(info.UID)
			if wp != nil {
				PublishWorkspaceProjectIndexer(ctx, o.publisher, org, ws, *wp, *updatedProjects, indexerConstants.ActionCreated)
			}
		}
	}

	wsCopy := *ws
	return &WorkspaceProjectResult{
		Workspace: &wsCopy,
		Projects:  updatedProjects,
	}, nil
}

// AddProjectsBulk adds multiple projects to a workspace using a batch SOQL enrichment.
// Per-item resolution failures are collected; only successfully enriched projects are
// persisted in a single CAS write.
func (o *workspaceWriterOrchestrator) AddProjectsBulk(ctx context.Context, in WorkspaceProjectsBulkAdd) (*WorkspaceBulkResult, error) {
	existing, _, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	ws := existing.FindWorkspace(in.WorkspaceUID)
	if ws == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	if o.projectResolver == nil {
		return nil, pkgerrors.NewUnexpected("project resolver not configured")
	}

	// Batch-resolve all project identifiers in one round-trip.
	infos, resolveErrs := o.projectResolver.ResolveProjectsBatch(ctx, in.ProjectIDs)

	var succeeded []model.ProjectInfo
	failedOut := make([]error, len(in.ProjectIDs))

	// Read the projects document (lazy create if not exists).
	existingProjects, projectsRevision, err := o.workspaceProjectsReader.GetWorkspaceProjects(ctx, in.WorkspaceUID)
	if err != nil {
		return nil, err
	}

	// Check If-Match against the projects document — the doc actually being
	// mutated. Matches OrgSettings pattern: validate against the same type
	// the HTTP layer hashes for the response ETag.
	if err := checkWorkspaceProjectsIfMatch(existingProjects, in.IfMatch); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	updatedProjects := model.CloneWorkspaceProjects(existingProjects, in.WorkspaceUID, in.OrgUID, now)
	newlyAdded := []*model.WorkspaceProject{}

	for i, info := range infos {
		if resolveErrs[i] != nil {
			// Infrastructure errors (NATS/Salesforce down) are not per-item failures —
			// fail the whole request so the caller retries rather than receiving a
			// partial-success 200 that masks a dependency outage.
			if !pkgerrors.IsValidation(resolveErrs[i]) {
				return nil, resolveErrs[i]
			}
			failedOut[i] = resolveErrs[i]
			continue
		}
		// Idempotency: skip already-associated projects.
		if updatedProjects.WorkspaceProjectByUID(info.UID) != nil {
			succeeded = append(succeeded, info)
			continue
		}
		wp := model.WorkspaceProject{
			ProjectUID:  info.UID,
			ProjectSFID: info.SFID,
			ProjectSlug: info.Slug,
			ProjectName: info.Name,
			CreatedBy:   in.CreatedBy,
			UpdatedBy:   in.CreatedBy,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		updatedProjects.Projects = append(updatedProjects.Projects, wp)
		newlyAdded = append(newlyAdded, &wp)
		succeeded = append(succeeded, info)
	}

	projectsResult := existingProjects
	if len(newlyAdded) > 0 {
		updatedProjects.UpdatedAt = now
		if err := o.workspaceProjectsWriter.UpdateWorkspaceProjects(ctx, updatedProjects, projectsRevision); err != nil {
			return nil, err
		}
		projectsResult = updatedProjects

		// Fire-and-forget: publish indexer creates for each newly added project.
		if o.b2bOrgReader != nil && o.publisher != nil {
			org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
			if orgErr == nil && org != nil {
				for _, wp := range newlyAdded {
					PublishWorkspaceProjectIndexer(ctx, o.publisher, org, ws, *wp, *updatedProjects, indexerConstants.ActionCreated)
				}
			}
		}
	}

	wsCopy := *ws
	return &WorkspaceBulkResult{
		Workspace: &wsCopy,
		Projects:  projectsResult,
		Succeeded: succeeded,
		Failed:    failedOut,
	}, nil
}

// RemoveProject removes a project association from a workspace's projects document.
// Returns NotFound if either the workspace or the project association does not exist.
func (o *workspaceWriterOrchestrator) RemoveProject(ctx context.Context, in WorkspaceProjectRemove) (*WorkspaceProjectResult, error) {
	existing, _, err := o.workspacesReader.GetWorkspaces(ctx, in.OrgUID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	ws := existing.FindWorkspace(in.WorkspaceUID)
	if ws == nil {
		return nil, pkgerrors.NewNotFound("workspace not found")
	}

	// Read the projects document.
	existingProjects, projectsRevision, err := o.workspaceProjectsReader.GetWorkspaceProjects(ctx, in.WorkspaceUID)
	if err != nil {
		return nil, err
	}
	if existingProjects == nil {
		return nil, pkgerrors.NewNotFound("project not found in workspace")
	}

	// Check If-Match on the projects document.
	if err := checkWorkspaceProjectsIfMatch(existingProjects, in.IfMatch); err != nil {
		return nil, err
	}

	// Verify the project exists before cloning — avoids allocating a clone
	// that would be immediately discarded on the not-found path.
	if existingProjects.WorkspaceProjectByUID(in.ProjectUID) == nil {
		return nil, pkgerrors.NewNotFound("project not found in workspace")
	}

	now := time.Now().UTC()
	updatedProjects := model.CloneWorkspaceProjects(existingProjects, in.WorkspaceUID, in.OrgUID, now)

	removedProject := updatedProjects.WorkspaceProjectByUID(in.ProjectUID)
	removedCopy := *removedProject // safe: checked on existingProjects above

	if !updatedProjects.RemoveProject(in.ProjectUID) {
		return nil, pkgerrors.NewNotFound("project not found in workspace")
	}

	updatedProjects.UpdatedAt = now

	if err := o.workspaceProjectsWriter.UpdateWorkspaceProjects(ctx, updatedProjects, projectsRevision); err != nil {
		return nil, err
	}

	// Fire-and-forget: publish indexer delete for the removed association.
	if o.b2bOrgReader != nil && o.publisher != nil {
		org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
		if orgErr == nil && org != nil {
			PublishWorkspaceProjectIndexer(ctx, o.publisher, org, ws, removedCopy, *updatedProjects, indexerConstants.ActionDeleted)
		}
	}

	wsCopy := *ws
	return &WorkspaceProjectResult{
		Workspace: &wsCopy,
		Projects:  updatedProjects,
	}, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// persistAndPublishRegistry writes the updated registry document (CAS) and fires
// the indexer publish for the workspace metadata. The action describes the operation
// performed on the specific workspace (created/updated/deleted).
func (o *workspaceWriterOrchestrator) persistAndPublishRegistry(
	ctx context.Context,
	existing *model.OrgWorkspaces,
	updated *model.OrgWorkspaces,
	revision uint64,
	ws *model.Workspace,
	action indexerConstants.MessageAction,
) error {
	if err := o.workspacesWriter.UpdateWorkspaces(ctx, updated, revision); err != nil {
		return err
	}
	if o.b2bOrgReader == nil || o.publisher == nil {
		return nil
	}
	_ = existing // available for future create-vs-update action derivation
	org, err := o.b2bOrgReader.GetB2BOrg(ctx, updated.OrgUID)
	if err != nil || org == nil {
		return nil // fire-and-forget: swallow publish errors
	}
	PublishWorkspaceIndexer(ctx, o.publisher, org, ws, action)
	return nil
}

// guardOrg verifies the parent b2b_org exists, returning NotFound if not.
// Returns the org name so callers can use it without a second fetch.
func (o *workspaceWriterOrchestrator) guardOrg(ctx context.Context, orgUID string) (string, error) {
	if o.b2bOrgReader == nil {
		return "", nil
	}
	org, err := o.b2bOrgReader.GetB2BOrg(ctx, orgUID)
	if err != nil {
		return "", err
	}
	if org == nil {
		return "", pkgerrors.NewNotFound("organization not found")
	}
	return org.Name, nil
}

// sweepAlreadyMissingWorkspace publishes a best-effort indexer delete for a
// workspace whose UID is gone from the registry. Fire-and-forget: the endpoint
// still returns 404, so a publish failure must not become a 500. Stub org and
// workspace structs carry only the UIDs needed for the delete tombstone (the
// indexer delete payload is the workspace UID string); no Salesforce/org fetch
// is attempted. Workspace-project associations are skipped here to keep the
// recovery path minimal — projects could be fetched via GetWorkspaceProjects
// but are not needed for the indexer tombstone.
func (o *workspaceWriterOrchestrator) sweepAlreadyMissingWorkspace(ctx context.Context, in WorkspaceDelete) {
	if o.publisher == nil {
		return
	}
	slog.InfoContext(ctx, "workspace already missing on delete — sweeping stale index state",
		"org_uid", in.OrgUID, "workspace_uid", in.WorkspaceUID, "not_found", true)

	PublishWorkspaceIndexer(ctx, o.publisher,
		&model.B2BOrg{UID: in.OrgUID},
		&model.Workspace{UID: in.WorkspaceUID},
		indexerConstants.ActionDeleted)
}

// checkWorkspacesIfMatch validates the optional If-Match precondition against
// the current registry document ETag.
func checkWorkspacesIfMatch(existing *model.OrgWorkspaces, ifMatch string) error {
	return checkIfMatch(existing, ifMatch, "workspace record")
}

// checkWorkspaceProjectsIfMatch validates the optional If-Match precondition against
// a workspace projects document.
func checkWorkspaceProjectsIfMatch(wps *model.WorkspaceProjects, ifMatch string) error {
	return checkIfMatch(wps, ifMatch, "workspace projects record")
}
