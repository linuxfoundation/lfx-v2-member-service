// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/uid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConvertMemberToResponse(t *testing.T) {
	tests := []struct {
		name           string
		input          *model.Member
		wantNil        bool
		wantProjectID  string
		wantProductID  string
		wantAccountUID string
	}{
		{
			name:    "nil member returns nil response",
			input:   nil,
			wantNil: true,
		},
		{
			name: "member with membership summary redacts project ID and derives product UID",
			input: &model.Member{
				UID:     "member-uid-1",
				Name:    "Example Corp",
				LogoURL: "https://example.com/logo.png",
				Website: "https://example.com",
				MembershipSummary: &model.MembershipSummary{
					ActiveCount: 1,
					TotalCount:  2,
					Memberships: []model.MembershipSummaryItem{
						{
							UID:            "ms-uid-1",
							Name:           "Gold Membership",
							Status:         "Active",
							Year:           "2025",
							Tier:           "Gold",
							MembershipType: "Corporate",
							AutoRenew:      true,
							StartDate:      "2025-01-01",
							EndDate:        "2025-12-31",
							Product: model.Product{
								ID:   "01tABC000012345",
								Name: "Gold Tier Product",
							},
							Project: model.Project{
								ID:   "a0BABC000012345",
								Name: "Linux Foundation",
							},
						},
					},
				},
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
			wantNil:       false,
			wantProjectID: "",
			wantProductID: uid.ForMembershipTier("01tABC000012345"),
		},
		{
			name: "member without membership summary",
			input: &model.Member{
				UID:       "member-uid-2",
				Name:      "Minimal Corp",
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertMemberToResponse(tt.input)

			if tt.wantNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.input.UID, *result.UID)
			assert.Equal(t, tt.input.Name, *result.Name)

			if tt.input.MembershipSummary != nil && len(tt.input.MembershipSummary.Memberships) > 0 {
				require.NotNil(t, result.MembershipSummary)
				require.Len(t, result.MembershipSummary.Memberships, len(tt.input.MembershipSummary.Memberships))

				item := result.MembershipSummary.Memberships[0]

				// Project ID must be redacted.
				require.NotNil(t, item.Project)
				assert.Equal(t, tt.wantProjectID, *item.Project.ID, "Project.ID should be redacted to empty string")

				// Product ID must be a derived v2 UID, not the raw SFID.
				require.NotNil(t, item.Product)
				assert.Equal(t, tt.wantProductID, *item.Product.ID, "Product.ID should be a deterministic v2 UUID")
				assert.NotEqual(t, tt.input.MembershipSummary.Memberships[0].Product.ID, *item.Product.ID,
					"Product.ID must not be the raw SFID")
			}
		})
	}
}

func TestConvertMembershipToResponse(t *testing.T) {
	now := time.Now()
	accountSFID := "001ABC000012345"
	contactSFID := "003ABC000012345"
	productSFID := "01tABC000012345"
	projectSFID := "a0BABC000012345"

	tests := []struct {
		name    string
		input   *model.Membership
		wantNil bool
	}{
		{
			name:    "nil membership returns nil response",
			input:   nil,
			wantNil: true,
		},
		{
			name: "membership response redacts contact and project IDs and derives account and product UIDs",
			input: &model.Membership{
				UID:              "membership-uid-1",
				MemberUID:        "member-uid-1",
				Name:             "Gold Membership 2025",
				Status:           "Active",
				Year:             "2025",
				Tier:             "Gold",
				MembershipType:   "Corporate",
				AutoRenew:        true,
				RenewalType:      "Auto",
				Price:            50000.00,
				AnnualFullPrice:  50000.00,
				PaymentFrequency: "Annual",
				PaymentTerms:     "Net 30",
				AgreementDate:    "2025-01-15",
				PurchaseDate:     "2025-01-20",
				StartDate:        "2025-02-01",
				EndDate:          "2025-12-31",
				Account: model.Account{
					ID:      accountSFID,
					Name:    "Example Corp",
					LogoURL: "https://example.com/logo.png",
					Website: "https://example.com",
				},
				Contact: model.Contact{
					ID:        contactSFID,
					FirstName: "Jane",
					LastName:  "Doe",
					Email:     "jane.doe@example.com",
					Title:     "CTO",
				},
				Product: model.Product{
					ID:     productSFID,
					Name:   "Gold Tier Product",
					Family: "Membership",
					Type:   "Standard",
				},
				Project: model.Project{
					ID:      projectSFID,
					Name:    "Linux Foundation",
					LogoURL: "https://lf.io/logo.png",
					Slug:    "linux-foundation",
					Status:  "Active",
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertMembershipToResponse(tt.input)

			if tt.wantNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.input.UID, *result.UID)
			assert.Equal(t, tt.input.Name, *result.Name)

			// Contact.ID must always be redacted.
			require.NotNil(t, result.Contact)
			assert.Equal(t, "", *result.Contact.ID, "Contact.ID should be redacted to empty string")
			assert.Equal(t, tt.input.Contact.FirstName, *result.Contact.FirstName)
			assert.Equal(t, tt.input.Contact.LastName, *result.Contact.LastName)
			assert.Equal(t, tt.input.Contact.Email, *result.Contact.Email)

			// Project.ID must always be redacted.
			require.NotNil(t, result.Project)
			assert.Equal(t, "", *result.Project.ID, "Project.ID should be redacted to empty string")
			assert.Equal(t, tt.input.Project.Name, *result.Project.Name)

			// Account.ID must be a deterministic v2 UUID, not the raw SFID.
			require.NotNil(t, result.Account)
			expectedAccountUID := uid.ForMember(accountSFID)
			assert.Equal(t, expectedAccountUID, *result.Account.ID,
				"Account.ID should be a deterministic v2 UUID derived from the SFID")
			assert.NotEqual(t, accountSFID, *result.Account.ID,
				"Account.ID must not be the raw SFID")

			// Product.ID must be a deterministic v2 UUID, not the raw SFID.
			require.NotNil(t, result.Product)
			expectedProductUID := uid.ForMembershipTier(productSFID)
			assert.Equal(t, expectedProductUID, *result.Product.ID,
				"Product.ID should be a deterministic v2 UUID derived from the SFID")
			assert.NotEqual(t, productSFID, *result.Product.ID,
				"Product.ID must not be the raw SFID")
		})
	}
}

func TestConvertKeyContactToResponse(t *testing.T) {
	now := time.Now()
	contactSFID := "003ABC000067890"
	orgSFID := "001ABC000067890"
	projectSFID := "a0BABC000067890"

	tests := []struct {
		name    string
		input   *model.KeyContact
		wantNil bool
	}{
		{
			name:    "nil key contact returns nil response",
			input:   nil,
			wantNil: true,
		},
		{
			name: "key contact response redacts contact and project IDs and derives organization UID",
			input: &model.KeyContact{
				UID:            "kc-uid-1",
				MembershipUID:  "membership-uid-1",
				Role:           "Technical Contact",
				Status:         "Active",
				BoardMember:    false,
				PrimaryContact: true,
				Contact: model.Contact{
					ID:        contactSFID,
					FirstName: "John",
					LastName:  "Smith",
					Email:     "john.smith@example.com",
					Title:     "VP Engineering",
				},
				Project: model.Project{
					ID:      projectSFID,
					Name:    "Linux Foundation",
					LogoURL: "https://lf.io/logo.png",
				},
				Organization: model.Organization{
					ID:      orgSFID,
					Name:    "Example Corp",
					LogoURL: "https://example.com/logo.png",
					Website: "https://example.com",
				},
				CreatedAt: now,
				UpdatedAt: now,
			},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertKeyContactToResponse(tt.input)

			if tt.wantNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.input.UID, *result.UID)
			assert.Equal(t, tt.input.MembershipUID, *result.MembershipUID)

			// Contact.ID must always be redacted.
			require.NotNil(t, result.Contact)
			assert.Equal(t, "", *result.Contact.ID, "Contact.ID should be redacted to empty string")
			assert.Equal(t, tt.input.Contact.FirstName, *result.Contact.FirstName)
			assert.Equal(t, tt.input.Contact.LastName, *result.Contact.LastName)
			assert.Equal(t, tt.input.Contact.Email, *result.Contact.Email)

			// Project.ID must always be redacted.
			require.NotNil(t, result.Project)
			assert.Equal(t, "", *result.Project.ID, "Project.ID should be redacted to empty string")
			assert.Equal(t, tt.input.Project.Name, *result.Project.Name)

			// Organization.ID must be a deterministic v2 UUID, not the raw SFID.
			require.NotNil(t, result.Organization)
			expectedOrgUID := uid.ForMember(orgSFID)
			assert.Equal(t, expectedOrgUID, *result.Organization.ID,
				"Organization.ID should be a deterministic v2 UUID derived from the SFID")
			assert.NotEqual(t, orgSFID, *result.Organization.ID,
				"Organization.ID must not be the raw SFID")
		})
	}
}

func TestEmptySFIDGuardBehavior(t *testing.T) {
	// When the raw SFID is empty, the helper functions should return pointers
	// to empty strings — not synthetic UUIDs derived from an empty seed.
	tests := []struct {
		name string
		fn   func(string) *string
	}{
		{
			name: "memberUID with empty SFID returns pointer to empty string",
			fn:   memberUID,
		},
		{
			name: "membershipTierUID with empty SFID returns pointer to empty string",
			fn:   membershipTierUID,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.fn("")
			require.NotNil(t, result, "Result pointer must not be nil")
			assert.Equal(t, "", *result, "Empty SFID must produce an empty string, not a synthetic UUID")
		})
	}
}
