// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemberReaderOrchestrator_ListTiersForProject(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		wantErr   bool
		wantCount int
	}{
		{
			name:      "list tiers for project with sample data",
			projectID: "project-uid-1",
			wantErr:   false,
			wantCount: 1,
		},
		{
			name:      "list tiers for project with no data",
			projectID: "nonexistent-project",
			wantErr:   false,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			tiers, err := orchestrator.ListTiersForProject(context.Background(), tt.projectID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, tiers, tt.wantCount)
		})
	}
}

func TestMemberReaderOrchestrator_GetTier(t *testing.T) {
	tests := []struct {
		name    string
		tierUID string
		wantErr bool
	}{
		{
			name:    "get existing tier",
			tierUID: "tier-1",
			wantErr: false,
		},
		{
			name:    "get nonexistent tier",
			tierUID: "nonexistent-tier",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			tier, err := orchestrator.GetTier(context.Background(), tt.tierUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, tier)
			assert.Equal(t, tt.tierUID, tier.UID)
		})
	}
}

func TestMemberReaderOrchestrator_ListMembershipsForProject(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		wantErr   bool
		wantCount int
	}{
		{
			name:      "list memberships for project with sample data",
			projectID: "project-uid-1",
			wantErr:   false,
			wantCount: 1,
		},
		{
			name:      "list memberships for project with no data",
			projectID: "nonexistent-project",
			wantErr:   false,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			page, err := orchestrator.ListMembershipsForProject(context.Background(), tt.projectID, model.MembershipFilters{}, 25)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, page.Memberships, tt.wantCount)
		})
	}
}

func TestMemberReaderOrchestrator_GetMembership(t *testing.T) {
	tests := []struct {
		name          string
		membershipUID string
		wantErr       bool
	}{
		{
			name:          "get existing membership",
			membershipUID: "11111111-1111-1111-1111-111111111111",
			wantErr:       false,
		},
		{
			name:          "get nonexistent membership",
			membershipUID: "nonexistent-membership",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			membership, err := orchestrator.GetMembership(context.Background(), tt.membershipUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, membership)
			assert.Equal(t, tt.membershipUID, membership.UID)
		})
	}
}

func TestMemberReaderOrchestrator_ListKeyContactsForMembership(t *testing.T) {
	tests := []struct {
		name          string
		membershipUID string
		wantErr       bool
		wantCount     int
	}{
		{
			name:          "list contacts for existing membership",
			membershipUID: "11111111-1111-1111-1111-111111111111",
			wantErr:       false,
			wantCount:     1,
		},
		{
			name:          "list contacts for membership with no contacts",
			membershipUID: "nonexistent-membership",
			wantErr:       false,
			wantCount:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			contacts, err := orchestrator.ListKeyContactsForMembership(context.Background(), tt.membershipUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, contacts, tt.wantCount)
		})
	}
}

func TestMemberReaderOrchestrator_GetKeyContact(t *testing.T) {
	tests := []struct {
		name          string
		keyContactUID string
		wantErr       bool
	}{
		{
			name:          "get existing key contact",
			keyContactUID: "contact-role-1",
			wantErr:       false,
		},
		{
			name:          "get nonexistent key contact",
			keyContactUID: "nonexistent-contact",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			contact, err := orchestrator.GetKeyContact(context.Background(), tt.keyContactUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, contact)
			assert.Equal(t, tt.keyContactUID, contact.UID)
		})
	}
}

func TestNewMemberReaderOrchestrator_PanicsWithoutReader(t *testing.T) {
	assert.Panics(t, func() {
		NewMemberReaderOrchestrator()
	})
}
