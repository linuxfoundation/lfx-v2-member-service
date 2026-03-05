// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/errors"
)

// MockMembershipRepository provides a mock implementation for testing
type MockMembershipRepository struct {
	memberships map[string]*model.Membership
	contacts    map[string]*model.KeyContact
	mu          sync.RWMutex
}

// NewMockMembershipRepository creates a new mock repository with sample data
func NewMockMembershipRepository() *MockMembershipRepository {
	now := time.Now()

	mock := &MockMembershipRepository{
		memberships: make(map[string]*model.Membership),
		contacts:    make(map[string]*model.KeyContact),
	}

	// Add sample membership
	sampleMembership := &model.Membership{
		UID:              "membership-1",
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
		},
		CreatedAt: now.Add(-24 * time.Hour),
		UpdatedAt: now,
	}
	mock.memberships[sampleMembership.UID] = sampleMembership

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

// IsReady always returns nil for mock
func (m *MockMembershipRepository) IsReady(ctx context.Context) error {
	return nil
}
