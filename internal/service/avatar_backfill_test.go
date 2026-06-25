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

func TestAvatarBackfiller_BackfillsAndIsIdempotent(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID:      testOrgUID,
		Writers:  []model.B2BOrgUser{{Username: "alice", Email: "a@x.com", InviteStatus: model.InviteStatusAccepted}},
		Auditors: []model.B2BOrgUser{{Username: "bob", Email: "b@x.com", InviteStatus: model.InviteStatusAccepted}},
	}, 1)

	ur := avatarStubUserReader{picture: "https://example.com/p.png"}
	bf := svc.NewAvatarBackfiller(store, store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}, ur, mock.NewMockMemberPublisher())

	require.NoError(t, bf.Run(context.Background(), svc.AvatarBackfillOptions{}))

	got, rev, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, "https://example.com/p.png", got.Writers[0].Avatar)
	assert.Equal(t, "https://example.com/p.png", got.Auditors[0].Avatar)
	assert.Equal(t, uint64(2), rev)

	// A second run finds no drift, so it must not write again (revision unchanged).
	require.NoError(t, bf.Run(context.Background(), svc.AvatarBackfillOptions{}))
	_, rev2, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, uint64(2), rev2, "idempotent re-run must not write again")
}

func TestAvatarBackfiller_DryRun_DoesNotBumpRevision(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: []model.B2BOrgUser{{Username: "alice", InviteStatus: model.InviteStatusAccepted}},
	}, 1)

	ur := avatarStubUserReader{picture: "https://example.com/p.png"}
	bf := svc.NewAvatarBackfiller(store, store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}, ur, mock.NewMockMemberPublisher())

	require.NoError(t, bf.Run(context.Background(), svc.AvatarBackfillOptions{DryRun: true}))
	_, rev, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, uint64(1), rev, "dry-run must not persist")
}

func TestAvatarBackfiller_MissingOnly_SkipsPopulated(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Username: "alice", Avatar: "https://old/alice.png", InviteStatus: model.InviteStatusAccepted},
			{Username: "bob", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)

	ur := avatarStubUserReader{picture: "https://new/p.png"}
	bf := svc.NewAvatarBackfiller(store, store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}, ur, mock.NewMockMemberPublisher())

	require.NoError(t, bf.Run(context.Background(), svc.AvatarBackfillOptions{MissingOnly: true}))

	got, _, _ := store.GetSettings(context.Background(), testOrgUID)
	assert.Equal(t, "https://old/alice.png", got.Writers[0].Avatar, "populated avatar must be left untouched in missing-only mode")
	assert.Equal(t, "https://new/p.png", got.Writers[1].Avatar)
}
