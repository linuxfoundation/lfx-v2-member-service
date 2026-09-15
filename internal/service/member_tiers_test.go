// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	pkgerrors "github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// fakeMemberReader resolves memberships and key contacts from maps; unknown
// membership UIDs return NotFound. The embedded interface is left nil so any
// other port.MemberReader method panics if a test reaches it unexpectedly.
type fakeMemberReader struct {
	port.MemberReader
	memberships map[string]*model.ProjectMembership
	keyContacts map[string][]*model.KeyContact
}

func (f *fakeMemberReader) GetMembership(_ context.Context, uid string) (*model.ProjectMembership, error) {
	if pm, ok := f.memberships[uid]; ok {
		return pm, nil
	}
	return nil, pkgerrors.NewNotFound("membership not found")
}

func (f *fakeMemberReader) ListKeyContactsForMembership(_ context.Context, membershipUID string) ([]*model.KeyContact, error) {
	return f.keyContacts[membershipUID], nil
}

func TestMembershipCountsAsActive(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		status  string
		endDate string
		want    bool
	}{
		{name: "active with no end date", status: "Active", want: true},
		// Salesforce picklist casing varies between orgs; casing must not drop members.
		{name: "status matches case-insensitively", status: "ACTIVE", want: true},
		{name: "non-active status", status: "Expired", endDate: "2099-12-31", want: false},
		{name: "empty status", status: "", want: false},
		{name: "end date in the past", status: "Active", endDate: "2026-03-14", want: false},
		// A membership stays active through its end date, not just until it.
		{name: "end date today still counts", status: "Active", endDate: "2026-03-15", want: true},
		{name: "end date in the future", status: "Active", endDate: "2027-01-01", want: true},
		{name: "RFC3339 end date in the past", status: "Active", endDate: "2020-01-01T00:00:00Z", want: false},
		// Status is the authority; a malformed date must not silently drop a member.
		{name: "unparseable end date does not deactivate", status: "Active", endDate: "03/15/2020", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &model.ProjectMembership{Status: tt.status, EndDate: tt.endDate}
			assert.Equal(t, tt.want, membershipCountsAsActive(m, now))
		})
	}
}

func TestMembershipOutranks(t *testing.T) {
	m := func(tierName, endDate string) *model.ProjectMembership {
		return &model.ProjectMembership{TierName: tierName, EndDate: endDate}
	}
	tests := []struct {
		name      string
		candidate *model.ProjectMembership
		current   *model.ProjectMembership
		want      bool
	}{
		{
			name:      "higher tier class wins regardless of end dates",
			candidate: m("Platinum Membership", "2026-01-01"),
			current:   m("Gold Membership", "2099-12-31"),
			want:      true,
		},
		{
			name:      "lower tier class never wins",
			candidate: m("Gold Membership", "2099-12-31"),
			current:   m("Platinum Membership", "2026-01-01"),
			want:      false,
		},
		{
			name:      "same class: later end date wins",
			candidate: m("Gold Membership", "2027-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      true,
		},
		{
			name:      "same class: earlier end date loses",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2027-01-01"),
			want:      false,
		},
		{
			// An absent end date is treated as open-ended, so it outlasts any dated membership.
			name:      "same class: open-ended beats dated",
			candidate: m("Gold Membership", ""),
			current:   m("Gold Membership", "2099-01-01"),
			want:      true,
		},
		{
			// Equal on both criteria keeps the incumbent, making the winner deterministic.
			name:      "same class and end date keeps the incumbent",
			candidate: m("Gold Membership", "2026-01-01"),
			current:   m("Gold Membership", "2026-01-01"),
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, membershipOutranks(tt.candidate, tt.current))
		})
	}
}

// TestHighestActiveTiers_HighestTierPerOrg exercises HighestActiveTiers
// itself, not just indirectly via the HTTP handler: two memberships on the
// same org must collapse to the higher tier, ordered highest tier first.
func TestHighestActiveTiers_HighestTierPerOrg(t *testing.T) {
	umr := mock.NewMockUserMembershipReader()
	umr.SetUserMemberships("jdoe", []string{"m-silver", "m-platinum", "m-gold"})
	storage := &fakeMemberReader{
		memberships: map[string]*model.ProjectMembership{
			"m-silver":   {UID: "m-silver", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Silver Membership", Status: "Active"},
			"m-platinum": {UID: "m-platinum", B2BOrgUID: "org-a", CompanyName: "Alpha Corp", TierName: "Platinum Membership", Status: "Active"},
			"m-gold":     {UID: "m-gold", B2BOrgUID: "org-b", CompanyName: "Beta Corp", TierName: "Gold Corporate Membership", Status: "Active"},
		},
		keyContacts: map[string][]*model.KeyContact{
			"m-silver":   {{UID: "kc-silver", MembershipUID: "m-silver", Username: "jdoe", Status: "Active"}},
			"m-platinum": {{UID: "kc-platinum", MembershipUID: "m-platinum", Username: "jdoe", Status: "Active"}},
			"m-gold":     {{UID: "kc-gold", MembershipUID: "m-gold", Username: "jdoe", Status: "Active"}},
		},
	}
	uc := NewMemberTiers(storage, umr, nil)

	res, err := uc.HighestActiveTiers(context.Background(), "jdoe")

	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.Equal(t, "org-a", res[0].B2BOrgUID)
	assert.Equal(t, "m-platinum", res[0].UID, "the higher tier on org-a must win over the silver record")
	assert.Equal(t, "org-b", res[1].B2BOrgUID)
}

// TestUserIsActiveKeyContact_UsernameMatch covers the direct-username branch:
// an active contact record whose Username matches is a hit, an inactive one
// or a different username is not.
func TestUserIsActiveKeyContact_UsernameMatch(t *testing.T) {
	tests := []struct {
		name     string
		contacts []*model.KeyContact
		want     bool
	}{
		{
			name:     "active contact with matching username",
			contacts: []*model.KeyContact{{UID: "kc-1", Username: "jdoe", Status: "Active"}},
			want:     true,
		},
		{
			name:     "inactive contact with matching username does not count",
			contacts: []*model.KeyContact{{UID: "kc-1", Username: "jdoe", Status: "Inactive"}},
			want:     false,
		},
		{
			name:     "active contact with a different username does not count",
			contacts: []*model.KeyContact{{UID: "kc-1", Username: "other", Status: "Active"}},
			want:     false,
		},
		{
			name:     "no contacts at all",
			contacts: nil,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := &fakeMemberReader{keyContacts: map[string][]*model.KeyContact{"m-1": tt.contacts}}
			uc := NewMemberTiers(storage, mock.NewMockUserMembershipReader(), nil)

			got, err := uc.userIsActiveKeyContact(context.Background(), "m-1", "jdoe")

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestUserIsActiveKeyContact_GrantIndexFallback covers the blank-Username
// branch: a production contact record (no Username resolved) is matched
// through the key-contact grant index instead, keyed by the contact UID.
func TestUserIsActiveKeyContact_GrantIndexFallback(t *testing.T) {
	storage := &fakeMemberReader{keyContacts: map[string][]*model.KeyContact{
		"m-1": {{UID: "kc-1", Username: "", Status: "Active"}},
	}}

	t.Run("matching grant is a hit", func(t *testing.T) {
		grantIndex := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "m-1", Username: "jdoe"},
		}}
		uc := NewMemberTiers(storage, mock.NewMockUserMembershipReader(), grantIndex)

		got, err := uc.userIsActiveKeyContact(context.Background(), "m-1", "jdoe")
		require.NoError(t, err)
		assert.True(t, got)
	})

	t.Run("grant for a different membership is not a hit", func(t *testing.T) {
		grantIndex := &mock.MockKeyContactGrantIndex{Entries: map[string]port.KeyContactGrant{
			"kc-1": {MembershipUID: "m-other", Username: "jdoe"},
		}}
		uc := NewMemberTiers(storage, mock.NewMockUserMembershipReader(), grantIndex)

		got, err := uc.userIsActiveKeyContact(context.Background(), "m-1", "jdoe")
		require.NoError(t, err)
		assert.False(t, got)
	})

	t.Run("nil grant index skips unmatchable contacts", func(t *testing.T) {
		uc := NewMemberTiers(storage, mock.NewMockUserMembershipReader(), nil)

		got, err := uc.userIsActiveKeyContact(context.Background(), "m-1", "jdoe")
		require.NoError(t, err)
		assert.False(t, got)
	})
}
