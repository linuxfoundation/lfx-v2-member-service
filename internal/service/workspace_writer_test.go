// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	wsOrgUID      = "001dy00000u0UnRAAU" // Salesforce SFID
	wsUID         = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	wsProjectUID  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" // a generated association UID, used for seed/delete
	wsProjectSlug = "test-project-primary"
	wsProjectName = "Test Project Primary"
	wsUser        = "alice"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

func newWorkspaceWriter(
	wsStore *mock.MockOrgWorkspaces,
	wpStore *mock.MockWorkspaceProjects,
) svc.WorkspaceWriter {
	return svc.NewWorkspaceWriter(
		svc.WithWorkspacesReader(wsStore),
		svc.WithWorkspacesWriter(wsStore),
		svc.WithWorkspaceProjectsReader(wpStore),
		svc.WithWorkspaceProjectsWriter(wpStore),
	)
}

// seedWorkspace pre-populates wsStore with a single workspace and returns the
// seeded registry document (for ETag computation in test assertions).
func seedWorkspace(wsStore *mock.MockOrgWorkspaces) *model.OrgWorkspaces {
	ws := model.Workspace{UID: wsUID, Name: "My Workspace"}
	reg := &model.OrgWorkspaces{
		OrgUID:     wsOrgUID,
		Workspaces: []model.Workspace{ws},
	}
	wsStore.Seed(wsOrgUID, reg, 1)
	return reg
}

// ── CreateWorkspace ───────────────────────────────────────────────────────────

func TestWorkspaceWriter_CreateWorkspace_HappyPath(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	result, err := writer.CreateWorkspace(context.Background(), svc.WorkspaceCreate{
		OrgUID:    wsOrgUID,
		Name:      "Alpha",
		CreatedBy: wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "Alpha", result.Workspace.Name)
	assert.NotEmpty(t, result.Workspace.UID)
}

func TestWorkspaceWriter_CreateWorkspace_DuplicateName_ReturnsConflict(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.CreateWorkspace(context.Background(), svc.WorkspaceCreate{
		OrgUID:    wsOrgUID,
		Name:      "My Workspace", // already exists
		CreatedBy: wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsConflict(err), "expected Conflict, got %T: %v", err, err)
}

func TestWorkspaceWriter_CreateWorkspace_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.CreateWorkspace(context.Background(), svc.WorkspaceCreate{
		OrgUID:    wsOrgUID,
		Name:      "Beta",
		CreatedBy: wsUser,
		IfMatch:   "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

func TestWorkspaceWriter_CreateWorkspace_CASConflict_ReturnsConflict(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wsStore.SetPutError(pkgerrors.NewConflict("concurrent write"))
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.CreateWorkspace(context.Background(), svc.WorkspaceCreate{
		OrgUID:    wsOrgUID,
		Name:      "Gamma",
		CreatedBy: wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsConflict(err), "expected Conflict, got %T: %v", err, err)
}

// ── UpdateWorkspace ───────────────────────────────────────────────────────────

func TestWorkspaceWriter_UpdateWorkspace_HappyPath(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	result, err := writer.UpdateWorkspace(context.Background(), svc.WorkspaceUpdate{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Name:         "Renamed",
		UpdatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "Renamed", result.Workspace.Name)
}

func TestWorkspaceWriter_UpdateWorkspace_NotFound_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	// No document seeded — org has no workspaces.
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.UpdateWorkspace(context.Background(), svc.WorkspaceUpdate{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Name:         "Renamed",
		UpdatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_UpdateWorkspace_DuplicateName_ReturnsConflict(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	other := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	reg := &model.OrgWorkspaces{
		OrgUID: wsOrgUID,
		Workspaces: []model.Workspace{
			{UID: wsUID, Name: "Alpha"},
			{UID: other, Name: "Beta"},
		},
	}
	wsStore.Seed(wsOrgUID, reg, 1)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.UpdateWorkspace(context.Background(), svc.WorkspaceUpdate{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Name:         "Beta", // already taken by other workspace
		UpdatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsConflict(err), "expected Conflict, got %T: %v", err, err)
}

// ── DeleteWorkspace ───────────────────────────────────────────────────────────

func TestWorkspaceWriter_DeleteWorkspace_HappyPath_ClearsProjectsDoc(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID, OrgUID: wsOrgUID,
	}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
	})

	require.NoError(t, err)
	// Projects doc should be gone.
	doc, _, readErr := wpStore.GetWorkspaceProjects(context.Background(), wsUID)
	require.NoError(t, readErr)
	assert.Nil(t, doc, "expected projects doc deleted")
}

func TestWorkspaceWriter_DeleteWorkspace_HappyPath_RemovesFromRegistry(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
	})

	require.NoError(t, err)
	reg, _, _ := wsStore.GetWorkspaces(context.Background(), wsOrgUID)
	require.NotNil(t, reg)
	assert.Nil(t, reg.FindWorkspace(wsUID), "workspace should be removed from registry")
}

func TestWorkspaceWriter_DeleteWorkspace_ProjectsDocDeleteFails_RegistryNotCommitted(t *testing.T) {
	// Verifies that a failed projects-doc delete aborts the registry removal so that
	// a retried DELETE can complete the full cascade (workspace UID still in registry).
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}, 1)
	wpStore.SetDeleteError(pkgerrors.NewUnexpected("NATS timeout"))
	writer := newWorkspaceWriter(wsStore, wpStore)

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
	})

	require.Error(t, err)
	// Workspace must still be in the registry — retry can complete the cascade.
	reg, _, _ := wsStore.GetWorkspaces(context.Background(), wsOrgUID)
	require.NotNil(t, reg)
	assert.NotNil(t, reg.FindWorkspace(wsUID), "workspace must remain in registry after failed delete so retry can succeed")
}

func TestWorkspaceWriter_DeleteWorkspace_WorkspaceNotFound_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: "nonexistent-uid",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_DeleteWorkspace_AlreadyMissing_Returns404NoPublish(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore) // registry exists; the requested UID does not
	pub := mock.NewMockMemberPublisher()
	writer := svc.NewWorkspaceWriter(
		svc.WithWorkspacesReader(wsStore),
		svc.WithWorkspacesWriter(wsStore),
		svc.WithWorkspaceProjectsReader(mock.NewMockWorkspaceProjects()),
		svc.WithWorkspaceProjectsWriter(mock.NewMockWorkspaceProjects()),
		svc.WithWorkspacesPublisher(pub),
	)

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: "nonexistent-uid",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "already-missing delete must preserve 404, got %T: %v", err, err)
	assert.Nil(t, pub.LastIndexerPayload,
		"missing workspace delete must not tombstone by path UID; the UID may belong to another org")
	assert.Empty(t, pub.CallOrder, "missing workspace delete must publish nothing")
}

func TestWorkspaceWriter_DeleteWorkspace_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

// ── AddProject ────────────────────────────────────────────────────────────────

func TestWorkspaceWriter_AddProject_HappyPath_GeneratesUID(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  wsProjectSlug,
		ProjectName:  wsProjectName,
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Projects)
	require.Len(t, result.Projects.Projects, 1)
	wp := result.Projects.Projects[0]
	assert.Equal(t, wsProjectSlug, wp.ProjectSlug)
	assert.Equal(t, wsProjectName, wp.ProjectName)
	// project_uid is a member-service-generated UUID, distinct from the slug.
	require.NotEmpty(t, wp.ProjectUID)
	_, parseErr := uuid.Parse(wp.ProjectUID)
	assert.NoError(t, parseErr, "project_uid must be a generated UUID")
	assert.NotEqual(t, wsProjectSlug, wp.ProjectUID)
}

func TestWorkspaceWriter_AddProject_Idempotent_SameSlug_NoSpuriousRevisionBump(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	// Pre-seed the projects doc with the slug already associated.
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID, ProjectSlug: wsProjectSlug}},
	}, 3)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  wsProjectSlug,
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	// Revision must not change — no write should have occurred.
	_, rev, _ := wpStore.GetWorkspaceProjects(context.Background(), wsUID)
	assert.EqualValues(t, 3, rev, "idempotent re-add of the same slug must not bump revision")
	// The pre-existing generated UID is preserved (no new UID minted).
	require.Len(t, result.Projects.Projects, 1)
	assert.Equal(t, wsProjectUID, result.Projects.Projects[0].ProjectUID)
}

func TestWorkspaceWriter_AddProject_BlankSlug_ReturnsValidation(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, wpStore)

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  "  ",
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "blank project_slug should fail validation, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_WorkspaceNotFound_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  wsProjectSlug,
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  wsProjectSlug,
		CreatedBy:    wsUser,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_DirectConstruction_StoresSlugAndGeneratesUID(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	// Build the writer directly from options rather than via the helper.
	writer := svc.NewWorkspaceWriter(
		svc.WithWorkspacesReader(wsStore),
		svc.WithWorkspacesWriter(wsStore),
		svc.WithWorkspaceProjectsReader(wpStore),
		svc.WithWorkspaceProjectsWriter(wpStore),
	)

	result, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectSlug:  "test-project-alpha",
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Projects.Projects, 1)
	assert.Equal(t, "test-project-alpha", result.Projects.Projects[0].ProjectSlug)
	require.NotEmpty(t, result.Projects.Projects[0].ProjectUID)
}

// ── AddProjectsBulk ───────────────────────────────────────────────────────────

func TestWorkspaceWriter_AddProjectsBulk_GeneratesUIDsPerSlug(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Projects: []svc.WorkspaceProjectItem{
			{Slug: "test-project-alpha", Name: "Alpha"},
			{Slug: "test-project-beta"},
		},
		CreatedBy: wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Succeeded, 2)
	assert.NoError(t, result.Failed[0])
	assert.NoError(t, result.Failed[1])
	require.Len(t, result.Projects.Projects, 2)
	assert.Equal(t, "test-project-alpha", result.Projects.Projects[0].ProjectSlug)
	assert.Equal(t, "test-project-beta", result.Projects.Projects[1].ProjectSlug)
	// Each association gets its own distinct, non-empty generated UID.
	uid0 := result.Projects.Projects[0].ProjectUID
	uid1 := result.Projects.Projects[1].ProjectUID
	require.NotEmpty(t, uid0)
	require.NotEmpty(t, uid1)
	assert.NotEqual(t, uid0, uid1)
	for _, info := range result.Succeeded {
		assert.NotEmpty(t, info.UID, "succeeded entry must carry the generated UID")
		assert.NotEmpty(t, info.Slug, "succeeded entry must carry the caller slug")
	}
}

func TestWorkspaceWriter_AddProjectsBulk_AllAlreadyAssociated_NoRevisionBump(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID, ProjectSlug: wsProjectSlug}},
	}, 5)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Projects:     []svc.WorkspaceProjectItem{{Slug: wsProjectSlug}},
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.Len(t, result.Succeeded, 1)
	// Idempotent success reports the existing generated UID, not a fresh one.
	assert.Equal(t, wsProjectUID, result.Succeeded[0].UID)
	// No new write — revision must not change.
	_, rev, _ := wpStore.GetWorkspaceProjects(context.Background(), wsUID)
	assert.EqualValues(t, 5, rev, "idempotent bulk re-add must not bump revision")
	// Result must carry the existing doc (nil would be returned if the empty-projects branch were hit).
	assert.NotNil(t, result.Projects)
}

func TestWorkspaceWriter_AddProjectsBulk_BlankSlug_ReturnsItemFailure(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Projects:     []svc.WorkspaceProjectItem{{Slug: "test-project-alpha"}, {Slug: " "}},
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Succeeded, 1)
	assert.Error(t, result.Failed[1], "blank project_slug should fail validation")
	assert.NoError(t, result.Failed[0])
}

func TestWorkspaceWriter_AddProjectsBulk_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	_, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Projects:     []svc.WorkspaceProjectItem{{Slug: wsProjectSlug}},
		CreatedBy:    wsUser,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProjectsBulk_ValidIfMatch_Succeeds(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		Projects:     []svc.WorkspaceProjectItem{{Slug: wsProjectSlug}},
		CreatedBy:    wsUser,
		IfMatch:      mustEtag(t, existingProjects),
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Succeeded, 1)
}

// ── RemoveProject ─────────────────────────────────────────────────────────────

func TestWorkspaceWriter_RemoveProject_HappyPath(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID}},
	}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	result, err := writer.RemoveProject(context.Background(), svc.WorkspaceProjectRemove{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectUID:   wsProjectUID,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Projects.Projects, 0)
}

func TestWorkspaceWriter_RemoveProject_ProjectNotAssociated_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	_, err := writer.RemoveProject(context.Background(), svc.WorkspaceProjectRemove{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectUID:   wsProjectUID, // not associated
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_RemoveProject_WorkspaceNotFound_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects())

	_, err := writer.RemoveProject(context.Background(), svc.WorkspaceProjectRemove{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectUID:   wsProjectUID,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_RemoveProject_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID}},
	}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore)

	_, err := writer.RemoveProject(context.Background(), svc.WorkspaceProjectRemove{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectUID:   wsProjectUID,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}
