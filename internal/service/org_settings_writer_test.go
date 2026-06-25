// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	inviteapi "github.com/linuxfoundation/lfx-v2-invite-service/pkg/api"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	svc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/etag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testOrgUID = "00000000-0000-0000-0000-000000000001"

func newOrgSettingsWriter(store *mock.MockB2BOrgSettings, orgReader *mock.MockB2BOrgReader, pub *mock.MockMemberPublisher) svc.OrgSettingsWriter {
	return svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(orgReader),
		svc.WithOrgSettingsPublisher(pub),
	)
}

// ── Update ──────────────────────────────────────────────────────────────────

func TestOrgSettingsWriter_Update_NilExisting_CreatesNewSettings(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // empty — no existing settings
	pub := mock.NewMockMemberPublisher()
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), pub)

	writers := []model.B2BOrgUser{{Email: "alice@example.com", Username: "alice"}}
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: writers}

	result, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Writers, 1)
	assert.Equal(t, "alice@example.com", result.Writers[0].Email)
}

func TestOrgSettingsWriter_Update_NilWriters_KeepsExisting(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: []model.B2BOrgUser{{Email: "bob@example.com"}},
	}
	store.Seed(testOrgUID, existing, 1)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	// nil Writers → keep bob
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: nil}
	result, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.Len(t, result.Writers, 1)
	assert.Equal(t, "bob@example.com", result.Writers[0].Email)
}

func TestOrgSettingsWriter_Update_EmptyWriters_ClearsAll(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: []model.B2BOrgUser{{Email: "bob@example.com"}},
	}
	store.Seed(testOrgUID, existing, 1)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	// empty (non-nil) slice → clear all
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{}}
	result, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	assert.Empty(t, result.Writers, "explicit empty slice should clear writers")
}

func TestOrgSettingsWriter_Update_PrincipalsCap(t *testing.T) {
	const cap = 700
	makeUsers := func(n int) []model.B2BOrgUser {
		users := make([]model.B2BOrgUser, n)
		for i := range users {
			users[i] = model.B2BOrgUser{Email: "u@example.com"}
		}
		return users
	}

	t.Run("exactly cap writers succeeds", func(t *testing.T) {
		writer := newOrgSettingsWriter(mock.NewMockB2BOrgSettings(), mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
		in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: makeUsers(cap)}
		_, err := writer.Update(context.Background(), in)
		require.NoError(t, err)
	})

	t.Run("cap+1 writers returns Validation error mentioning cap", func(t *testing.T) {
		writer := newOrgSettingsWriter(mock.NewMockB2BOrgSettings(), mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
		in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: makeUsers(cap + 1)}
		_, err := writer.Update(context.Background(), in)
		require.Error(t, err)
		var ve pkgerrors.Validation
		assert.True(t, errors.As(err, &ve), "expected Validation error, got %T: %v", err, err)
		assert.Contains(t, err.Error(), "700")
	})
}

func TestOrgSettingsWriter_Update_IfMatch_Matches_Succeeds(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{UID: testOrgUID, CreatedAt: time.Now().UTC()}
	store.Seed(testOrgUID, existing, 1)

	etagVal, err := etag.LFXEtag(existing)
	require.NoError(t, err)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, IfMatch: etagVal}
	_, err = writer.Update(context.Background(), in)

	assert.NoError(t, err)
}

func TestOrgSettingsWriter_Update_IfMatch_RoundTrip_Succeeds(t *testing.T) {
	// Seed a record, compute its ETag via GetSettings, then assert Update accepts it.
	// This locks in that the ETag produced on GET is the same value accepted on PUT.
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{
		UID:       testOrgUID,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Writers:   []model.B2BOrgUser{{Email: "alice@example.com"}},
	}
	store.Seed(testOrgUID, existing, 1)

	// Simulate GET: read settings and derive ETag exactly as the handler does.
	seeded, _, err := store.GetSettings(context.Background(), testOrgUID)
	require.NoError(t, err)
	etagVal, err := etag.LFXEtag(seeded)
	require.NoError(t, err)

	// PUT with that ETag must succeed.
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, IfMatch: etagVal}
	_, err = writer.Update(context.Background(), in)
	assert.NoError(t, err, "ETag from GET must be accepted by a subsequent PUT")
}

func TestOrgSettingsWriter_Update_IfMatch_NoExistingRecord_PreconditionFailed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // empty — no existing settings

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, IfMatch: "\"some-etag\""}
	_, err := writer.Update(context.Background(), in)

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "If-Match on non-existent record must return PreconditionFailed, got: %v", err)
}

func TestOrgSettingsWriter_Update_IfMatch_Mismatch_PreconditionFailed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{UID: testOrgUID}
	store.Seed(testOrgUID, existing, 1)

	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, IfMatch: "\"stale-etag\""}
	_, err := writer.Update(context.Background(), in)

	require.Error(t, err)
	assert.True(t, pkgerrors.IsPreconditionFailed(err), "expected PreconditionFailed, got: %v", err)
}

func TestOrgSettingsWriter_Update_OrgFetchFailure_FGASwallowed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // no existing settings
	// MockB2BOrgReader always returns not-found — FGA publish will be swallowed
	writer := newOrgSettingsWriter(store, mock.NewMockB2BOrgReader(), mock.NewMockMemberPublisher())

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{}}
	_, err := writer.Update(context.Background(), in)

	assert.NoError(t, err, "org fetch failure for FGA must not propagate")
}

func TestOrgSettingsWriter_Update_PublishFailure_Swallowed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	// Seed an org so the FGA reader succeeds, but the publisher errors
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(pkgerrors.NewUnexpected("nats down", nil))

	// Use a seeded org reader so org fetch succeeds and publish is actually attempted
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{}}
	_, err := writer.Update(context.Background(), in)

	assert.NoError(t, err, "FGA publish failure must not propagate")
}

func TestOrgSettingsWriter_Update_ClearWriters_FGADoesNotExcludeWriter(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{
		UID:     testOrgUID,
		Writers: []model.B2BOrgUser{{Email: "alice@example.com", Username: "alice", InviteStatus: model.InviteStatusAccepted}},
	}
	store.Seed(testOrgUID, existing, 1)

	pub := mock.NewMockMemberPublisher()
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	// explicit empty slice — intent: clear all writers
	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{}}
	_, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.NotNil(t, pub.LastAccessData, "FGA message must be published")

	// The FGA message must NOT exclude "writer" — an explicit empty list must let
	// the full-sync run so it revokes alice's existing tuple.
	fgaMsg, ok := pub.LastAccessData.(fgatypes.GenericFGAMessage)
	require.True(t, ok, "expected GenericFGAMessage, got %T", pub.LastAccessData)
	data, ok := fgaMsg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.NotContains(t, data.ExcludeRelations, "writer",
		"explicit empty writers must not be excluded from FGA sync — revocation requires full-sync")
}

// ── Indexer publish ────────────────────────────────────────────────────────

func TestOrgSettingsWriter_Update_PublishesIndexerAfterFGA(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	pub := mock.NewMockMemberPublisher()
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{
		{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
	}}
	_, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.Equal(t, []string{"access", "indexer"}, pub.CallOrder,
		"FGA (access) must be published before the indexer to ensure access tuples land first")
}

func TestOrgSettingsWriter_Update_IndexerSubjectIsSettingsSubject(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	pub := mock.NewMockMemberPublisher()
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{
		{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
	}}
	_, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	assert.Equal(t, "lfx.index.b2b_org_settings", pub.LastIndexSubject)
}

func TestOrgSettingsWriter_Update_IndexerPublishFailure_Swallowed(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	pub := mock.NewMockMemberPublisher()
	pub.SetIndexerError(pkgerrors.NewUnexpected("nats down", nil))
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{}}
	_, err := writer.Update(context.Background(), in)

	assert.NoError(t, err, "indexer publish failure must not propagate to caller")
}

func TestOrgSettingsWriter_Update_FirstWrite_EmitsActionCreated(t *testing.T) {
	store := mock.NewMockB2BOrgSettings() // empty — no existing settings
	pub := mock.NewMockMemberPublisher()
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{
		{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
	}}
	_, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.NotNil(t, pub.LastIndexerPayload, "indexer must have been called")
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok, "expected *model.MemberIndexerMessage, got %T", pub.LastIndexerPayload)
	assert.Equal(t, indexerConstants.ActionCreated, msg.Action,
		"first write (no prior KV record) must emit ActionCreated")
}

func TestOrgSettingsWriter_Update_SubsequentWrite_EmitsActionUpdated(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	existing := &model.B2BOrgSettings{UID: testOrgUID}
	store.Seed(testOrgUID, existing, 1)

	pub := mock.NewMockMemberPublisher()
	seededReader := &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(seededReader),
		svc.WithOrgSettingsPublisher(pub),
	)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{
		{Username: "alice", Email: "alice@acme.com", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
	}}
	_, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.NotNil(t, pub.LastIndexerPayload, "indexer must have been called")
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok, "expected *model.MemberIndexerMessage, got %T", pub.LastIndexerPayload)
	assert.Equal(t, indexerConstants.ActionUpdated, msg.Action,
		"write with existing KV record must emit ActionUpdated")
}

// ── Helpers ────────────────────────────────────────────────────────────────

// seedB2BOrgReader returns a fixed org for any UID.
// TestOrgSettingsWriter_AddPrincipal_OnlySyncsTouchedRelation verifies the per-principal FGA
// sync reconciles only the relation that changed: inviting an auditor must exclude "writer"
// from the full-sync (untouched, tuples preserved) while syncing "auditor".
func TestOrgSettingsWriter_AddPrincipal_OnlySyncsTouchedRelation(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{
			{Email: "alice@example.com", Username: "alice", InvitedAs: "writer", InviteStatus: model.InviteStatusAccepted},
		},
	}, 1)
	pub := mock.NewMockMemberPublisher()
	writer := svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}}),
		svc.WithOrgSettingsPublisher(pub),
	)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor",
	})
	require.NoError(t, err)
	require.NotNil(t, pub.LastAccessData, "FGA message must be published")
	fgaMsg, ok := pub.LastAccessData.(fgatypes.GenericFGAMessage)
	require.True(t, ok, "expected GenericFGAMessage, got %T", pub.LastAccessData)
	data, ok := fgaMsg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Contains(t, data.ExcludeRelations, "writer",
		"inviting an auditor must not re-sync the untouched writers relation")
	assert.NotContains(t, data.ExcludeRelations, "auditor",
		"the touched auditors relation must be synced so the new invite reconciles")
}

type seedB2BOrgReader struct{ org *model.B2BOrg }

func (r *seedB2BOrgReader) GetB2BOrg(_ context.Context, _ string) (*model.B2BOrg, error) {
	return r.org, nil
}

func (r *seedB2BOrgReader) FetchChildUIDsByParentUID(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func (r *seedB2BOrgReader) FetchChildUIDsByParentUIDs(_ context.Context, _ []string) (map[string][]string, error) {
	return map[string][]string{}, nil
}

// ── AddPrincipal (invite flow) ─────────────────────────────────────────────

// stubInviteSender is a controllable stub for port.InviteSender.
type stubInviteSender struct {
	result port.InviteResult
	err    error
	calls  []inviteapi.SendInviteRequest
}

func (s *stubInviteSender) SendInvite(_ context.Context, req inviteapi.SendInviteRequest) (port.InviteResult, error) {
	s.calls = append(s.calls, req)
	return s.result, s.err
}

// UserMetadataByPrincipal satisfies the extended port.UserReader for the func-based fake; no-op
// (returns empty metadata) so avatar enrichment is a no-op in tests that don't exercise it.
func (f userReaderFunc) UserMetadataByPrincipal(_ context.Context, _ string) (port.UserMetadata, error) {
	return port.UserMetadata{}, nil
}

// avatarStubUserReader implements port.UserReader returning a fixed username + picture, for the
// avatar-enrichment tests.
type avatarStubUserReader struct {
	username string
	picture  string
	metaErr  error
}

func (s avatarStubUserReader) UsernameByEmail(_ context.Context, _ string) (string, error) {
	if s.username == "" {
		return "", pkgerrors.NewNotFound("mock: no username")
	}
	return s.username, nil
}

func (s avatarStubUserReader) UserMetadataByPrincipal(_ context.Context, _ string) (port.UserMetadata, error) {
	if s.metaErr != nil {
		return port.UserMetadata{}, s.metaErr
	}
	return port.UserMetadata{Picture: s.picture}, nil
}

func TestOrgSettingsWriter_AddPrincipal_EnrichesAvatar(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := avatarStubUserReader{username: "bob", picture: "https://example.com/bob.png"}
	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, &stubInviteSender{}, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "writer",
	})

	require.NoError(t, err)
	require.Len(t, result.Writers, 1)
	assert.Equal(t, "https://example.com/bob.png", result.Writers[0].Avatar, "accepted principal's avatar must be enriched from auth-service")
}

func TestOrgSettingsWriter_AddPrincipal_AvatarEnrichFailSoft(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	// Metadata lookup fails: the username still resolves and the add succeeds with an empty avatar.
	userReader := avatarStubUserReader{username: "bob", metaErr: errors.New("nats: metadata timeout")}
	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, &stubInviteSender{}, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "writer",
	})

	require.NoError(t, err, "a metadata lookup failure must not block the add")
	require.Len(t, result.Writers, 1)
	assert.Equal(t, "bob", result.Writers[0].Username)
	assert.Empty(t, result.Writers[0].Avatar)
}

func TestOrgSettingsWriter_Update_EnrichesOnlyMissingAvatars(t *testing.T) {
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := avatarStubUserReader{username: "x", picture: "https://example.com/new.png"}
	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, &stubInviteSender{}, nil)

	in := svc.B2BOrgSettingsUpdate{OrgUID: testOrgUID, Writers: []model.B2BOrgUser{
		{Email: "alice@example.com", Username: "alice", InviteStatus: model.InviteStatusAccepted},
		{Email: "bob@example.com", Username: "bob", Avatar: "https://example.com/keep.png", InviteStatus: model.InviteStatusAccepted},
	}}

	result, err := writer.Update(context.Background(), in)

	require.NoError(t, err)
	require.Len(t, result.Writers, 2)
	assert.Equal(t, "https://example.com/new.png", result.Writers[0].Avatar, "missing avatar must be enriched on write")
	assert.Equal(t, "https://example.com/keep.png", result.Writers[1].Avatar, "existing avatar must be left untouched (refresh is the backfill's job)")
}

func TestOrgSettingsWriter_AddPrincipal_LFIDFound_AcceptsImmediately(t *testing.T) {
	// When UsernameByEmail returns a username, the entry should be InviteStatusAccepted with no invite sent.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	sender := &stubInviteSender{result: port.InviteResult{InviteUID: "unused"}}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "bob", nil
	})

	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, sender, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: "writer",
	})

	require.NoError(t, err)
	require.Len(t, result.Writers, 1)
	w := result.Writers[0]
	assert.Equal(t, "bob", w.Username, "LFID username must be stamped immediately")
	assert.Equal(t, model.InviteStatusAccepted, w.InviteStatus)
	assert.NotNil(t, w.AcceptedAt)
	assert.Empty(t, w.InviteUUID, "no invite UUID when LFID found")
	assert.Empty(t, sender.calls, "invite service must not be called when LFID is found")
}

func TestOrgSettingsWriter_AddPrincipal_NoLFID_SendsInvite(t *testing.T) {
	// When UsernameByEmail returns empty, invite service is called and entry is pending.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	sender := &stubInviteSender{result: port.InviteResult{InviteUID: "invite-uuid-123"}}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil
	})

	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, sender, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: "auditor",
	})

	require.NoError(t, err)
	require.Len(t, result.Auditors, 1)
	a := result.Auditors[0]
	assert.Equal(t, model.InviteStatusPending, a.InviteStatus)
	assert.Empty(t, a.Username, "no username until invite accepted")
	assert.Equal(t, "invite-uuid-123", a.InviteUUID, "InviteUUID must be set from invite service response")
	require.Len(t, sender.calls, 1, "invite service must be called once")
	assert.Equal(t, "carol@example.com", sender.calls[0].Recipient.Email)
}

func TestOrgSettingsWriter_AddPrincipal_PendingResendInPlace(t *testing.T) {
	// An existing pending entry for the same email and same role must be refreshed in-place
	// (InviteUUID updated, CreatedAt preserved) rather than returning Conflict.
	now := time.Now().UTC().Add(-time.Hour)
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "dave@example.com",
			InvitedAs:    "writer",
			InviteStatus: model.InviteStatusPending,
			InviteUUID:   "old-uuid",
			CreatedAt:    now,
			UpdatedAt:    now,
		}},
	}, 1)

	sender := &stubInviteSender{result: port.InviteResult{InviteUID: "new-uuid"}}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil })

	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, sender, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "dave@example.com", InvitedAs: "writer",
	})

	require.NoError(t, err)
	require.Len(t, result.Writers, 1, "must not create a duplicate entry")
	w := result.Writers[0]
	assert.Equal(t, "new-uuid", w.InviteUUID, "InviteUUID must be refreshed")
	assert.Equal(t, now.UTC().Truncate(time.Second), w.CreatedAt.UTC().Truncate(time.Second),
		"CreatedAt must be preserved from the original entry")
	require.Len(t, sender.calls, 1, "invite must be re-sent")
}

func TestOrgSettingsWriter_AddPrincipal_PendingDifferentRole_ReturnsConflict(t *testing.T) {
	// An existing pending entry for the same email but a different role must return Conflict.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{
		UID: testOrgUID,
		Writers: []model.B2BOrgUser{{
			Email:        "eve@example.com",
			InvitedAs:    "writer",
			InviteStatus: model.InviteStatusPending,
		}},
	}, 1)

	sender := &stubInviteSender{}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil })

	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, sender, nil)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "eve@example.com", InvitedAs: "auditor",
	})

	require.Error(t, err)
	assert.True(t, pkgerrors.IsConflict(err), "pending entry with different role must return Conflict, got: %v", err)
	assert.Empty(t, sender.calls, "invite service must not be called on conflict")
}

func TestOrgSettingsWriter_AddPrincipal_InviteSendFails_EntryStillPersisted(t *testing.T) {
	// Invite send failure must not block the entry from being persisted (best-effort).
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	sender := &stubInviteSender{err: errors.New("invite service unavailable")}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil })

	writer := newOrgSettingsWriterWithNotifier(store, &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(), userReader, sender, nil)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "frank@example.com", InvitedAs: "writer",
	})

	require.NoError(t, err, "invite send failure must not propagate to caller")
	require.Len(t, result.Writers, 1, "entry must still be persisted")
	assert.Equal(t, model.InviteStatusPending, result.Writers[0].InviteStatus)
	assert.Empty(t, result.Writers[0].InviteUUID, "InviteUUID is empty when send failed")
}

// ── AddPrincipal (role-assignment notification) ────────────────────────────

// capturingRoleNotifier records all NotifyRoleAssigned calls for assertions.
type capturingRoleNotifier struct {
	calls []port.OrgRoleAssignedNotification
	err   error
}

func (n *capturingRoleNotifier) NotifyRoleAssigned(_ context.Context, notif port.OrgRoleAssignedNotification) error {
	n.calls = append(n.calls, notif)
	return n.err
}

func newOrgSettingsWriterWithNotifier(
	store *mock.MockB2BOrgSettings,
	orgReader port.B2BOrgReader,
	pub *mock.MockMemberPublisher,
	userReader port.UserReader,
	inviteSender port.InviteSender,
	notifier port.OrgRoleNotifier,
) svc.OrgSettingsWriter {
	return svc.NewOrgSettingsWriter(
		svc.WithOrgSettingsReader(store),
		svc.WithOrgSettingsWriter(store),
		svc.WithOrgSettingsB2BOrgReader(orgReader),
		svc.WithOrgSettingsPublisher(pub),
		svc.WithOrgSettingsUserReader(userReader),
		svc.WithOrgSettingsInviteSender(inviteSender),
		svc.WithOrgSettingsRoleNotifier(notifier),
	)
}

func TestOrgSettingsWriter_AddPrincipal_LFIDFound_NotifiesRoleAssigned(t *testing.T) {
	// On the existing-LFID path, NotifyRoleAssigned must be called once with the
	// correct email, orgName, and role after a successful write.
	store := mock.NewMockB2BOrgSettings()
	const orgName = "Acme Corp"
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "bob", nil
	})
	notifier := &capturingRoleNotifier{}

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID, Name: orgName}},
		mock.NewMockMemberPublisher(),
		userReader,
		&stubInviteSender{},
		notifier,
	)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: model.B2BOrgRoleWriter,
	})

	require.NoError(t, err)
	require.Len(t, notifier.calls, 1, "NotifyRoleAssigned must be called exactly once")
	got := notifier.calls[0]
	assert.Equal(t, "bob@example.com", got.RecipientEmail)
	assert.Equal(t, orgName, got.OrgName)
	assert.Equal(t, model.B2BOrgRoleWriter, got.Role)
}

func TestOrgSettingsWriter_AddPrincipal_NoLFID_DoesNotNotify(t *testing.T) {
	// On the no-LFID path, NotifyRoleAssigned must NOT be called (invite is sent instead).
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "", nil // no LFID
	})
	notifier := &capturingRoleNotifier{}

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(),
		userReader,
		&stubInviteSender{result: port.InviteResult{InviteUID: "inv-1"}},
		notifier,
	)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "carol@example.com", InvitedAs: model.B2BOrgRoleAuditor,
	})

	require.NoError(t, err)
	assert.Empty(t, notifier.calls, "NotifyRoleAssigned must not be called when no LFID")
}

func TestOrgSettingsWriter_AddPrincipal_NotifyFails_WriteStillSucceeds(t *testing.T) {
	// A notification failure must not propagate to the caller — email is best-effort.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "dan", nil
	})
	notifier := &capturingRoleNotifier{err: errors.New("email service down")}

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(),
		userReader,
		&stubInviteSender{},
		notifier,
	)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "dan@example.com", InvitedAs: model.B2BOrgRoleWriter,
	})

	require.NoError(t, err, "notification failure must not propagate to caller")
	require.Len(t, result.Writers, 1, "entry must still be persisted")
	assert.Equal(t, model.InviteStatusAccepted, result.Writers[0].InviteStatus)
}

// countingOrgReader wraps seedB2BOrgReader and records how many times GetB2BOrg
// was called. Used to verify that AddPrincipal does not issue redundant fetches.
type countingOrgReader struct {
	inner *seedB2BOrgReader
	calls int
}

func (r *countingOrgReader) GetB2BOrg(ctx context.Context, uid string) (*model.B2BOrg, error) {
	r.calls++
	return r.inner.GetB2BOrg(ctx, uid)
}

func (r *countingOrgReader) FetchChildUIDsByParentUID(ctx context.Context, uid string) ([]string, error) {
	return r.inner.FetchChildUIDsByParentUID(ctx, uid)
}

func (r *countingOrgReader) FetchChildUIDsByParentUIDs(ctx context.Context, uids []string) (map[string][]string, error) {
	return r.inner.FetchChildUIDsByParentUIDs(ctx, uids)
}

// ── AddPrincipal (SuppressNotification) ──────────────────────────────────────

func TestOrgSettingsWriter_AddPrincipal_SuppressNotification_RegisteredNoEmail(t *testing.T) {
	// SuppressNotification=true on the registered-LFID path: entry must be accepted
	// with Username set, but NotifyRoleAssigned must NOT be called.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "auth0|bob", nil
	})
	notifier := &capturingRoleNotifier{}

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID, Name: "Acme"}},
		mock.NewMockMemberPublisher(),
		userReader,
		&stubInviteSender{},
		notifier,
	)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "bob@example.com", InvitedAs: model.B2BOrgRoleWriter,
		SuppressNotification: true,
	})

	require.NoError(t, err)
	require.Len(t, result.Writers, 1)
	w := result.Writers[0]
	assert.Equal(t, "auth0|bob", w.Username, "LFID must still be stamped when suppressed")
	assert.Equal(t, model.InviteStatusAccepted, w.InviteStatus)
	assert.Empty(t, notifier.calls, "NotifyRoleAssigned must NOT be called when SuppressNotification=true")
}

func TestOrgSettingsWriter_AddPrincipal_SuppressNotification_False_Notifies(t *testing.T) {
	// SuppressNotification=false (default) on the registered-LFID path: NotifyRoleAssigned
	// MUST be called, preserving the existing behavior.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "auth0|alice", nil
	})
	notifier := &capturingRoleNotifier{}

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID, Name: "Acme"}},
		mock.NewMockMemberPublisher(),
		userReader,
		&stubInviteSender{},
		notifier,
	)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "alice@example.com", InvitedAs: model.B2BOrgRoleAuditor,
		SuppressNotification: false,
	})

	require.NoError(t, err)
	require.Len(t, notifier.calls, 1, "NotifyRoleAssigned must be called when SuppressNotification=false")
}

func TestOrgSettingsWriter_AddPrincipal_SuppressNotification_UnregisteredNoInvite(t *testing.T) {
	// SuppressNotification=true on the no-LFID path: entry is persisted as pending but
	// the invite service must NOT be called, and InviteUUID must be empty.
	store := mock.NewMockB2BOrgSettings()
	store.Seed(testOrgUID, &model.B2BOrgSettings{UID: testOrgUID}, 1)

	sender := &stubInviteSender{result: port.InviteResult{InviteUID: "should-not-be-set"}}
	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) { return "", nil })

	writer := newOrgSettingsWriterWithNotifier(
		store,
		&seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID}},
		mock.NewMockMemberPublisher(),
		userReader,
		sender,
		nil,
	)

	result, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "unregistered@example.com", InvitedAs: model.B2BOrgRoleAuditor,
		SuppressNotification: true,
	})

	require.NoError(t, err)
	require.Len(t, result.Auditors, 1)
	a := result.Auditors[0]
	assert.Equal(t, model.InviteStatusPending, a.InviteStatus)
	assert.Empty(t, a.InviteUUID, "InviteUUID must be empty when SuppressNotification=true and no LFID")
	assert.Empty(t, sender.calls, "invite service must NOT be called when SuppressNotification=true")
}

func TestOrgSettingsWriter_AddPrincipal_FirstPrincipal_SingleOrgFetch(t *testing.T) {
	// On the first-principal + existing-LFID path (existing == nil), GetB2BOrg must
	// be called exactly twice: once for the existence guard, once by publishAll.
	// Before this optimisation it was three calls (guard + publishAll + fetchOrgName
	// for the notification). The guard result is now passed directly to notifyRoleAssigned
	// so fetchOrgName is not invoked a third time.
	store := mock.NewMockB2BOrgSettings() // no seed → existing == nil
	const orgName = "First Org"

	userReader := userReaderFunc(func(_ context.Context, _ string) (string, error) {
		return "eve", nil
	})
	notifier := &capturingRoleNotifier{}
	orgReader := &countingOrgReader{inner: &seedB2BOrgReader{org: &model.B2BOrg{UID: testOrgUID, Name: orgName}}}

	writer := newOrgSettingsWriterWithNotifier(
		store, orgReader, mock.NewMockMemberPublisher(),
		userReader, &stubInviteSender{}, notifier,
	)

	_, err := writer.AddPrincipal(context.Background(), svc.B2BOrgSettingsAddPrincipal{
		OrgUID: testOrgUID, Email: "eve@example.com", InvitedAs: model.B2BOrgRoleWriter,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, orgReader.calls, "GetB2BOrg must be called exactly twice (guard + publishAll); notify reuses the guard result")
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, orgName, notifier.calls[0].OrgName, "org name must come from the guard fetch, not a third read")
}
