// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	wsOrgUID     = "001dy00000u0UnRAAU" // Salesforce SFID
	wsUID        = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	wsProjectUID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	wsUser       = "alice"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

func newWorkspaceWriter(
	wsStore *mock.MockOrgWorkspaces,
	wpStore *mock.MockWorkspaceProjects,
	resolver *mock.MockProjectResolver,
) svc.WorkspaceWriter {
	return svc.NewWorkspaceWriter(
		svc.WithWorkspacesReader(wsStore),
		svc.WithWorkspacesWriter(wsStore),
		svc.WithWorkspaceProjectsReader(wpStore),
		svc.WithWorkspaceProjectsWriter(wpStore),
		svc.WithWorkspacesProjectResolver(resolver),
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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, wpStore, mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, wpStore, mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, wpStore, mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
		svc.WithWorkspacesProjectResolver(mock.NewMockProjectResolver()),
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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

	err := writer.DeleteWorkspace(context.Background(), svc.WorkspaceDelete{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

// ── AddProject ────────────────────────────────────────────────────────────────

func TestWorkspaceWriter_AddProject_HappyPath(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA", Slug: "test-proj", Name: "Test Project"})
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	result, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    wsProjectUID,
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Projects)
	assert.Len(t, result.Projects.Projects, 1)
	wp := result.Projects.Projects[0]
	assert.Equal(t, wsProjectUID, wp.ProjectUID)
	assert.Equal(t, "a2C000001AAA", wp.ProjectSFID, "write-time enrichment: SFID must be snapshotted")
	assert.Equal(t, "test-proj", wp.ProjectSlug, "write-time enrichment: Slug must be snapshotted")
	assert.Equal(t, "Test Project", wp.ProjectName, "write-time enrichment: Name must be snapshotted")
}

func TestWorkspaceWriter_AddProject_Idempotent_NoSpuriousRevisionBump(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA"})
	// Pre-seed the projects doc with the project already associated.
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID}},
	}, 3)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	result, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    wsProjectUID,
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	// Revision must not change — no write should have occurred.
	_, rev, _ := wpStore.GetWorkspaceProjects(context.Background(), wsUID)
	assert.EqualValues(t, 3, rev, "idempotent re-add must not bump revision")
}

func TestWorkspaceWriter_AddProject_UnknownProject_ReturnsValidation(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    "does-not-exist",
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsValidation(err), "unknown project should be Validation error, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_WorkspaceNotFound_ReturnsNotFound(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	resolver := mock.NewMockProjectResolver()
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID})
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), resolver)

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    wsProjectUID,
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID})
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    wsProjectUID,
		CreatedBy:    wsUser,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProject_NilResolver_ReturnsUnexpected(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	seedWorkspace(wsStore)
	// No resolver wired in.
	writer := svc.NewWorkspaceWriter(
		svc.WithWorkspacesReader(wsStore),
		svc.WithWorkspacesWriter(wsStore),
		svc.WithWorkspaceProjectsReader(mock.NewMockWorkspaceProjects()),
		svc.WithWorkspaceProjectsWriter(mock.NewMockWorkspaceProjects()),
	)

	_, err := writer.AddProject(context.Background(), svc.WorkspaceProjectAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectID:    wsProjectUID,
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	// Must not panic; error should not be Validation (no blame on the caller).
	assert.False(t, pkgerrors.IsValidation(err), "nil resolver should not produce Validation error")
}

// ── AddProjectsBulk ───────────────────────────────────────────────────────────

func TestWorkspaceWriter_AddProjectsBulk_PartialSuccess(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA", Name: "Good"})
	// "bad-id" is NOT seeded → will fail validation.
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectIDs:   []string{wsProjectUID, "bad-id"},
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Len(t, result.Succeeded, 1)
	assert.Equal(t, wsProjectUID, result.Succeeded[0].UID)
	assert.NotNil(t, result.Failed[1], "index 1 (bad-id) should have an error")
	assert.Nil(t, result.Failed[0], "index 0 (good) should have no error")
}

func TestWorkspaceWriter_AddProjectsBulk_AllAlreadyAssociated_NoRevisionBump(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA"})
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID}},
	}, 5)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectIDs:   []string{wsProjectUID},
		CreatedBy:    wsUser,
	})

	require.NoError(t, err)
	assert.Len(t, result.Succeeded, 1)
	// No new write — revision must not change.
	_, rev, _ := wpStore.GetWorkspaceProjects(context.Background(), wsUID)
	assert.EqualValues(t, 5, rev, "idempotent bulk re-add must not bump revision")
	// Result must carry the existing doc (nil would be returned if the empty-projects branch were hit).
	assert.NotNil(t, result.Projects)
}

func TestWorkspaceWriter_AddProjectsBulk_InfraError_FailsWholeRequest(t *testing.T) {
	// Infrastructure errors (not IsValidation) from the resolver must propagate and
	// abort the whole request — partial-success 200 would mask a dependency outage.
	wsStore := mock.NewMockOrgWorkspaces()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)

	infraErr := pkgerrors.NewUnexpected("NATS timeout")
	resolver.ResolveBatchFunc = func(_ context.Context, ids []string) ([]model.ProjectInfo, []error) {
		errs := make([]error, len(ids))
		infos := make([]model.ProjectInfo, len(ids))
		for i := range ids {
			errs[i] = infraErr
		}
		return infos, errs
	}
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), resolver)

	_, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectIDs:   []string{wsProjectUID},
		CreatedBy:    wsUser,
	})

	require.Error(t, err)
	assert.False(t, pkgerrors.IsValidation(err), "infra error must not be wrapped as Validation")
}

func TestWorkspaceWriter_AddProjectsBulk_StaleIfMatch_ReturnsPreconditionFailed(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA"})
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	_, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectIDs:   []string{wsProjectUID},
		CreatedBy:    wsUser,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}

func TestWorkspaceWriter_AddProjectsBulk_ValidIfMatch_Succeeds(t *testing.T) {
	wsStore := mock.NewMockOrgWorkspaces()
	wpStore := mock.NewMockWorkspaceProjects()
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	resolver.SeedProject(model.ProjectInfo{UID: wsProjectUID, SFID: "a2C000001AAA"})
	existingProjects := &model.WorkspaceProjects{WorkspaceUID: wsUID, OrgUID: wsOrgUID}
	wpStore.Seed(wsUID, existingProjects, 1)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

	result, err := writer.AddProjectsBulk(context.Background(), svc.WorkspaceProjectsBulkAdd{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectIDs:   []string{wsProjectUID},
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
	resolver := mock.NewMockProjectResolver()
	seedWorkspace(wsStore)
	wpStore.Seed(wsUID, &model.WorkspaceProjects{
		WorkspaceUID: wsUID,
		OrgUID:       wsOrgUID,
		Projects:     []model.WorkspaceProject{{ProjectUID: wsProjectUID}},
	}, 1)
	writer := newWorkspaceWriter(wsStore, wpStore, resolver)

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
	writer := newWorkspaceWriter(wsStore, wpStore, mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, mock.NewMockWorkspaceProjects(), mock.NewMockProjectResolver())

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
	writer := newWorkspaceWriter(wsStore, wpStore, mock.NewMockProjectResolver())

	_, err := writer.RemoveProject(context.Background(), svc.WorkspaceProjectRemove{
		OrgUID:       wsOrgUID,
		WorkspaceUID: wsUID,
		ProjectUID:   wsProjectUID,
		IfMatch:      "stale-etag",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got %T: %v", err, err)
}
