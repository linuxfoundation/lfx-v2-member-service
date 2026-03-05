// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	membershipservice "github.com/linuxfoundation/lfx-v2-member-service/gen/membership_service"
	"github.com/linuxfoundation/lfx-v2-member-service/internal/domain/model"
)

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

	// Account
	result.Account = &membershipservice.AccountType{
		ID:   &m.Account.ID,
		Name: &m.Account.Name,
	}
	if m.Account.LogoURL != "" {
		result.Account.LogoURL = &m.Account.LogoURL
	}
	if m.Account.Website != "" {
		result.Account.Website = &m.Account.Website
	}

	// Contact
	result.Contact = &membershipservice.ContactType{
		ID:        &m.Contact.ID,
		FirstName: &m.Contact.FirstName,
		LastName:  &m.Contact.LastName,
		Email:     &m.Contact.Email,
	}
	if m.Contact.Title != "" {
		result.Contact.Title = &m.Contact.Title
	}

	// Product
	result.Product = &membershipservice.ProductType{
		ID:     &m.Product.ID,
		Name:   &m.Product.Name,
		Family: &m.Product.Family,
	}
	if m.Product.Type != "" {
		result.Product.Type = &m.Product.Type
	}

	// Project
	result.Project = &membershipservice.ProjectType{
		ID:   &m.Project.ID,
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

	// Contact
	result.Contact = &membershipservice.ContactType{
		ID:        &c.Contact.ID,
		FirstName: &c.Contact.FirstName,
		LastName:  &c.Contact.LastName,
		Email:     &c.Contact.Email,
	}
	if c.Contact.Title != "" {
		result.Contact.Title = &c.Contact.Title
	}

	// Project
	result.Project = &membershipservice.ProjectType{
		ID:   &c.Project.ID,
		Name: &c.Project.Name,
	}
	if c.Project.LogoURL != "" {
		result.Project.LogoURL = &c.Project.LogoURL
	}

	// Organization
	result.Organization = &membershipservice.OrganizationType{
		ID:   &c.Organization.ID,
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
