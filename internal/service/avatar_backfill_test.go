// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAvatarEnrichRunner wires a Runner for the b2b_org_settings avatar-enrichment path: the settings
// store doubles as reader + writer, plus the auth-service user reader and a b2b org reader for republish.
func newAvatarEnrichRunner(store *mock.MockB2BOrgSettings, ur avatarStubUserReader, org *model.B2BOrg) *svc.Runner {
	return svc.NewRunner(
		&mock.MockBackfillIterator{},
		&seedB2BOrgReader{org: org},
		mock.NewMockProjectMembershipReader(),
		nil,
		store,
		mock.NewMockMemberPublisher(),
		nil,
		"",
		nil,
		svc.WithSettingsWriter(store),
		svc.WithUserReader(ur),
	)
}

func TestAvatarBackfill_Runner_EnrichesAndIsIdempotent(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID:      testOrgUID,
		Writers:  []model.B2BOrgUser{{Username: "alice", Email: "a@x.com", InviteStatus: model.InviteStatusAccepted}},
		Auditors: []model.B2BOrgUser{{Username: "bob", Email: "b@x.com", InviteStatus: model.InviteStatusAccepted}},
	}, 1)

	runner := newAvatarEnrichRunner(store, avatarStubUserReader{picture: "https://example.com/p.png"}, &model.B2BOrg{UID: testOrgUID})

	require.NoError(t, runner.Run(context.Background(), svc.AvatarBackfillRequest("run-1", false, false, 0)))

	got, rev, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, "https://example.com/p.png", got.Writers[0].Avatar)
	assert.Equal(t, "https://example.com/p.png", got.Auditors[0].Avatar)
	assert.Equal(t, uint64(2), rev)

	// A second run finds no drift, so it must not write again (revision unchanged).
	require.NoError(t, runner.Run(context.Background(), svc.AvatarBackfillRequest("run-2", false, false, 0)))
	_, rev2, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, uint64(2), rev2, "idempotent re-run must not write again")
}

func TestAvatarBackfill_Runner_DryRun_DoesNotBumpRevision(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: []model.B2BOrgUser{{Username: "alice", InviteStatus: model.InviteStatusAccepted}},
	}, 1)

	runner := newAvatarEnrichRunner(store, avatarStubUserReader{picture: "https://example.com/p.png"}, &model.B2BOrg{UID: testOrgUID})

	require.NoError(t, runner.Run(context.Background(), svc.AvatarBackfillRequest("run-dry", true, false, 0)))
	_, rev, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, uint64(1), rev, "dry-run must not persist")
}

func TestAvatarBackfill_Runner_MissingOnly_SkipsPopulated(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Username: "alice", Avatar: "https://old/alice.png", InviteStatus: model.InviteStatusAccepted},
			{Username: "bob", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)

	runner := newAvatarEnrichRunner(store, avatarStubUserReader{picture: "https://new/p.png"}, &model.B2BOrg{UID: testOrgUID})

	require.NoError(t, runner.Run(context.Background(), svc.AvatarBackfillRequest("run-missing", false, true, 0)))

	got, _, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, "https://old/alice.png", got.Writers[0].Avatar, "populated avatar must be left untouched in missing-only mode")
	assert.Equal(t, "https://new/p.png", got.Writers[1].Avatar)
}
