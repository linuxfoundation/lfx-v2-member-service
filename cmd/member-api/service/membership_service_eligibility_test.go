// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
)

const eligibilityTestOrgUID = "001000000000001AAA"

// errWriterReached stands in for any writer outcome: the tests only ask
// whether the handler got past the eligibility check.
var errWriterReached = errors.New("writer reached")

type stubEligibility struct {
	eligible bool
	absent   bool
	err      error
	calls    int
}

func (s *stubEligibility) FetchB2BOrgEligibility(_ context.Context, uids []string) (map[string]bool, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]bool, len(uids))
	if !s.absent {
		for _, uid := range uids {
			out[uid] = s.eligible
		}
	}
	return out, nil
}

type countingB2BOrgWriter struct {
	usecaseSvc.B2BOrgWriter
	calls *int
}

func (w countingB2BOrgWriter) Update(context.Context, string, model.B2BOrgInput, string) (*model.B2BOrg, error) {
	*w.calls++
	return nil, errWriterReached
}

type countingLogoUploader struct{ calls *int }

func (u countingLogoUploader) UploadB2BOrgLogo(context.Context, string, string, io.Reader, string) (*model.B2BOrg, error) {
	*u.calls++
	return nil, errWriterReached
}

type countingSettingsWriter struct{ calls *int }

func (w countingSettingsWriter) Update(context.Context, usecaseSvc.B2BOrgSettingsUpdate) (*model.B2BOrgSettings, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingSettingsWriter) AddPrincipal(context.Context, usecaseSvc.B2BOrgSettingsAddPrincipal) (*model.B2BOrgSettings, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingSettingsWriter) ChangePrincipalRole(context.Context, usecaseSvc.B2BOrgSettingsChangeRole) (*model.B2BOrgSettings, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingSettingsWriter) RemovePrincipal(context.Context, usecaseSvc.B2BOrgSettingsRemovePrincipal) (*model.B2BOrgSettings, error) {
	*w.calls++
	return nil, errWriterReached
}

type countingWorkspaceWriter struct{ calls *int }

func (w countingWorkspaceWriter) CreateWorkspace(context.Context, usecaseSvc.WorkspaceCreate) (*usecaseSvc.WorkspaceMetaResult, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingWorkspaceWriter) UpdateWorkspace(context.Context, usecaseSvc.WorkspaceUpdate) (*usecaseSvc.WorkspaceMetaResult, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingWorkspaceWriter) DeleteWorkspace(context.Context, usecaseSvc.WorkspaceDelete) error {
	*w.calls++
	return errWriterReached
}
func (w countingWorkspaceWriter) AddProject(context.Context, usecaseSvc.WorkspaceProjectAdd) (*usecaseSvc.WorkspaceProjectResult, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingWorkspaceWriter) AddProjectsBulk(context.Context, usecaseSvc.WorkspaceProjectsBulkAdd) (*usecaseSvc.WorkspaceBulkResult, error) {
	*w.calls++
	return nil, errWriterReached
}
func (w countingWorkspaceWriter) RemoveProject(context.Context, usecaseSvc.WorkspaceProjectRemove) (*usecaseSvc.WorkspaceProjectResult, error) {
	*w.calls++
	return nil, errWriterReached
}

type handlerCall func(ctx context.Context, svc membershipservice.Service) error

func newEligibilityTestSvc(reader *stubEligibility, writerCalls *int) membershipservice.Service {
	return newTestSvc(
		withB2BOrgEligibility(reader),
		withB2BOrgWriterUC(countingB2BOrgWriter{calls: writerCalls}),
		withLogoUploaderUC(countingLogoUploader{calls: writerCalls}),
		withSettingsWriterUC(countingSettingsWriter{calls: writerCalls}),
		withWorkspaceWriterUC(countingWorkspaceWriter{calls: writerCalls}),
	)
}

func guardedWriteCalls() map[string]handlerCall {
	uid := eligibilityTestOrgUID
	return map[string]handlerCall{
		"update org": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.UpdateB2bOrg(ctx, &membershipservice.UpdateB2bOrgPayload{UID: uid})
			return err
		},
		"upload logo": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.UploadB2bOrgLogo(ctx, &membershipservice.UploadB2bOrgLogoPayload{UID: uid, ContentType: "image/png"},
				io.NopCloser(strings.NewReader("png")))
			return err
		},
		"update settings": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.UpdateB2bOrgSettings(ctx, &membershipservice.UpdateB2bOrgSettingsPayload{UID: uid})
			return err
		},
		"add settings user": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.AddB2bOrgSettingsUser(ctx, &membershipservice.AddB2bOrgSettingsUserPayload{UID: uid, Email: "a@example.com", InvitedAs: "writer"})
			return err
		},
		"change settings user role": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.UpdateB2bOrgSettingsUserRole(ctx, &membershipservice.UpdateB2bOrgSettingsUserRolePayload{UID: uid, Email: "a@example.com", InvitedAs: "auditor"})
			return err
		},
		"create workspace": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.CreateB2bOrgWorkspace(ctx, &membershipservice.CreateB2bOrgWorkspacePayload{UID: uid, Name: "w"})
			return err
		},
		"update workspace": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.UpdateB2bOrgWorkspace(ctx, &membershipservice.UpdateB2bOrgWorkspacePayload{UID: uid, WorkspaceUID: "ws", Name: "w"})
			return err
		},
		"add workspace project": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.AddB2bOrgWorkspaceProject(ctx, &membershipservice.AddB2bOrgWorkspaceProjectPayload{UID: uid, WorkspaceUID: "ws", ProjectSlug: "p"})
			return err
		},
		"bulk add workspace projects": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.BulkAddB2bOrgWorkspaceProjects(ctx, &membershipservice.BulkAddB2bOrgWorkspaceProjectsPayload{UID: uid, WorkspaceUID: "ws"})
			return err
		},
	}
}

func requireServiceErrorName(t *testing.T, err error, name string) {
	t.Helper()
	var serviceErr *goa.ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected *goa.ServiceError, got %T: %v", err, err)
	assert.Equal(t, name, serviceErr.Name)
}

func TestGuardedWrites_RefuseAccountThatIsNoOrganization(t *testing.T) {
	for name, call := range guardedWriteCalls() {
		for _, reader := range []struct {
			label string
			stub  *stubEligibility
		}{
			{"not an organization", &stubEligibility{eligible: false}},
			{"deleted or unknown", &stubEligibility{absent: true}},
		} {
			t.Run(name+"/"+reader.label, func(t *testing.T) {
				writerCalls := 0
				err := call(context.Background(), newEligibilityTestSvc(reader.stub, &writerCalls))

				requireServiceErrorName(t, err, "NotFound")
				assert.Equal(t, 1, reader.stub.calls)
				assert.Zero(t, writerCalls, "a refused write must not reach the writer")
			})
		}
	}
}

func TestGuardedWrites_EligibleAccountReachesWriter(t *testing.T) {
	for name, call := range guardedWriteCalls() {
		t.Run(name, func(t *testing.T) {
			reader := &stubEligibility{eligible: true}
			writerCalls := 0
			_ = call(context.Background(), newEligibilityTestSvc(reader, &writerCalls))

			assert.Equal(t, 1, reader.calls)
			assert.Equal(t, 1, writerCalls)
		})
	}
}

func TestGuardedWrites_EligibilityReadErrorIsNotNotFound(t *testing.T) {
	for name, call := range guardedWriteCalls() {
		t.Run(name, func(t *testing.T) {
			writerCalls := 0
			err := call(context.Background(), newEligibilityTestSvc(&stubEligibility{err: errors.New("salesforce unavailable")}, &writerCalls))

			requireServiceErrorName(t, err, "InternalServerError")
			assert.Zero(t, writerCalls)
		})
	}
}

func TestRemovals_AllowedForAccountThatIsNoOrganization(t *testing.T) {
	uid := eligibilityTestOrgUID
	removals := map[string]handlerCall{
		"remove settings user": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.DeleteB2bOrgSettingsUser(ctx, &membershipservice.DeleteB2bOrgSettingsUserPayload{UID: uid, Email: "a@example.com"})
			return err
		},
		"delete workspace": func(ctx context.Context, svc membershipservice.Service) error {
			return svc.DeleteB2bOrgWorkspace(ctx, &membershipservice.DeleteB2bOrgWorkspacePayload{UID: uid, WorkspaceUID: "ws"})
		},
		"remove workspace project": func(ctx context.Context, svc membershipservice.Service) error {
			_, err := svc.RemoveB2bOrgWorkspaceProject(ctx, &membershipservice.RemoveB2bOrgWorkspaceProjectPayload{UID: uid, WorkspaceUID: "ws", ProjectUID: "p"})
			return err
		},
	}
	for name, call := range removals {
		t.Run(name, func(t *testing.T) {
			reader := &stubEligibility{eligible: false}
			writerCalls := 0
			_ = call(context.Background(), newEligibilityTestSvc(reader, &writerCalls))

			assert.Zero(t, reader.calls, "removals must not spend a Salesforce read")
			assert.Equal(t, 1, writerCalls)
		})
	}
}
