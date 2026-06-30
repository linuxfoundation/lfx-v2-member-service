// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"fmt"
	"time"
)

// ProjectInfo carries the persisted project reference for a workspace
// association. Workspace writes treat project identifiers as opaque strings and
// do not validate or enrich them.
type ProjectInfo struct {
	UID  string `json:"uid"`
	SFID string `json:"sfid,omitempty"`
	Name string `json:"name,omitempty"`
	Slug string `json:"slug,omitempty"`
}

// WorkspaceProject is a project reference associated with a workspace.
type WorkspaceProject struct {
	// ProjectUID is the stored opaque project reference.
	ProjectUID string `json:"project_uid"`
	// ProjectSFID is reserved; the write path does not populate it.
	ProjectSFID string `json:"project_sfid,omitempty"`
	// ProjectSlug is reserved; the write path does not populate it.
	ProjectSlug string `json:"project_slug,omitempty"`
	// ProjectName is reserved; the write path does not populate it.
	ProjectName string `json:"project_name,omitempty"`
	// CreatedBy is the LFID username of the principal who added this project.
	CreatedBy string `json:"created_by,omitempty"`
	// UpdatedBy is the LFID username of the principal who last touched this association.
	UpdatedBy string    `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AssociationID returns the compound ObjectID used in the indexer:
// "{workspaceUID}/{projectUID}".
func (wp *WorkspaceProject) AssociationID(workspaceUID string) string {
	return workspaceUID + "/" + wp.ProjectUID
}

// Tags returns the indexer tags for a workspace-project association.
// Mirrors the tag style of model.ProjectMembership.Tags(): bare compound ID
// first, then prefixed relation refs. Omits empty values.
func (wp *WorkspaceProject) Tags(orgUID, workspaceUID string) []string {
	if wp == nil {
		return nil
	}
	var tags []string
	if workspaceUID != "" && wp.ProjectUID != "" {
		tags = append(tags, workspaceUID+"/"+wp.ProjectUID)
	}
	if orgUID != "" {
		tags = append(tags, TagPrefixB2BOrgUID+orgUID)
	}
	if workspaceUID != "" {
		tags = append(tags, TagPrefixB2BOrgWorkspaceUID+workspaceUID)
	}
	if wp.ProjectUID != "" {
		tags = append(tags, TagPrefixProjectUID+wp.ProjectUID)
	}
	if wp.ProjectSlug != "" {
		tags = append(tags, TagPrefixProjectSlug+wp.ProjectSlug)
	}
	if wp.ProjectSFID != "" {
		tags = append(tags, TagPrefixProjectSFID+wp.ProjectSFID)
	}
	return tags
}

// Workspace is a named container of project associations within a b2b_org.
// Projects are stored separately in the WorkspaceProjects aggregate.
type Workspace struct {
	// UID is the workspace's v2 UUID.
	UID string `json:"uid"`
	// Name is the workspace display name; unique within the org.
	Name string `json:"name"`
	// CreatedBy is the LFID username of the principal who created the workspace.
	CreatedBy string `json:"created_by,omitempty"`
	// UpdatedBy is the LFID username of the principal who last mutated the workspace.
	UpdatedBy string    `json:"updated_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OrgWorkspaces is the root document stored in the "org-workspaces" NATS KV
// bucket. One document per b2b_org; keyed "org-workspaces.{orgUID}".
type OrgWorkspaces struct {
	// OrgUID is the parent b2b_org UID this document belongs to.
	OrgUID string `json:"org_uid"`
	// Workspaces lists all workspaces owned by the org.
	Workspaces []Workspace `json:"workspaces,omitempty"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// WorkspaceProjects is the document stored in the "org_workspace_projects"
// NATS KV bucket. One document per workspace; keyed
// "org_workspace_projects.{workspaceUID}".
type WorkspaceProjects struct {
	// WorkspaceUID is the parent workspace UID this document belongs to.
	WorkspaceUID string `json:"workspace_uid"`
	// OrgUID is the parent b2b_org UID (denormalised for indexer context).
	OrgUID string `json:"org_uid"`
	// Projects lists all project associations for this workspace.
	Projects  []WorkspaceProject `json:"projects,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// Tag prefix consts for workspace indexer tags — mirror the TagPrefix* style
// used by b2b_org_settings but scoped to workspaces.
const (
	// TagPrefixB2BOrgWorkspaceUID is the per-workspace type-prefixed UID tag.
	TagPrefixB2BOrgWorkspaceUID = "b2b_org_workspace_uid:"
	// TagPrefixB2BOrgUID is the parent org UID tag.
	TagPrefixB2BOrgUID = "b2b_org_uid:"
	// TagPrefixProjectUID is the per-project UID tag.
	TagPrefixProjectUID = "project_uid:"
	// TagPrefixProjectSFID is the per-project Salesforce ID tag.
	TagPrefixProjectSFID = "project_sfid:"
	// TagPrefixProjectSlug is the per-project URL slug tag.
	TagPrefixProjectSlug = "project_slug:"
)

// Tags returns the indexer tags for a single Workspace within its parent org.
// Metadata-only: bare UID, b2b_org_workspace_uid:, b2b_org_uid:.
// No project tags — those live on WorkspaceProject.Tags().
func (w *Workspace) Tags(orgUID string) []string {
	if w == nil {
		return nil
	}
	var tags []string
	if w.UID != "" {
		tags = append(tags, w.UID)
		tags = append(tags, TagPrefixB2BOrgWorkspaceUID+w.UID)
	}
	if orgUID != "" {
		tags = append(tags, TagPrefixB2BOrgUID+orgUID)
	}
	return tags
}

// FulltextTokens returns the free-text search tokens for a workspace (name only).
func (w *Workspace) FulltextTokens() []string {
	if w == nil {
		return nil
	}
	if w.Name == "" {
		return nil
	}
	return []string{w.Name}
}

// FindWorkspace returns the workspace with the given UID, or nil if not found.
func (o *OrgWorkspaces) FindWorkspace(uid string) *Workspace {
	if o == nil {
		return nil
	}
	for i := range o.Workspaces {
		if o.Workspaces[i].UID == uid {
			return &o.Workspaces[i]
		}
	}
	return nil
}

// FindWorkspaceByName returns the first workspace whose Name exactly matches
// name (case-sensitive), or nil if not found.
func (o *OrgWorkspaces) FindWorkspaceByName(name string) *Workspace {
	if o == nil {
		return nil
	}
	for i := range o.Workspaces {
		if o.Workspaces[i].Name == name {
			return &o.Workspaces[i]
		}
	}
	return nil
}

// CloneOrgWorkspaces returns a shallow copy of the OrgWorkspaces document with
// the Workspaces slice duplicated. A nil source yields a fresh document.
func CloneOrgWorkspaces(o *OrgWorkspaces, orgUID string, now time.Time) *OrgWorkspaces {
	if o == nil {
		return &OrgWorkspaces{OrgUID: orgUID, UpdatedAt: now}
	}
	clone := &OrgWorkspaces{
		OrgUID:    orgUID,
		UpdatedAt: o.UpdatedAt,
	}
	if len(o.Workspaces) > 0 {
		clone.Workspaces = make([]Workspace, len(o.Workspaces))
		copy(clone.Workspaces, o.Workspaces)
	}
	return clone
}

// CloneWorkspaceProjects returns a shallow copy of the WorkspaceProjects
// document with the Projects slice duplicated. A nil source yields a fresh
// document for workspaceUID + orgUID.
func CloneWorkspaceProjects(p *WorkspaceProjects, workspaceUID, orgUID string, now time.Time) *WorkspaceProjects {
	if p == nil {
		return &WorkspaceProjects{WorkspaceUID: workspaceUID, OrgUID: orgUID, UpdatedAt: now}
	}
	clone := &WorkspaceProjects{
		WorkspaceUID: workspaceUID,
		OrgUID:       orgUID,
		UpdatedAt:    p.UpdatedAt,
	}
	if len(p.Projects) > 0 {
		clone.Projects = make([]WorkspaceProject, len(p.Projects))
		copy(clone.Projects, p.Projects)
	}
	return clone
}

// WorkspaceProjectByUID returns a pointer to the WorkspaceProject with the
// given projectUID, or nil if not present.
func (p *WorkspaceProjects) WorkspaceProjectByUID(projectUID string) *WorkspaceProject {
	if p == nil {
		return nil
	}
	for i := range p.Projects {
		if p.Projects[i].ProjectUID == projectUID {
			return &p.Projects[i]
		}
	}
	return nil
}

// RemoveProject removes the project with the given projectUID from Projects.
// Returns true if a project was removed.
func (p *WorkspaceProjects) RemoveProject(projectUID string) bool {
	if p == nil {
		return false
	}
	for i, wp := range p.Projects {
		if wp.ProjectUID == projectUID {
			p.Projects = append(p.Projects[:i], p.Projects[i+1:]...)
			return true
		}
	}
	return false
}

// WorkspaceTagKey returns a formatted tag; exposed as a package-level helper
// so tests and callers outside the model can construct expected tag values
// without importing format strings.
func WorkspaceTagKey(prefix, value string) string {
	return fmt.Sprintf("%s%s", prefix, value)
}
