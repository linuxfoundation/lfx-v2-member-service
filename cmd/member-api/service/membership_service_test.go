// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/auth"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/infrastructure/mock"
	usecaseSvc "github.com/linuxfoundation/lfx-v2-member-service/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService() membershipservice.Service {
	mockRepo := mock.NewMockMembershipRepository()
	orchestrator := usecaseSvc.NewMemberReaderOrchestrator(
		usecaseSvc.WithMemberReader(mockRepo),
	)
	jwtAuth, _ := auth.NewJWTAuth(auth.JWTAuthConfig{
		MockLocalPrincipal: "test-user",
	})
	return NewMembershipService(orchestrator, mockRepo, jwtAuth)
}

func TestListMembers(t *testing.T) {
	tests := []struct {
		name       string
		payload    *membershipservice.ListMembersPayload
		wantErr    bool
		wantCount  int
		wantTotal  int
	}{
		{
			name: "list all members",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
			},
			wantErr:   false,
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name: "list members with search",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Search:   strPtr("Example"),
			},
			wantErr:   false,
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name: "list members with non-matching search",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Search:   strPtr("NonExistent"),
			},
			wantErr:   false,
			wantCount: 0,
			wantTotal: 0,
		},
		{
			name: "list members with name filter",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Filter:   strPtr("name=Example"),
			},
			wantErr:   false,
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name: "list members with project_slug filter",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Filter:   strPtr("project_slug=linux-foundation"),
			},
			wantErr:   false,
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name: "list members with non-matching project_slug filter",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Filter:   strPtr("project_slug=non-existent"),
			},
			wantErr:   false,
			wantCount: 0,
			wantTotal: 0,
		},
		{
			name: "search members by project slug",
			payload: &membershipservice.ListMembersPayload{
				PageSize: 25,
				Offset:   0,
				Search:   strPtr("linux-foundation"),
			},
			wantErr:   false,
			wantCount: 1,
			wantTotal: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService()
			ctx := context.Background()

			res, err := svc.ListMembers(ctx, tt.payload)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, res)
			assert.Len(t, res.Members, tt.wantCount)
			assert.Equal(t, tt.wantTotal, res.Metadata.TotalSize)
		})
	}
}

func TestGetMemberMembership(t *testing.T) {
	tests := []struct {
		name    string
		payload *membershipservice.GetMemberMembershipPayload
		wantErr bool
	}{
		{
			name: "get existing membership for member",
			payload: &membershipservice.GetMemberMembershipPayload{
				MemberID: strPtr("member-1"),
				ID:       strPtr("membership-1"),
			},
			wantErr: false,
		},
		{
			name: "get non-existing membership",
			payload: &membershipservice.GetMemberMembershipPayload{
				MemberID: strPtr("member-1"),
				ID:       strPtr("non-existing"),
			},
			wantErr: true,
		},
		{
			name: "get membership for wrong member",
			payload: &membershipservice.GetMemberMembershipPayload{
				MemberID: strPtr("wrong-member"),
				ID:       strPtr("membership-1"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService()
			ctx := context.Background()

			res, err := svc.GetMemberMembership(ctx, tt.payload)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, res)
			assert.NotNil(t, res.Membership)
		})
	}
}

func TestListMemberMembershipKeyContacts(t *testing.T) {
	tests := []struct {
		name      string
		payload   *membershipservice.ListMemberMembershipKeyContactsPayload
		wantErr   bool
		wantCount int
	}{
		{
			name: "list contacts for existing membership",
			payload: &membershipservice.ListMemberMembershipKeyContactsPayload{
				MemberID: strPtr("member-1"),
				ID:       strPtr("membership-1"),
			},
			wantErr:   false,
			wantCount: 1,
		},
		{
			name: "list contacts for non-existing membership",
			payload: &membershipservice.ListMemberMembershipKeyContactsPayload{
				MemberID: strPtr("member-1"),
				ID:       strPtr("non-existing"),
			},
			wantErr: true,
		},
		{
			name: "list contacts for wrong member",
			payload: &membershipservice.ListMemberMembershipKeyContactsPayload{
				MemberID: strPtr("wrong-member"),
				ID:       strPtr("membership-1"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService()
			ctx := context.Background()

			res, err := svc.ListMemberMembershipKeyContacts(ctx, tt.payload)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, res)
			assert.Len(t, res.Contacts, tt.wantCount)
		})
	}
}

func TestReadyz(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	res, err := svc.Readyz(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("OK\n"), res)
}

func TestLivez(t *testing.T) {
	svc := newTestService()
	ctx := context.Background()

	res, err := svc.Livez(ctx)
	require.NoError(t, err)
	assert.Equal(t, []byte("OK\n"), res)
}

func TestParseFilters(t *testing.T) {
	tests := []struct {
		name    string
		filter  *string
		want    map[string]string
	}{
		{
			name:   "nil filter",
			filter: nil,
			want:   map[string]string{},
		},
		{
			name:   "empty filter",
			filter: strPtr(""),
			want:   map[string]string{},
		},
		{
			name:   "single filter",
			filter: strPtr("status=Active"),
			want:   map[string]string{"status": "Active"},
		},
		{
			name:   "multiple filters",
			filter: strPtr("status=Active;tier=Gold"),
			want:   map[string]string{"status": "Active", "tier": "Gold"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseFilters(tt.filter)
			assert.Equal(t, tt.want, result)
		})
	}
}

func strPtr(s string) *string {
	return &s
}
