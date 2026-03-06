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

func TestMemberReaderOrchestrator_ListMembers(t *testing.T) {
	tests := []struct {
		name      string
		params    model.ListParams
		wantErr   bool
		wantCount int
	}{
		{
			name: "list all members",
			params: model.ListParams{
				PageSize: 25,
				Offset:   0,
				Filters:  map[string]string{},
			},
			wantErr:   false,
			wantCount: 1,
		},
		{
			name: "list members with search",
			params: model.ListParams{
				PageSize: 25,
				Offset:   0,
				Filters:  map[string]string{},
				Search:   "Example",
			},
			wantErr:   false,
			wantCount: 1,
		},
		{
			name: "list members with non-matching search",
			params: model.ListParams{
				PageSize: 25,
				Offset:   0,
				Filters:  map[string]string{},
				Search:   "NonExistent",
			},
			wantErr:   false,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			members, totalSize, err := orchestrator.ListMembers(context.Background(), tt.params)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, members, tt.wantCount)
			assert.Equal(t, tt.wantCount, totalSize)
		})
	}
}

func TestMemberReaderOrchestrator_GetMembershipForMember(t *testing.T) {
	tests := []struct {
		name          string
		memberUID     string
		membershipUID string
		wantErr       bool
	}{
		{
			name:          "get existing membership for member",
			memberUID:     "member-1",
			membershipUID: "membership-1",
			wantErr:       false,
		},
		{
			name:          "get non-existing membership",
			memberUID:     "member-1",
			membershipUID: "non-existing",
			wantErr:       true,
		},
		{
			name:          "membership belongs to different member",
			memberUID:     "wrong-member",
			membershipUID: "membership-1",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			membership, _, err := orchestrator.GetMembershipForMember(context.Background(), tt.memberUID, tt.membershipUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, membership)
			assert.Equal(t, tt.membershipUID, membership.UID)
		})
	}
}

func TestMemberReaderOrchestrator_ListKeyContactsForMembership(t *testing.T) {
	tests := []struct {
		name          string
		memberUID     string
		membershipUID string
		wantErr       bool
		wantCount     int
	}{
		{
			name:          "list contacts for existing membership",
			memberUID:     "member-1",
			membershipUID: "membership-1",
			wantErr:       false,
			wantCount:     1,
		},
		{
			name:          "list contacts for non-existing membership",
			memberUID:     "member-1",
			membershipUID: "non-existing",
			wantErr:       true,
		},
		{
			name:          "list contacts for wrong member",
			memberUID:     "wrong-member",
			membershipUID: "membership-1",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := mock.NewMockMembershipRepository()
			orchestrator := NewMemberReaderOrchestrator(WithMemberReader(mockRepo))

			contacts, err := orchestrator.ListKeyContactsForMembership(context.Background(), tt.memberUID, tt.membershipUID)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Len(t, contacts, tt.wantCount)
		})
	}
}

func TestNewMemberReaderOrchestrator_PanicsWithoutReader(t *testing.T) {
	assert.Panics(t, func() {
		NewMemberReaderOrchestrator()
	})
}
