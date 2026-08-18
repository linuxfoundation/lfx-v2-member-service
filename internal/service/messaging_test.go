// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	fgaconstants "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/constants"
	fgatypes "github.com/linuxfoundation/lfx-v2-fga-sync/pkg/types"
	indexerConstants "github.com/linuxfoundation/lfx-v2-indexer-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testB2BOrg is the canonical B2BOrg fixture used in events golden tests.
var testB2BOrg = &model.B2BOrg{
	UID:           "b2b-org-uid-001",
	SFID:          "001000000000001AAA",
	Name:          "Linux Foundation",
	PrimaryDomain: "linuxfoundation.org",
	Description:   "Supporting open source ecosystems.",
	Industry:      "Technology",
	Sector:        "Non-Profit",
	DomainAliases: []string{"lf.org", "thelinuxfoundation.org"},
}

func TestBuildB2BOrgIndexingConfig(t *testing.T) {
	cfg := BuildB2BOrgIndexingConfig(testB2BOrg)

	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Public, "Public must be set explicitly — nil defaults indexer to public")
	assert.False(t, *cfg.Public, "b2b_org must not be publicly accessible")
	assert.Equal(t, "b2b-org-uid-001", cfg.ObjectID)
	assert.Equal(t, "b2b_org:b2b-org-uid-001", cfg.AccessCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.AccessCheckRelation)
	assert.Equal(t, "b2b_org:b2b-org-uid-001", cfg.HistoryCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.HistoryCheckRelation)
	assert.Equal(t, "linux foundation", cfg.SortName)
	assert.Equal(t,
		[]string{"Linux Foundation", "linuxfoundation.org", "lf.org", "thelinuxfoundation.org"},
		cfg.NameAndAliases,
	)
	assert.Contains(t, cfg.Fulltext, "Linux Foundation")
	assert.Contains(t, cfg.Fulltext, "linuxfoundation.org")
	assert.Contains(t, cfg.Fulltext, "Supporting open source ecosystems.")
	assert.Contains(t, cfg.Fulltext, "Technology")
	assert.Contains(t, cfg.Fulltext, "Non-Profit")
	assert.Equal(t, testB2BOrg.Tags(), cfg.Tags)
}

func TestBuildB2BOrgIndexingConfig_EmptyOptionals(t *testing.T) {
	sparse := &model.B2BOrg{UID: "uid-sparse", Name: "Sparse Org"}
	cfg := BuildB2BOrgIndexingConfig(sparse)

	assert.Equal(t, []string{"Sparse Org"}, cfg.NameAndAliases)
	assert.Equal(t, "Sparse Org", cfg.Fulltext)
	assert.Empty(t, cfg.ParentRefs)
}

func TestBuildB2BOrgIndexingConfig_WithParent(t *testing.T) {
	org := &model.B2BOrg{UID: "child-org-uid", Name: "Child Org", ParentUID: "parent-org-uid"}
	cfg := BuildB2BOrgIndexingConfig(org)

	require.NotNil(t, cfg)
	assert.Equal(t, []string{"b2b_org:parent-org-uid"}, cfg.ParentRefs)
}

func TestBuildProjectMembershipIndexingConfig(t *testing.T) {
	pm := &model.ProjectMembership{
		UID:           "pm-uid-001",
		B2BOrgUID:     "b2b-org-uid-001",
		ProjectUID:    "project-uid-001",
		CompanyName:   "Acme Corp",
		CompanyDomain: "acme.com",
		TierName:      "Gold Corporate Membership",
		Status:        "Active",
		Year:          "2025",
	}
	cfg := BuildProjectMembershipIndexingConfig(pm)

	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Public)
	assert.False(t, *cfg.Public)
	assert.Equal(t, "pm-uid-001", cfg.ObjectID)
	assert.Equal(t, "project_membership:pm-uid-001", cfg.AccessCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.AccessCheckRelation)
	assert.Equal(t, "acme corp", cfg.SortName)
	assert.Equal(t, []string{"Acme Corp", "acme.com"}, cfg.NameAndAliases)
	assert.Equal(t,
		[]string{"b2b_org:b2b-org-uid-001", "project:project-uid-001"},
		cfg.ParentRefs,
	)
	assert.Contains(t, cfg.Fulltext, "Acme Corp")
	assert.Contains(t, cfg.Fulltext, "Gold Corporate Membership")
	assert.Contains(t, cfg.Fulltext, "Active")
	assert.Contains(t, cfg.Fulltext, "2025")
	assert.Equal(t, pm.Tags(), cfg.Tags)
}

func TestBuildKeyContactIndexingConfig(t *testing.T) {
	kc := &model.KeyContact{
		UID:            "kc-uid-001",
		B2BOrgUID:      "b2b-org-uid-001",
		ProjectUID:     "project-uid-001",
		MembershipUID:  "pm-uid-001",
		FirstName:      "Ada",
		LastName:       "Lovelace",
		Email:          "ada@example.com",
		Emails:         []string{"ada@example.com", "alovelace@example.com"},
		Role:           "Voting Representative",
		CompanyName:    "Acme Corp",
		ProjectName:    "Kubernetes",
		ProjectLogoURL: "https://artwork.cncf.io/projects/kubernetes/icon/color/kubernetes-icon-color.svg",
	}
	cfg := BuildKeyContactIndexingConfig(kc)

	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Public)
	assert.False(t, *cfg.Public)
	assert.Equal(t, "kc-uid-001", cfg.ObjectID)
	assert.Equal(t, "project_membership:pm-uid-001", cfg.AccessCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.AccessCheckRelation)
	assert.Equal(t, "project_membership:pm-uid-001", cfg.HistoryCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.HistoryCheckRelation)
	assert.Equal(t, "lovelace ada", cfg.SortName)
	assert.Equal(t, []string{"Ada Lovelace", "ada@example.com"}, cfg.NameAndAliases)
	assert.Equal(t,
		[]string{"b2b_org:b2b-org-uid-001", "project:project-uid-001", "project_membership:pm-uid-001"},
		cfg.ParentRefs,
	)
	assert.Contains(t, cfg.Fulltext, "Ada")
	assert.Contains(t, cfg.Fulltext, "Lovelace")
	assert.Contains(t, cfg.Fulltext, "ada@example.com")
	assert.Contains(t, cfg.Fulltext, "Voting Representative")
	assert.Contains(t, cfg.Fulltext, "Acme Corp")
	assert.Contains(t, cfg.Fulltext, "Kubernetes")
	assert.Equal(t, kc.Tags(), cfg.Tags)
	require.Len(t, cfg.Contacts, 1)
	assert.Equal(t, "kc-uid-001", cfg.Contacts[0].LfxPrincipal)
	assert.Equal(t, kc.Name(), cfg.Contacts[0].Name)
	assert.Equal(t, []string{"ada@example.com", "alovelace@example.com"}, cfg.Contacts[0].Emails)
}

func TestBuildKeyContactIndexingConfig_ProjectNameInFulltext_ProjectLogoNotInFulltext(t *testing.T) {
	// project_name is searchable via fulltext; project_logo_url is data-only.
	kc := &model.KeyContact{
		UID:            "kc-uid-logo-001",
		MembershipUID:  "pm-uid-001",
		FirstName:      "Grace",
		LastName:       "Hopper",
		ProjectName:    "OpenTelemetry",
		ProjectLogoURL: "https://artwork.cncf.io/projects/opentelemetry/icon/color/opentelemetry-icon-color.svg",
	}
	cfg := BuildKeyContactIndexingConfig(kc)

	assert.Contains(t, cfg.Fulltext, "OpenTelemetry")
	assert.NotContains(t, cfg.Fulltext, "artwork.cncf.io")
}

func TestBuildKeyContactFGAPutMessage(t *testing.T) {
	msg := BuildKeyContactFGAPutMessage("pm-uid-001", "user-sub-abc")

	assert.Equal(t, "project_membership", msg.ObjectType)
	assert.Equal(t, "member_put", msg.Operation)
	data, ok := msg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, "pm-uid-001", data.UID)
	assert.Equal(t, "user-sub-abc", data.Username)
	assert.Equal(t, []string{"key_contact"}, data.Relations)
}

func TestBuildKeyContactFGARemoveMessage(t *testing.T) {
	msg := BuildKeyContactFGARemoveMessage("pm-uid-001", "user-sub-abc")

	assert.Equal(t, "project_membership", msg.ObjectType)
	assert.Equal(t, "member_remove", msg.Operation)
	data, ok := msg.Data.(fgatypes.GenericMemberData)
	require.True(t, ok)
	assert.Equal(t, "pm-uid-001", data.UID)
	assert.Equal(t, "user-sub-abc", data.Username)
	assert.Equal(t, []string{"key_contact"}, data.Relations)
}

func TestBuildB2BOrgReparentingMessages_SetParent(t *testing.T) {
	updated := &model.B2BOrg{UID: "child-uid", ParentUID: "parent-uid"}
	newChildren := []string{"sibling-uid", "child-uid"}
	msgs := BuildB2BOrgReparentingMessages(nil, updated, nil, newChildren)

	require.Len(t, msgs, 2)

	data0, ok := msgs[0].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "child-uid", data0.UID)
	assert.Equal(t, []string{"b2b_org:parent-uid"}, data0.References["parent"])
	assert.Equal(t, b2bOrgNonParentRelations, data0.ExcludeRelations)

	data1, ok := msgs[1].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "parent-uid", data1.UID)
	assert.Equal(t, []string{"b2b_org:sibling-uid", "b2b_org:child-uid"}, data1.References["child"])
	assert.Equal(t, b2bOrgNonChildRelations, data1.ExcludeRelations)
}

func TestBuildB2BOrgReparentingMessages_Reparent(t *testing.T) {
	current := &model.B2BOrg{UID: "child-uid", ParentUID: "old-parent-uid"}
	updated := &model.B2BOrg{UID: "child-uid", ParentUID: "new-parent-uid"}
	oldChildren := []string{"sibling-uid"}
	newChildren := []string{"other-uid", "child-uid"}

	msgs := BuildB2BOrgReparentingMessages(current, updated, oldChildren, newChildren)

	require.Len(t, msgs, 3)

	data0, ok := msgs[0].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "child-uid", data0.UID)
	assert.Equal(t, []string{"b2b_org:new-parent-uid"}, data0.References["parent"])

	data1, ok := msgs[1].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "old-parent-uid", data1.UID)
	assert.Equal(t, []string{"b2b_org:sibling-uid"}, data1.References["child"])
	assert.Equal(t, b2bOrgNonChildRelations, data1.ExcludeRelations)

	data2, ok := msgs[2].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "new-parent-uid", data2.UID)
	assert.Equal(t, []string{"b2b_org:other-uid", "b2b_org:child-uid"}, data2.References["child"])
	assert.Equal(t, b2bOrgNonChildRelations, data2.ExcludeRelations)
}

func TestBuildB2BOrgReparentingMessages_ClearParent(t *testing.T) {
	current := &model.B2BOrg{UID: "child-uid", ParentUID: "old-parent-uid"}
	updated := &model.B2BOrg{UID: "child-uid", ParentUID: ""}
	msgs := BuildB2BOrgReparentingMessages(current, updated, nil, nil)

	require.Len(t, msgs, 1)
	data, ok := msgs[0].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Empty(t, data.References)
}

func TestBuildB2BOrgReparentingMessages_ClearParent_WithOldChildren(t *testing.T) {
	current := &model.B2BOrg{UID: "child-uid", ParentUID: "old-parent-uid"}
	updated := &model.B2BOrg{UID: "child-uid", ParentUID: ""}
	oldChildren := []string{"sibling-uid"}

	msgs := BuildB2BOrgReparentingMessages(current, updated, oldChildren, nil)

	require.Len(t, msgs, 2)
	data1, ok := msgs[1].Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "old-parent-uid", data1.UID)
	assert.Equal(t, []string{"b2b_org:sibling-uid"}, data1.References["child"])
}

func TestBuildB2BOrgReparentingMessages_NoChange(t *testing.T) {
	current := &model.B2BOrg{UID: "child-uid", ParentUID: "same-parent"}
	updated := &model.B2BOrg{UID: "child-uid", ParentUID: "same-parent"}
	msgs := BuildB2BOrgReparentingMessages(current, updated, nil, nil)

	assert.Nil(t, msgs)
}

func TestBuildB2BOrgFGAMessage_ExcludesParentChildAndMembership(t *testing.T) {
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Contains(t, data.ExcludeRelations, "parent")
	assert.Contains(t, data.ExcludeRelations, "child")
	assert.Contains(t, data.ExcludeRelations, "membership")
	assert.Contains(t, data.ExcludeRelations, "global_org_admin",
		"empty globalOrgAdminTeamUID must exclude global_org_admin to preserve existing tuples")
}

func TestBuildB2BOrgFGAMessage_GlobalOrgAdminNotExcludedWhenSet(t *testing.T) {
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{GlobalOrgAdminTeamUID: "global-admin-uid"})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.NotContains(t, data.ExcludeRelations, "global_org_admin",
		"non-empty globalOrgAdminTeamUID must not exclude global_org_admin — caller is setting it")
}

func TestBuildB2BOrgFGAMessage_NilWritersAuditorsExcluded(t *testing.T) {
	// nil writers/auditors means "don't touch these relations" (e.g. b2b_org field
	// update that must not wipe settings-driven ACL tuples set by a prior PUT /settings).
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Contains(t, data.ExcludeRelations, "writer",
		"nil writers must be excluded so update_access does not wipe existing writer tuples")
	assert.Contains(t, data.ExcludeRelations, "auditor",
		"nil auditors must be excluded so update_access does not wipe existing auditor tuples")
}

func TestBuildB2BOrgFGAMessage_ExplicitWritersAuditorsNotExcluded(t *testing.T) {
	// Non-nil (even empty) slices mean "replace this relation" — must NOT be excluded.
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{Writers: []string{}, Auditors: []string{}})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.NotContains(t, data.ExcludeRelations, "writer",
		"explicit (empty) writers must not be excluded — caller intends to clear all writers")
	assert.NotContains(t, data.ExcludeRelations, "auditor",
		"explicit (empty) auditors must not be excluded — caller intends to clear all auditors")
}

func TestBuildB2BOrgFGAMessage_WithWritersAndAuditors(t *testing.T) {
	writers := []string{"alice", "bob"}
	auditors := []string{"viewer1"}
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{GlobalOrgAdminTeamUID: "global-admin-uid", Writers: writers, Auditors: auditors})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, []string{"alice", "bob"}, data.Relations["writer"])
	assert.Equal(t, []string{"viewer1"}, data.Relations["auditor"])
	assert.Equal(t, []string{"team:global-admin-uid#member"}, data.References["global_org_admin"])
}

func TestBuildB2BOrgFGAMessage_WithMembershipUIDs(t *testing.T) {
	membershipUIDs := []string{"pm-uid-001", "pm-uid-002"}
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{MembershipUIDs: membershipUIDs})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t,
		[]string{"project_membership:pm-uid-001", "project_membership:pm-uid-002"},
		data.References["membership"],
	)
	assert.NotContains(t, data.ExcludeRelations, "membership")
}

func TestBuildB2BOrgFGAMessage_WithGlobalAdmin(t *testing.T) {
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{GlobalOrgAdminTeamUID: "global-admin-team-uid"})

	assert.Equal(t, "b2b_org", msg.ObjectType)
	assert.Equal(t, "update_access", msg.Operation)

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "b2b-org-uid-001", data.UID)
	require.Contains(t, data.References, "global_org_admin")
	assert.Equal(t, []string{"team:global-admin-team-uid#member"}, data.References["global_org_admin"])
}

func TestBuildB2BOrgFGAMessage_NoGlobalAdmin(t *testing.T) {
	msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{})

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.NotContains(t, data.References, "global_org_admin")
}

// TestPublishB2BOrgTeamGrantsFGA_Guard covers the widened early return. This
// emitter is the only FGA publisher on both /admin/reindex b2b_org paths, so
// keeping the old "blank global-admin UID → return" guard would make reindex —
// the tool an operator reaches for to repair missing grants — silently skip the
// auditor grants.
func TestPublishB2BOrgTeamGrantsFGA_Guard(t *testing.T) {
	// wantAuditorRefs and wantAdminRefs are spelled out as literals rather than
	// derived from teamMemberRefs: asserting with the renderer the code under
	// test uses would pass through any rendering regression.
	tests := []struct {
		name            string
		adminUID        string
		auditorTeams    []string
		wantPublish     bool
		wantAuditorRefs []string
		wantAdminRefs   []string
	}{
		{
			name: "blank admin UID with teams still publishes", adminUID: "",
			auditorTeams: []string{"staff-team"}, wantPublish: true,
			wantAuditorRefs: []string{"team:staff-team#member"},
		},
		{
			name: "admin UID with no teams still publishes", adminUID: "admin-uid",
			auditorTeams: nil, wantPublish: true,
			wantAdminRefs: []string{"team:admin-uid#member"},
		},
		{
			name: "both configured publishes", adminUID: "admin-uid",
			auditorTeams: []string{"staff-team", "second-team"}, wantPublish: true,
			wantAuditorRefs: []string{"team:staff-team#member", "team:second-team#member"},
			wantAdminRefs:   []string{"team:admin-uid#member"},
		},
		{name: "both absent publishes nothing", adminUID: "", auditorTeams: nil, wantPublish: false},
		{
			name: "whitespace-only values count as absent", adminUID: "  ",
			auditorTeams: []string{"  "}, wantPublish: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub := mock.NewMockMemberPublisher()
			PublishB2BOrgTeamGrantsFGA(context.Background(), pub, testB2BOrg, tt.adminUID, tt.auditorTeams)

			if !tt.wantPublish {
				assert.Empty(t, pub.CallOrder, "nothing to assert means nothing to publish")
				return
			}
			// CallOrder, not just a non-nil payload: this pins exactly one
			// publish, which LastAccessData alone would not catch.
			assert.Equal(t, []string{"access"}, pub.CallOrder)
			msg, ok := pub.LastAccessData.(fgatypes.GenericFGAMessage)
			require.True(t, ok)
			data, ok := msg.Data.(fgatypes.GenericAccessData)
			require.True(t, ok)

			if tt.wantAuditorRefs != nil {
				assert.Equal(t, tt.wantAuditorRefs, data.References["auditor"])
			} else {
				assert.NotContains(t, data.References, "auditor")
			}
			if tt.wantAdminRefs != nil {
				assert.Equal(t, tt.wantAdminRefs, data.References["global_org_admin"])
			} else {
				assert.NotContains(t, data.References, "global_org_admin",
					"a blank admin UID must not produce a team:#member subject")
			}
		})
	}
}

func TestBuildB2BOrgFGAMessage_AuditorTeams(t *testing.T) {
	tests := []struct {
		name         string
		auditorTeams []string
		auditors     []string
		wantRefs     []string
		wantRelation []string
		wantExcluded bool
	}{
		{
			// The common case on every non-settings path. The team refs must be
			// written even though "auditor" is excluded: fga-sync applies
			// ExcludeRelations only when deleting, so the exclusion preserves
			// per-user auditor tuples this caller is not managing.
			name:         "teams with nil auditors are written and auditor stays excluded",
			auditorTeams: []string{"staff-team", "second-team"},
			auditors:     nil,
			wantRefs:     []string{"team:staff-team#member", "team:second-team#member"},
			wantRelation: nil,
			wantExcluded: true,
		},
		{
			name:         "teams coexist with an explicit auditor list",
			auditorTeams: []string{"staff-team"},
			auditors:     []string{"alice"},
			wantRefs:     []string{"team:staff-team#member"},
			wantRelation: []string{"alice"},
			wantExcluded: false,
		},
		{
			name:         "blank and whitespace-only names are dropped",
			auditorTeams: []string{"staff-team", "", "   "},
			auditors:     nil,
			wantRefs:     []string{"team:staff-team#member"},
			wantRelation: nil,
			wantExcluded: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{
				AuditorTeams: tt.auditorTeams,
				Auditors:     tt.auditors,
			})

			data, ok := msg.Data.(fgatypes.GenericAccessData)
			require.True(t, ok)
			assert.Equal(t, tt.wantRefs, data.References["auditor"])
			assert.Equal(t, tt.wantRelation, data.Relations["auditor"])
			if tt.wantExcluded {
				assert.Contains(t, data.ExcludeRelations, "auditor",
					"nil auditors must still exclude auditor so per-user tuples survive")
			} else {
				assert.NotContains(t, data.ExcludeRelations, "auditor")
			}
		})
	}
}

// TestBuildB2BOrgFGAMessage_NoAuditorTeamsIsUnchanged pins the compatibility
// guarantee the spec makes: with no teams configured the message must be
// identical to what the pre-change implementation produced, so an operator can
// disable the grants and get exactly the old behaviour back.
func TestBuildB2BOrgFGAMessage_NoAuditorTeamsIsUnchanged(t *testing.T) {
	for _, teams := range [][]string{nil, {}, {"", "  "}} {
		msg := BuildB2BOrgFGAMessage(testB2BOrg, B2BOrgFGARefs{
			GlobalOrgAdminTeamUID: "global-admin-uid",
			AuditorTeams:          teams,
		})

		data, ok := msg.Data.(fgatypes.GenericAccessData)
		require.True(t, ok)
		assert.NotContains(t, data.References, "auditor",
			"no configured teams must leave References[auditor] absent, not empty")
		assert.Equal(t, []string{"team:global-admin-uid#member"}, data.References["global_org_admin"])
		assert.Contains(t, data.ExcludeRelations, "auditor")
	}
}

func TestBuildProjectMembershipFGAMessage(t *testing.T) {
	pm := &model.ProjectMembership{
		UID:        "pm-uid-001",
		B2BOrgUID:  "b2b-org-uid-001",
		ProjectUID: "project-uid-001",
	}
	msg := BuildProjectMembershipFGAMessage(pm)

	assert.Equal(t, "project_membership", msg.ObjectType)
	assert.Equal(t, "update_access", msg.Operation)

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Equal(t, "pm-uid-001", data.UID)
	assert.Equal(t, []string{"b2b_org:b2b-org-uid-001"}, data.References["b2b_org"])
	assert.Equal(t, []string{"project:project-uid-001"}, data.References["project"])
	assert.Contains(t, data.ExcludeRelations, "key_contact")
}

func TestBuildProjectMembershipFGAMessage_MissingParents(t *testing.T) {
	pm := &model.ProjectMembership{UID: "pm-uid-sparse"}
	msg := BuildProjectMembershipFGAMessage(pm)

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Empty(t, data.References)
	assert.Contains(t, data.ExcludeRelations, "key_contact")
	assert.NotContains(t, data.ExcludeRelations, "project")
	assert.NotContains(t, data.ExcludeRelations, "b2b_org")
}

func TestBuildProjectMembershipFGAMessagePreserveMissingRefs_MissingParents(t *testing.T) {
	pm := &model.ProjectMembership{UID: "pm-uid-sparse"}
	msg := BuildProjectMembershipFGAMessagePreserveMissingRefs(pm)

	data, ok := msg.Data.(fgatypes.GenericAccessData)
	require.True(t, ok)
	assert.Empty(t, data.References)
	assert.Contains(t, data.ExcludeRelations, "key_contact")
	assert.Contains(t, data.ExcludeRelations, "project")
	assert.Contains(t, data.ExcludeRelations, "b2b_org")
}

// ── BuildB2BOrgSettingsIndexingConfig ────────────────────────────────────────

var testB2BOrgWithParent = &model.B2BOrg{
	UID:           "org-uid-001",
	Name:          "Acme Corp",
	PrimaryDomain: "acme.com",
	DomainAliases: []string{"acmecorp.com"},
	ParentUID:     "parent-org-uid-001",
}

func TestBuildB2BOrgSettingsIndexingConfig_CoreFields(t *testing.T) {
	settings := &model.B2BOrgSettings{UID: "org-uid-001"}
	cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, settings)

	require.NotNil(t, cfg)
	require.NotNil(t, cfg.Public, "Public must be set explicitly")
	assert.False(t, *cfg.Public, "settings doc must never be public")
	assert.Equal(t, "org-uid-001", cfg.ObjectID)
	assert.Equal(t, "b2b_org:org-uid-001", cfg.AccessCheckObject)
	assert.Equal(t, fgaconstants.RelationAuditor, cfg.AccessCheckRelation)
	assert.Equal(t, "b2b_org:org-uid-001", cfg.HistoryCheckObject)
	assert.Equal(t, fgaconstants.RelationWriter, cfg.HistoryCheckRelation, "history = write-side concern")
	assert.Equal(t, "acme corp", cfg.SortName)
}

func TestBuildB2BOrgSettingsIndexingConfig_NameAndAliases(t *testing.T) {
	settings := &model.B2BOrgSettings{UID: "org-uid-001"}
	cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, settings)

	assert.Equal(t,
		[]string{"Acme Corp", "acme.com", "acmecorp.com"},
		cfg.NameAndAliases,
		"NameAndAliases must include org name + domains for domain-typeahead even with empty writers",
	)
}

func TestBuildB2BOrgSettingsIndexingConfig_ParentRefs(t *testing.T) {
	settings := &model.B2BOrgSettings{UID: "org-uid-001"}
	cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, settings)

	assert.Equal(t,
		[]string{"b2b_org:org-uid-001", "b2b_org:parent-org-uid-001"},
		cfg.ParentRefs,
	)
}

func TestBuildB2BOrgSettingsIndexingConfig_ParentRefs_NoParent(t *testing.T) {
	org := &model.B2BOrg{UID: "root-org", Name: "Root Org"}
	settings := &model.B2BOrgSettings{UID: "root-org"}
	cfg := BuildB2BOrgSettingsIndexingConfig(org, settings)

	assert.Equal(t, []string{"b2b_org:root-org"}, cfg.ParentRefs)
}

func TestBuildB2BOrgSettingsIndexingConfig_Fulltext_AcceptedAndPending(t *testing.T) {
	settings := &model.B2BOrgSettings{
		UID: "org-uid-001",
		Writers: []model.B2BOrgUser{
			{Name: "Alice Smith", Email: "alice@acme.com", InviteStatus: model.InviteStatusAccepted},
			{Email: "pending@acme.com", InviteStatus: model.InviteStatusPending},
		},
		Auditors: []model.B2BOrgUser{
			{Name: "Bob Jones", Email: "bob@acme.com", InviteStatus: model.InviteStatusAccepted},
		},
	}
	cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, settings)

	assert.Contains(t, cfg.Fulltext, "Alice Smith")
	assert.Contains(t, cfg.Fulltext, "alice@acme.com")
	assert.Contains(t, cfg.Fulltext, "pending@acme.com")
	assert.Contains(t, cfg.Fulltext, "Bob Jones")
	assert.Contains(t, cfg.Fulltext, "bob@acme.com")
}

func TestBuildB2BOrgSettingsIndexingConfig_Fulltext_ExcludesRevokedExpired(t *testing.T) {
	settings := &model.B2BOrgSettings{
		UID: "org-uid-001",
		Writers: []model.B2BOrgUser{
			{Name: "Revoked User", Email: "revoked@acme.com", InviteStatus: model.InviteStatusRevoked},
			{Name: "Expired User", Email: "expired@acme.com", InviteStatus: model.InviteStatusExpired},
		},
	}
	cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, settings)

	assert.NotContains(t, cfg.Fulltext, "revoked@acme.com", "revoked entries must not appear in fulltext")
	assert.NotContains(t, cfg.Fulltext, "expired@acme.com", "expired entries must not appear in fulltext")
}

func TestBuildB2BOrgSettingsIndexingConfig_Tags(t *testing.T) {
	tests := []struct {
		name         string
		settings     *model.B2BOrgSettings
		wantContains []string
		wantAbsent   []string
	}{
		{
			name: "accepted writer emits writer and member tags",
			settings: &model.B2BOrgSettings{
				UID: "org-uid-001",
				Writers: []model.B2BOrgUser{
					{InviteStatus: model.InviteStatusAccepted, Username: "alice"},
				},
				Auditors: []model.B2BOrgUser{
					{Email: "pending@acme.com", InviteStatus: model.InviteStatusPending},
				},
			},
			wantContains: []string{"has_writers", "has_pending_invites", "writer:alice", "member:alice"},
			wantAbsent:   []string{"has_auditors"},
		},
		{
			name: "accepted auditor emits auditor and member tags",
			settings: &model.B2BOrgSettings{
				UID: "org-uid-002",
				Auditors: []model.B2BOrgUser{
					{InviteStatus: model.InviteStatusAccepted, Username: "bob"},
				},
			},
			wantContains: []string{"has_auditors", "auditor:bob", "member:bob"},
			wantAbsent:   []string{"has_writers", "has_pending_invites"},
		},
		{
			name: "writer and auditor both get member tag, no duplicates across roles",
			settings: &model.B2BOrgSettings{
				UID:      "org-uid-003",
				Writers:  []model.B2BOrgUser{{InviteStatus: model.InviteStatusAccepted, Username: "alice"}},
				Auditors: []model.B2BOrgUser{{InviteStatus: model.InviteStatusAccepted, Username: "bob"}},
			},
			wantContains: []string{"member:alice", "member:bob"},
			wantAbsent:   []string{"has_pending_invites"},
		},
		{
			name: "same user accepted as both writer and auditor emits member tag only once",
			settings: &model.B2BOrgSettings{
				UID:      "org-uid-004",
				Writers:  []model.B2BOrgUser{{InviteStatus: model.InviteStatusAccepted, Username: "charlie"}},
				Auditors: []model.B2BOrgUser{{InviteStatus: model.InviteStatusAccepted, Username: "charlie"}},
			},
			wantContains: []string{"writer:charlie", "auditor:charlie", "member:charlie"},
			wantAbsent:   []string{"has_pending_invites"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := BuildB2BOrgSettingsIndexingConfig(testB2BOrgWithParent, tt.settings)
			for _, tag := range tt.wantContains {
				assert.Contains(t, cfg.Tags, tag)
			}
			for _, tag := range tt.wantAbsent {
				assert.NotContains(t, cfg.Tags, tag)
			}
		})
	}
}

// ── PublishB2BOrgParentFGA ───────────────────────────────────────────────────

func TestPublishB2BOrgParentFGA_NoParent_NoOp(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "child-uid", ParentUID: ""}

	PublishB2BOrgParentFGA(context.Background(), pub, org, nil)

	assert.Nil(t, pub.LastAccessData, "no FGA message should be published when ParentUID is empty")
}

func TestPublishB2BOrgParentFGA_WithParent_EmitsParentAndChildTuples(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "child-uid", ParentUID: "parent-uid"}
	parentChildren := []string{"child-uid", "sibling-uid"}

	PublishB2BOrgParentFGA(context.Background(), pub, org, parentChildren)

	require.NotNil(t, pub.LastAccessData, "FGA message must be published")
	// Two messages: parent tuple on child org + child-list tuple on parent org.
	accessCalls := 0
	for _, v := range pub.CallOrder {
		if v == "access" {
			accessCalls++
		}
	}
	assert.Equal(t, 2, accessCalls, "expected parent tuple + child-list tuple")
}

func TestPublishB2BOrgParentFGA_PublishError_Swallowed(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pub.SetAccessError(assert.AnError)
	org := &model.B2BOrg{UID: "child-uid", ParentUID: "parent-uid"}

	// Must not panic — fire-and-forget.
	PublishB2BOrgParentFGA(context.Background(), pub, org, []string{"child-uid"})
}

// ── PublishB2BOrgSettingsIndexer ─────────────────────────────────────────────

func TestPublishB2BOrgSettingsIndexer_PublishesToCorrectSubject(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "org-uid-pub-001", Name: "Pub Org"}
	settings := &model.B2BOrgSettings{
		UID:     "org-uid-pub-001",
		Writers: []model.B2BOrgUser{{Username: "alice", Email: "alice@acme.com", InviteStatus: model.InviteStatusAccepted}},
	}

	PublishB2BOrgSettingsIndexer(context.Background(), pub, org, settings, indexerConstants.ActionCreated)

	assert.Equal(t, constants.IndexB2BOrgSettingsSubject, pub.LastIndexSubject)
}

func TestPublishB2BOrgSettingsIndexer_PublishError_Swallowed(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pub.SetIndexerError(assert.AnError)
	org := &model.B2BOrg{UID: "org-uid-pub-002", Name: "Pub Org 2"}
	settings := &model.B2BOrgSettings{
		UID:     "org-uid-pub-002",
		Writers: []model.B2BOrgUser{{Username: "bob", Email: "bob@acme.com", InviteStatus: model.InviteStatusAccepted}},
	}

	// Must not panic or return an error — fire-and-forget.
	PublishB2BOrgSettingsIndexer(context.Background(), pub, org, settings, indexerConstants.ActionUpdated)
}

// ── buildB2BOrgSettingsIndexerView ───────────────────────────────────────────

func TestBuildB2BOrgSettingsIndexerView_FlatMembersWithRole(t *testing.T) {
	settings := &model.B2BOrgSettings{
		UID: "org-view-001",
		Writers: []model.B2BOrgUser{
			{Username: "alice", Email: "alice@acme.com", Name: "Alice A", Avatar: "https://example.com/alice.png", InviteStatus: model.InviteStatusAccepted},
		},
		Auditors: []model.B2BOrgUser{
			{Username: "bob", Email: "bob@acme.com", Name: "Bob B", InviteStatus: model.InviteStatusAccepted},
		},
	}

	view := buildB2BOrgSettingsIndexerView(settings)

	require.Len(t, view.Members, 2)
	assert.Equal(t, "alice", view.Members[0].Username)
	assert.Equal(t, "writer", view.Members[0].Role)
	assert.Equal(t, "https://example.com/alice.png", view.Members[0].Avatar, "avatar must surface on the indexer projection")
	assert.Equal(t, "bob", view.Members[1].Username)
	assert.Equal(t, "auditor", view.Members[1].Role)
	assert.Empty(t, view.Members[1].Avatar)
	assert.Equal(t, "org-view-001", view.UID)
}

func TestBuildB2BOrgSettingsIndexerView_WriterPrecedence(t *testing.T) {
	// User in both writers and auditors must appear once with role "writer".
	settings := &model.B2BOrgSettings{
		UID:      "org-view-002",
		Writers:  []model.B2BOrgUser{{Username: "charlie", Email: "c@example.com", InviteStatus: model.InviteStatusAccepted}},
		Auditors: []model.B2BOrgUser{{Username: "charlie", Email: "c@example.com", InviteStatus: model.InviteStatusAccepted}},
	}

	view := buildB2BOrgSettingsIndexerView(settings)

	require.Len(t, view.Members, 1, "duplicate user must appear exactly once")
	assert.Equal(t, "writer", view.Members[0].Role)
}

func TestBuildB2BOrgSettingsIndexerView_PendingUsersIncluded(t *testing.T) {
	// Pending users (no username) must appear in members[] but tags must exclude them.
	settings := &model.B2BOrgSettings{
		UID: "org-view-003",
		Writers: []model.B2BOrgUser{
			{Email: "pending@example.com", InviteStatus: model.InviteStatusPending},
		},
	}

	view := buildB2BOrgSettingsIndexerView(settings)

	require.Len(t, view.Members, 1, "pending user must appear in members[]")
	assert.Equal(t, "writer", view.Members[0].Role)
	assert.Equal(t, model.InviteStatusPending, view.Members[0].InviteStatus)
	assert.Empty(t, view.Members[0].Username)

	// Tags must not include writer: or member: for the pending user (no username).
	for _, tag := range settings.Tags() {
		assert.NotEqual(t, "writer:"+view.Members[0].Username, tag)
		assert.NotEqual(t, "member:"+view.Members[0].Username, tag)
	}
}

func TestBuildB2BOrgSettingsIndexerView_RevokedExpiredExcluded(t *testing.T) {
	settings := &model.B2BOrgSettings{
		UID: "org-view-004",
		Writers: []model.B2BOrgUser{
			{Username: "active", Email: "active@example.com", InviteStatus: model.InviteStatusAccepted},
			{Username: "revoked", Email: "revoked@example.com", InviteStatus: model.InviteStatusRevoked},
			{Username: "expired", Email: "expired@example.com", InviteStatus: model.InviteStatusExpired},
		},
	}

	view := buildB2BOrgSettingsIndexerView(settings)

	require.Len(t, view.Members, 1, "only accepted entry must appear")
	assert.Equal(t, "active", view.Members[0].Username)
}

func TestBuildB2BOrgSettingsIndexerView_EmptySettingsProducesEmptySlice(t *testing.T) {
	settings := &model.B2BOrgSettings{UID: "org-view-005"}

	view := buildB2BOrgSettingsIndexerView(settings)

	assert.NotNil(t, view.Members, "members must be [] not nil")
	assert.Empty(t, view.Members)
}

// ── Workspace indexer delete payload ───────────────────────────────────────────

func TestBuildWorkspaceIndexerInput_DeleteCarriesUIDString(t *testing.T) {
	ws := &model.Workspace{UID: "ws-uid-001", Name: "North America Programs"}
	input := buildWorkspaceIndexerInput(ws, indexerConstants.ActionDeleted)
	assert.Equal(t, "ws-uid-001", input)
}

func TestBuildWorkspaceIndexerInput_CreateCarriesStruct(t *testing.T) {
	ws := &model.Workspace{UID: "ws-uid-001", Name: "North America Programs"}
	input := buildWorkspaceIndexerInput(ws, indexerConstants.ActionCreated)
	assert.Equal(t, ws, input)
}

func TestBuildWorkspaceProjectIndexerInput_DeleteCarriesCompoundID(t *testing.T) {
	wp := model.WorkspaceProject{ProjectUID: "proj-uid-001"}
	input := buildWorkspaceProjectIndexerInput(
		"org-uid-001",
		"ws-uid-001",
		wp,
		model.WorkspaceProjects{},
		indexerConstants.ActionDeleted,
	)
	assert.Equal(t, "ws-uid-001:proj-uid-001", input)
}

func TestBuildWorkspaceProjectIndexerInput_CreateCarriesView(t *testing.T) {
	wp := model.WorkspaceProject{ProjectUID: "proj-uid-001", ProjectSlug: "cncf"}
	wps := model.WorkspaceProjects{}
	input := buildWorkspaceProjectIndexerInput(
		"org-uid-001",
		"ws-uid-001",
		wp,
		wps,
		indexerConstants.ActionCreated,
	)
	assert.Equal(t, buildWorkspaceProjectIndexerView("org-uid-001", "ws-uid-001", wp, wps), input)
	assert.NotEqual(t, "ws-uid-001:proj-uid-001", input, "create must carry the view struct, not the delete ID string")
}

func TestBuildWorkspaceProjectIndexingConfig_ObjectIDIsColonCompound(t *testing.T) {
	org := &model.B2BOrg{UID: "org-uid-001"}
	ws := &model.Workspace{UID: "ws-uid-001"}
	wp := model.WorkspaceProject{ProjectUID: "proj-uid-001", ProjectSlug: "cncf"}

	cfg := BuildWorkspaceProjectIndexingConfig(org, ws, wp)

	require.NotNil(t, cfg)
	// ObjectID becomes the OpenSearch document _id. The separator must be ':' —
	// a '/' is read as an extra URL path segment on the index request (the
	// OpenSearch client does not escape it) and the write fails with 400.
	assert.Equal(t, "ws-uid-001:proj-uid-001", cfg.ObjectID)
}

func TestPublishWorkspaceIndexer_DeleteMessageDataIsUIDString(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "0014100000Te2QjAAJ", Name: "Red Hat LLC"}
	ws := &model.Workspace{UID: "ws-uid-del-001", Name: "Delete probe"}

	PublishWorkspaceIndexer(context.Background(), pub, org, ws, indexerConstants.ActionDeleted)

	require.NotNil(t, pub.LastIndexerPayload)
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok)
	assert.Equal(t, indexerConstants.ActionDeleted, msg.Action)
	assert.Equal(t, "ws-uid-del-001", msg.Data,
		"deleted workspace indexer message must carry UID string as data")
	assert.Equal(t, constants.IndexOrgWorkspaceSubject, pub.LastIndexSubject)

	payload, err := json.Marshal(msg)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(payload, &wire))
	assert.IsType(t, "", wire["data"], "indexer wire format requires string data for delete actions")
}

func TestPublishWorkspaceProjectIndexer_DeleteMessageDataIsCompoundID(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "0014100000Te2QjAAJ", Name: "Red Hat LLC"}
	ws := &model.Workspace{UID: "ws-uid-del-001", Name: "Delete probe"}
	wp := model.WorkspaceProject{ProjectUID: "proj-uid-cncf", ProjectSlug: "cncf"}
	wps := model.WorkspaceProjects{WorkspaceUID: ws.UID, OrgUID: org.UID}

	PublishWorkspaceProjectIndexer(context.Background(), pub, org, ws, wp, wps, indexerConstants.ActionDeleted)

	require.NotNil(t, pub.LastIndexerPayload)
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok)
	assert.Equal(t, indexerConstants.ActionDeleted, msg.Action)
	assert.Equal(t, "ws-uid-del-001:proj-uid-cncf", msg.Data)
	assert.Equal(t, constants.IndexOrgWorkspaceProjectSubject, pub.LastIndexSubject)

	payload, err := json.Marshal(msg)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(payload, &wire))
	assert.IsType(t, "", wire["data"], "indexer wire format requires string data for delete actions")
}

// ── B2BOrg / ProjectMembership / KeyContact delete payload ──────────────────────

// assertDeleteDataIsUIDString marshals the message and asserts wire data is a JSON string.
func assertDeleteDataIsUIDString(t *testing.T, msg *model.MemberIndexerMessage, wantUID string) {
	t.Helper()
	assert.Equal(t, indexerConstants.ActionDeleted, msg.Action)
	assert.Equal(t, wantUID, msg.Data, "deleted indexer message must carry UID string as data")
	payload, err := json.Marshal(msg)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(payload, &wire))
	assert.IsType(t, "", wire["data"], "indexer wire format requires string data for delete actions")
}

func TestBuildB2BOrgIndexerInput_DeleteVsCreate(t *testing.T) {
	org := &model.B2BOrg{UID: "org-uid-001", Name: "Red Hat"}
	assert.Equal(t, "org-uid-001", buildB2BOrgIndexerInput(org, indexerConstants.ActionDeleted))
	assert.Equal(t, org, buildB2BOrgIndexerInput(org, indexerConstants.ActionCreated))
}

func TestBuildProjectMembershipIndexerInput_DeleteVsCreate(t *testing.T) {
	pm := &model.ProjectMembership{UID: "pm-uid-001"}
	assert.Equal(t, "pm-uid-001", buildProjectMembershipIndexerInput(pm, indexerConstants.ActionDeleted))
	assert.Equal(t, pm, buildProjectMembershipIndexerInput(pm, indexerConstants.ActionUpdated))
}

func TestBuildKeyContactIndexerInput_DeleteVsCreate(t *testing.T) {
	kc := &model.KeyContact{UID: "kc-uid-001"}
	assert.Equal(t, "kc-uid-001", buildKeyContactIndexerInput(kc, indexerConstants.ActionDeleted))
	assert.Equal(t, kc, buildKeyContactIndexerInput(kc, indexerConstants.ActionCreated))
}

func TestPublishB2BOrgIndexer_DeleteMessageDataIsUIDString(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	org := &model.B2BOrg{UID: "org-del-001", Name: "Red Hat LLC"}

	PublishB2BOrgIndexer(context.Background(), pub, org, indexerConstants.ActionDeleted)

	require.NotNil(t, pub.LastIndexerPayload)
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok)
	assertDeleteDataIsUIDString(t, msg, "org-del-001")
	assert.Equal(t, constants.IndexB2BOrgSubject, pub.LastIndexSubject)
}

func TestPublishProjectMembershipIndexer_DeleteMessageDataIsUIDString(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	pm := &model.ProjectMembership{UID: "pm-del-001"}

	PublishProjectMembershipIndexer(context.Background(), pub, pm, indexerConstants.ActionDeleted)

	require.NotNil(t, pub.LastIndexerPayload)
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok)
	assertDeleteDataIsUIDString(t, msg, "pm-del-001")
	assert.Equal(t, constants.IndexProjectMembershipSubject, pub.LastIndexSubject)
}

func TestPublishKeyContactIndexer_DeleteMessageDataIsUIDString(t *testing.T) {
	pub := mock.NewMockMemberPublisher()
	kc := &model.KeyContact{UID: "kc-del-001", MembershipUID: "pm-001"}

	PublishKeyContactIndexer(context.Background(), pub, kc, indexerConstants.ActionDeleted)

	require.NotNil(t, pub.LastIndexerPayload)
	msg, ok := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
	require.True(t, ok)
	assertDeleteDataIsUIDString(t, msg, "kc-del-001")
	assert.Equal(t, constants.IndexKeyContactSubject, pub.LastIndexSubject)
}

func TestPublishIndexer_CreateCarriesFullObject(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		publish func(*mock.MockMemberPublisher)
		msg     string
	}{
		{"b2b_org", func(p *mock.MockMemberPublisher) {
			PublishB2BOrgIndexer(ctx, p, &model.B2BOrg{UID: "org-c-001", Name: "Acme"}, indexerConstants.ActionCreated)
		}, "create b2b_org must carry an object payload"},
		{"project_membership", func(p *mock.MockMemberPublisher) {
			PublishProjectMembershipIndexer(ctx, p, &model.ProjectMembership{UID: "pm-c-001"}, indexerConstants.ActionUpdated)
		}, "update project_membership must carry an object payload"},
		{"key_contact", func(p *mock.MockMemberPublisher) {
			PublishKeyContactIndexer(ctx, p, &model.KeyContact{UID: "kc-c-001"}, indexerConstants.ActionCreated)
		}, "create key_contact must carry an object payload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := mock.NewMockMemberPublisher()
			tc.publish(pub)
			msg := pub.LastIndexerPayload.(*model.MemberIndexerMessage)
			assert.IsType(t, map[string]any{}, msg.Data, tc.msg)
		})
	}
}

// TestBuildDeleteAccessMessages pins the exact wire payload of the delete_access
// envelope against docs/fga-contract.md. The assertion is on serialized
// JSON rather than the struct because the consumer is a separate service reading
// the bytes — a field-name change would keep the struct assertions passing while
// silently breaking the contract.
func TestBuildDeleteAccessMessages(t *testing.T) {
	cases := []struct {
		name     string
		build    func(string) fgatypes.GenericFGAMessage
		uid      string
		expected string
	}{
		{
			name:     "b2b_org",
			build:    BuildB2BOrgDeleteAccessMessage,
			uid:      "0014100000Td9x0AAB",
			expected: `{"object_type":"b2b_org","operation":"delete_access","data":{"uid":"0014100000Td9x0AAB"}}`,
		},
		{
			name:     "project_membership",
			build:    BuildProjectMembershipDeleteAccessMessage,
			uid:      "a0H4100000XyZabEAF",
			expected: `{"object_type":"project_membership","operation":"delete_access","data":{"uid":"a0H4100000XyZabEAF"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.build(tc.uid)

			assert.Equal(t, "delete_access", msg.Operation,
				"operation must be the literal the consumer switches on")
			require.IsType(t, fgatypes.GenericDeleteData{}, msg.Data,
				"delete_access must carry GenericDeleteData, not GenericAccessData")

			got, err := json.Marshal(msg)
			require.NoError(t, err)
			assert.JSONEq(t, tc.expected, string(got))
		})
	}
}

// TestBuildDeleteAccessMessageCarriesNoRelations guards the property that makes
// delete_access unscopeable: it has no relation or exclusion field to narrow it.
// A future author tempted to add one should change the contract deliberately
// rather than discover the omission by accident.
func TestBuildDeleteAccessMessageCarriesNoRelations(t *testing.T) {
	raw, err := json.Marshal(BuildB2BOrgDeleteAccessMessage("0014100000Td9x0AAB"))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))

	data, ok := decoded["data"].(map[string]any)
	require.True(t, ok, "data must be an object")
	assert.Equal(t, []string{"uid"}, mapKeys(data),
		"delete_access data must carry uid and nothing else")
}

// mapKeys returns the sorted keys of m, so key-set assertions are order-stable.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
