// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-member-service/pkg/uid"
)

// emptyString is a convenience pointer to an empty string, used for ID fields that
// must be redacted from API responses (e.g. Contact.ID has no v2 identity equivalent).
var emptyString = func() *string { s := ""; return &s }()

// memberUID derives the v2 member UID from a raw Salesforce Account SFID stored in the
// model. The stored Account.ID is a raw SFID; callers must not pass a pre-converted UID.
func memberUID(accountSFID string) *string {
	s := uid.ForMember(accountSFID)
	return &s
}

// membershipTierUID derives the v2 membership tier UID from a raw Salesforce Product2
// SFID stored in the model. The stored Product.ID is a raw SFID; callers must not pass
// a pre-converted UID.
func membershipTierUID(product2SFID string) *string {
	s := uid.ForMembershipTier(product2SFID)
	return &s
}

// convertMemberToResponse converts a domain Member to a Goa response
func convertMemberToResponse(m *model.Member) *membershipservice.MemberResponse {
	if m == nil {
		return nil
	}

	result := &membershipservice.MemberResponse{
		UID:  &m.UID,
		Name: &m.Name,
	}

	if m.LogoURL != "" {
		result.LogoURL = &m.LogoURL
	}
	if m.Website != "" {
		result.Website = &m.Website
	}

	// Membership summary
	if m.MembershipSummary != nil {
		summary := &membershipservice.MembershipSummaryType{
			ActiveCount: &m.MembershipSummary.ActiveCount,
			TotalCount:  &m.MembershipSummary.TotalCount,
		}

		items := make([]*membershipservice.MembershipSummaryItemType, 0, len(m.MembershipSummary.Memberships))
		for i := range m.MembershipSummary.Memberships {
			ms := &m.MembershipSummary.Memberships[i]
			item := &membershipservice.MembershipSummaryItemType{
				UID:            &ms.UID,
				Name:           &ms.Name,
				Status:         &ms.Status,
				MembershipType: &ms.MembershipType,
				AutoRenew:      &ms.AutoRenew,
			}
			if ms.Year != "" {
				item.Year = &ms.Year
			}
			if ms.Tier != "" {
				item.Tier = &ms.Tier
			}
			if ms.StartDate != "" {
				item.StartDate = &ms.StartDate
			}
			if ms.EndDate != "" {
				item.EndDate = &ms.EndDate
			}
			item.Product = &membershipservice.ProductType{
				// Product.ID: derive v2 membership tier UID from the raw Product2 SFID
				// stored in the model. The stored value is always a raw SFID at this point.
				ID:   membershipTierUID(ms.Product.ID),
				Name: &ms.Product.Name,
			}
			item.Project = &membershipservice.ProjectType{
				// Project.ID stores a raw Salesforce SFID internally for KV lookup key
				// construction and must not be exposed in the API response.
				ID:   emptyString,
				Name: &ms.Project.Name,
			}
			items = append(items, item)
		}
		summary.Memberships = items
		result.MembershipSummary = summary
	}

	// Timestamps
	if !m.CreatedAt.IsZero() {
		createdAt := m.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.CreatedAt = &createdAt
	}
	if !m.UpdatedAt.IsZero() {
		updatedAt := m.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.UpdatedAt = &updatedAt
	}

	return result
}

// convertMembershipToResponse converts a domain Membership to a Goa response
func convertMembershipToResponse(m *model.Membership) *membershipservice.MembershipResponse {
	if m == nil {
		return nil
	}

	result := &membershipservice.MembershipResponse{
		UID:             &m.UID,
		Name:            &m.Name,
		Status:          &m.Status,
		MembershipType:  &m.MembershipType,
		AutoRenew:       &m.AutoRenew,
		Price:           &m.Price,
		AnnualFullPrice: &m.AnnualFullPrice,
	}

	if m.Year != "" {
		result.Year = &m.Year
	}
	if m.Tier != "" {
		result.Tier = &m.Tier
	}
	if m.RenewalType != "" {
		result.RenewalType = &m.RenewalType
	}
	if m.PaymentFrequency != "" {
		result.PaymentFrequency = &m.PaymentFrequency
	}
	if m.PaymentTerms != "" {
		result.PaymentTerms = &m.PaymentTerms
	}
	if m.AgreementDate != "" {
		result.AgreementDate = &m.AgreementDate
	}
	if m.PurchaseDate != "" {
		result.PurchaseDate = &m.PurchaseDate
	}
	if m.StartDate != "" {
		result.StartDate = &m.StartDate
	}
	if m.EndDate != "" {
		result.EndDate = &m.EndDate
	}

	// Account — derive v2 member UID from the raw Account SFID stored in the model.
	result.Account = &membershipservice.AccountType{
		ID:   memberUID(m.Account.ID),
		Name: &m.Account.Name,
	}
	if m.Account.LogoURL != "" {
		result.Account.LogoURL = &m.Account.LogoURL
	}
	if m.Account.Website != "" {
		result.Account.Website = &m.Account.Website
	}

	// Contact — ID is intentionally empty: there is no v2 User identity available at
	// this stage. Identity is conveyed by name and email only.
	result.Contact = &membershipservice.ContactType{
		ID:        emptyString,
		FirstName: &m.Contact.FirstName,
		LastName:  &m.Contact.LastName,
		Email:     &m.Contact.Email,
	}
	if m.Contact.Title != "" {
		result.Contact.Title = &m.Contact.Title
	}

	// Product — derive v2 membership tier UID from the raw Product2 SFID stored in the
	// model. The stored value is always a raw SFID at this point.
	result.Product = &membershipservice.ProductType{
		ID:     membershipTierUID(m.Product.ID),
		Name:   &m.Product.Name,
		Family: &m.Product.Family,
	}
	if m.Product.Type != "" {
		result.Product.Type = &m.Product.Type
	}

	// Project — ID stores a raw Salesforce SFID internally for KV lookup key construction
	// and must not be exposed in the API response.
	result.Project = &membershipservice.ProjectType{
		ID:   emptyString,
		Name: &m.Project.Name,
	}
	if m.Project.LogoURL != "" {
		result.Project.LogoURL = &m.Project.LogoURL
	}
	if m.Project.Slug != "" {
		result.Project.Slug = &m.Project.Slug
	}
	if m.Project.Status != "" {
		result.Project.Status = &m.Project.Status
	}

	// Timestamps
	if !m.CreatedAt.IsZero() {
		createdAt := m.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.CreatedAt = &createdAt
	}
	if !m.UpdatedAt.IsZero() {
		updatedAt := m.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.UpdatedAt = &updatedAt
	}

	return result
}

// convertKeyContactToResponse converts a domain KeyContact to a Goa response
func convertKeyContactToResponse(c *model.KeyContact) *membershipservice.KeyContactResponse {
	if c == nil {
		return nil
	}

	result := &membershipservice.KeyContactResponse{
		UID:            &c.UID,
		MembershipUID:  &c.MembershipUID,
		Role:           &c.Role,
		Status:         &c.Status,
		BoardMember:    &c.BoardMember,
		PrimaryContact: &c.PrimaryContact,
	}

	// Contact — ID is intentionally empty: there is no v2 User identity available at
	// this stage. Identity is conveyed by name and email only.
	result.Contact = &membershipservice.ContactType{
		ID:        emptyString,
		FirstName: &c.Contact.FirstName,
		LastName:  &c.Contact.LastName,
		Email:     &c.Contact.Email,
	}
	if c.Contact.Title != "" {
		result.Contact.Title = &c.Contact.Title
	}

	// Project — ID stores a raw Salesforce SFID internally for KV lookup key construction
	// and must not be exposed in the API response.
	result.Project = &membershipservice.ProjectType{
		ID:   emptyString,
		Name: &c.Project.Name,
	}
	if c.Project.LogoURL != "" {
		result.Project.LogoURL = &c.Project.LogoURL
	}

	// Organization — derive v2 member UID from the raw Account SFID stored in the model.
	result.Organization = &membershipservice.OrganizationType{
		ID:   memberUID(c.Organization.ID),
		Name: &c.Organization.Name,
	}
	if c.Organization.LogoURL != "" {
		result.Organization.LogoURL = &c.Organization.LogoURL
	}
	if c.Organization.Website != "" {
		result.Organization.Website = &c.Organization.Website
	}

	// Timestamps
	if !c.CreatedAt.IsZero() {
		createdAt := c.CreatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.CreatedAt = &createdAt
	}
	if !c.UpdatedAt.IsZero() {
		updatedAt := c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		result.UpdatedAt = &updatedAt
	}

	return result
}
