// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// MockMembershipRepository provides a mock implementation for testing
type MockMembershipRepository struct {
	members     map[string]*model.Member
	memberships map[string]*model.Membership
	contacts    map[string]*model.KeyContact
	mu          sync.RWMutex
}

// NewMockMembershipRepository creates a new mock repository with sample data
func NewMockMembershipRepository() *MockMembershipRepository {
	now := time.Now()

	mock := &MockMembershipRepository{
		members:     make(map[string]*model.Member),
		memberships: make(map[string]*model.Membership),
		contacts:    make(map[string]*model.KeyContact),
	}

	// Add sample membership
	sampleMembership := &model.Membership{
		UID:              "membership-1",
		MemberUID:        "member-1",
		Name:             "Gold Membership - Example Corp",
		Status:           "Active",
		Year:             "2025",
		Tier:             "Gold",
		MembershipType:   "Corporate",
		AutoRenew:        true,
		RenewalType:      "Annual",
		Price:            50000,
		AnnualFullPrice:  50000,
		PaymentFrequency: "Annual",
		StartDate:        "2025-01-01T00:00:00Z",
		EndDate:          "2025-12-31T23:59:59Z",
		Account: model.Account{
			ID:   "account-1",
			Name: "Example Corp",
		},
		Contact: model.Contact{
			ID:        "contact-1",
			FirstName: "John",
			LastName:  "Doe",
			Email:     "john.doe@example.com",
		},
		Product: model.Product{
			ID:     "product-1",
			Name:   "Gold Membership",
			Family: "Membership",
		},
		Project: model.Project{
			ID:   "project-1",
			Name: "Linux Foundation",
			Slug: "linux-foundation",
		},
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now,
	}
	mock.memberships[sampleMembership.UID] = sampleMembership

	// Add sample member
	sampleMember := &model.Member{
		UID:     "member-1",
		Name:    "Example Corp",
		LogoURL: "https://example.com/logo.png",
		Website: "https://example.com",
		SFIDs:   []string{"account-1"},
		MembershipSummary: &model.MembershipSummary{
			ActiveCount: 1,
			TotalCount:  1,
			Memberships: []model.MembershipSummaryItem{
				{
					UID:            "membership-1",
					Name:           "Gold Membership - Example Corp",
					Status:         "Active",
					Year:           "2025",
					Tier:           "Gold",
					MembershipType: "Corporate",
					AutoRenew:      true,
					StartDate:      "2025-01-01T00:00:00Z",
					EndDate:        "2025-12-31T23:59:59Z",
					Product: model.Product{
						ID:   "product-1",
						Name: "Gold Membership",
					},
					Project: model.Project{
						ID:   "project-1",
						Name: "Linux Foundation",
						Slug: "linux-foundation",
					},
				},
			},
		},
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now,
	}
	mock.members[sampleMember.UID] = sampleMember

	// Add sample contact
	sampleContact := &model.KeyContact{
		UID:            "contact-role-1",
		MembershipUID:  "membership-1",
		Role:           "Primary Contact",
		Status:         "Active",
		BoardMember:    false,
		PrimaryContact: true,
		Contact: model.Contact{
			ID:        "contact-1",
			FirstName: "John",
			LastName:  "Doe",
			Email:     "john.doe@example.com",
			Title:     "CTO",
		},
		Project: model.Project{
			ID:   "project-1",
			Name: "Linux Foundation",
			Slug: "linux-foundation",
		},
		Organization: model.Organization{
			ID:   "account-1",
			Name: "Example Corp",
		},
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now,
	}
	mock.contacts[sampleContact.UID] = sampleContact

	return mock
}

// GetMember retrieves a member by UID
func (m *MockMembershipRepository) GetMember(ctx context.Context, uid string) (*model.Member, uint64, error) {
	slog.DebugContext(ctx, "mock: getting member", "uid", uid)

	m.mu.RLock()
	defer m.mu.RUnlock()

	member, exists := m.members[uid]
	if !exists {
		return nil, 0, errors.NewNotFound(fmt.Sprintf("member with UID %s not found", uid))
	}

	return member, 1, nil
}

// ListMembers retrieves members with pagination, filtering, and search
func (m *MockMembershipRepository) ListMembers(ctx context.Context, params model.ListParams) ([]*model.Member, int, error) {
	slog.DebugContext(ctx, "mock: listing members")

	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []*model.Member
	for _, member := range m.members {
		if matchesMockMemberFilters(member, params.Filters, params.Search) {
			all = append(all, member)
		}
	}

	totalSize := len(all)

	start := params.Offset
	if start > totalSize {
		start = totalSize
	}
	end := start + params.PageSize
	if end > totalSize {
		end = totalSize
	}

	return all[start:end], totalSize, nil
}

// GetMembershipForMember retrieves a membership and verifies it belongs to the member
func (m *MockMembershipRepository) GetMembershipForMember(ctx context.Context, memberUID, membershipUID string) (*model.Membership, uint64, error) {
	slog.DebugContext(ctx, "mock: getting membership for member", "member_uid", memberUID, "membership_uid", membershipUID)

	m.mu.RLock()
	defer m.mu.RUnlock()

	membership, exists := m.memberships[membershipUID]
	if !exists {
		return nil, 0, errors.NewNotFound(fmt.Sprintf("membership with UID %s not found", membershipUID))
	}

	if membership.MemberUID != memberUID {
		return nil, 0, errors.NewNotFound(fmt.Sprintf("membership %s not found for member %s", membershipUID, memberUID))
	}

	return membership, 1, nil
}

// ListKeyContactsForMembership retrieves key contacts for a membership under a member
func (m *MockMembershipRepository) ListKeyContactsForMembership(ctx context.Context, memberUID, membershipUID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "mock: listing key contacts for membership", "member_uid", memberUID, "membership_uid", membershipUID)

	// Verify membership belongs to member
	_, _, err := m.GetMembershipForMember(ctx, memberUID, membershipUID)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var contacts []*model.KeyContact
	for _, contact := range m.contacts {
		if contact.MembershipUID == membershipUID {
			contacts = append(contacts, contact)
		}
	}

	return contacts, nil
}

// GetMembership retrieves a membership by UID
func (m *MockMembershipRepository) GetMembership(ctx context.Context, uid string) (*model.Membership, uint64, error) {
	slog.DebugContext(ctx, "mock: getting membership", "uid", uid)

	m.mu.RLock()
	defer m.mu.RUnlock()

	membership, exists := m.memberships[uid]
	if !exists {
		return nil, 0, errors.NewNotFound(fmt.Sprintf("membership with UID %s not found", uid))
	}

	return membership, 1, nil
}

// ListMemberships retrieves memberships with pagination and filtering
func (m *MockMembershipRepository) ListMemberships(ctx context.Context, params model.ListParams) ([]*model.Membership, int, error) {
	slog.DebugContext(ctx, "mock: listing memberships")

	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []*model.Membership
	for _, membership := range m.memberships {
		all = append(all, membership)
	}

	totalSize := len(all)

	start := params.Offset
	if start > totalSize {
		start = totalSize
	}
	end := start + params.PageSize
	if end > totalSize {
		end = totalSize
	}

	return all[start:end], totalSize, nil
}

// ListKeyContacts retrieves key contacts for a membership
func (m *MockMembershipRepository) ListKeyContacts(ctx context.Context, membershipUID string) ([]*model.KeyContact, error) {
	slog.DebugContext(ctx, "mock: listing key contacts", "membership_uid", membershipUID)

	m.mu.RLock()
	defer m.mu.RUnlock()

	var contacts []*model.KeyContact
	for _, contact := range m.contacts {
		if contact.MembershipUID == membershipUID {
			contacts = append(contacts, contact)
		}
	}

	return contacts, nil
}

// WriteProjectAliasLookups is a no-op for mock
func (m *MockMembershipRepository) WriteProjectAliasLookups(_ context.Context, _, _, _ string) error {
	return nil
}

// IsReady always returns nil for mock
func (m *MockMembershipRepository) IsReady(ctx context.Context) error {
	return nil
}

// matchesMockMemberFilters checks if a member matches filters and search for mock
func matchesMockMemberFilters(member *model.Member, filters map[string]string, search string) bool {
	if search != "" {
		searchLower := strings.ToLower(search)
		found := strings.Contains(strings.ToLower(member.Name), searchLower)

		if !found && member.MembershipSummary != nil {
			for _, ms := range member.MembershipSummary.Memberships {
				if strings.Contains(strings.ToLower(ms.Project.Name), searchLower) ||
					strings.Contains(strings.ToLower(ms.Project.Slug), searchLower) ||
					strings.Contains(strings.ToLower(ms.Tier), searchLower) {
					found = true
					break
				}
			}
		}

		if !found {
			return false
		}
	}

	for key, value := range filters {
		switch strings.ToLower(key) {
		case "name":
			if !strings.Contains(strings.ToLower(member.Name), strings.ToLower(value)) {
				return false
			}
		case "project_slug":
			matched := false
			if member.MembershipSummary != nil {
				for _, ms := range member.MembershipSummary.Memberships {
					if strings.EqualFold(ms.Project.Slug, value) {
						matched = true
						break
					}
				}
			}
			if !matched {
				return false
			}
		}
	}

	return true
}
