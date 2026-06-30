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
	ProjectSlug  string // caller-owned identifier; idempotency key within the workspace
	ProjectName  string // optional human-readable name for indexer search/display
	CreatedBy    string
	IfMatch      string
}

// WorkspaceProjectItem is a single project to add in a bulk request.
type WorkspaceProjectItem struct {
	Slug string
	Name string
}

// WorkspaceProjectsBulkAdd carries the validated parameters for adding multiple
// projects to a workspace in one operation.
type WorkspaceProjectsBulkAdd struct {
	OrgUID       string
	WorkspaceUID string
	Projects     []WorkspaceProjectItem
	CreatedBy    string
	IfMatch      string
}

// WorkspaceProjectRemove carries the validated parameters for removing a project
// from a workspace. ProjectUID is the member-service-generated association UID.
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
	// Succeeded holds the projects that were successfully added (or were
	// already present), each carrying the generated UID plus the caller slug.
	Succeeded []model.ProjectInfo
	// Failed holds per-item errors aligned to the input Projects slice.
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
		// Workspace UID is the index object ID, so a wrong-org path must not
		// tombstone a real workspace owned by another org.
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

// AddProject appends a single project reference to a workspace's projects document.
// Idempotent: if the project is already associated, returns the current state
// without error. Returns NotFound if the workspace does not exist and Validation
// (→ HTTP 400) if the supplied project identifier is blank.
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

	wp, err := workspaceProjectFromSlug(in.ProjectSlug, in.ProjectName, in.CreatedBy, time.Now().UTC())
	if err != nil {
		return nil, err
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

	// Idempotency keyed on slug: already associated → return current state
	// without cloning or minting a new UID.
	if existingProjects != nil && existingProjects.WorkspaceProjectBySlug(wp.ProjectSlug) != nil {
		wsCopy := *ws
		return &WorkspaceProjectResult{
			Workspace: &wsCopy,
			Projects:  existingProjects,
		}, nil
	}

	now := wp.CreatedAt
	updatedProjects := model.CloneWorkspaceProjects(existingProjects, in.WorkspaceUID, in.OrgUID, now)
	updatedProjects.Projects = append(updatedProjects.Projects, wp)
	updatedProjects.UpdatedAt = now

	// CAS write projects document (lazy create: projectsRevision==0 on first add).
	if err := o.workspaceProjectsWriter.UpdateWorkspaceProjects(ctx, updatedProjects, projectsRevision); err != nil {
		return nil, err
	}

	// Fire-and-forget: publish indexer create for the new association.
	if o.b2bOrgReader != nil && o.publisher != nil {
		org, orgErr := o.b2bOrgReader.GetB2BOrg(ctx, in.OrgUID)
		if orgErr == nil && org != nil {
			added := updatedProjects.WorkspaceProjectByUID(wp.ProjectUID)
			if added != nil {
				PublishWorkspaceProjectIndexer(ctx, o.publisher, org, ws, *added, *updatedProjects, indexerConstants.ActionCreated)
			}
		}
	}

	wsCopy := *ws
	return &WorkspaceProjectResult{
		Workspace: &wsCopy,
		Projects:  updatedProjects,
	}, nil
}

// AddProjectsBulk adds multiple project references to a workspace without
// validating them against a project catalog.
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

	var succeeded []model.ProjectInfo
	failedOut := make([]error, len(in.Projects))

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
	newlyAdded := []model.WorkspaceProject{}

	for i, item := range in.Projects {
		wp, itemErr := workspaceProjectFromSlug(item.Slug, item.Name, in.CreatedBy, now)
		if itemErr != nil {
			failedOut[i] = itemErr
			continue
		}
		// Idempotency keyed on slug: report the existing record (its UID),
		// not the freshly-minted one that would be discarded.
		if existingWP := updatedProjects.WorkspaceProjectBySlug(wp.ProjectSlug); existingWP != nil {
			succeeded = append(succeeded, workspaceProjectInfo(*existingWP))
			continue
		}
		updatedProjects.Projects = append(updatedProjects.Projects, wp)
		newlyAdded = append(newlyAdded, wp)
		succeeded = append(succeeded, workspaceProjectInfo(wp))
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
				for i := range newlyAdded {
					PublishWorkspaceProjectIndexer(ctx, o.publisher, org, ws, newlyAdded[i], *updatedProjects, indexerConstants.ActionCreated)
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

// workspaceProjectFromSlug builds a new association from the caller-owned slug,
// generating the member-service-owned ProjectUID. The slug is the idempotency
// key; the name is optional indexer metadata.
func workspaceProjectFromSlug(slug, name, principal string, now time.Time) (model.WorkspaceProject, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return model.WorkspaceProject{}, pkgerrors.NewValidation("project_slug is required")
	}

	wp := model.WorkspaceProject{
		ProjectUID:  uuid.NewString(),
		ProjectSlug: slug,
		ProjectName: strings.TrimSpace(name),
		CreatedBy:   principal,
		UpdatedBy:   principal,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return wp, nil
}

func workspaceProjectInfo(wp model.WorkspaceProject) model.ProjectInfo {
	return model.ProjectInfo{
		UID:  wp.ProjectUID,
		Name: wp.ProjectName,
		Slug: wp.ProjectSlug,
	}
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
