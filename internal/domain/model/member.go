// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// Member represents a member (account/organization) entity
type Member struct {
	UID               string             `json:"uid"`
	Name              string             `json:"name"`
	LogoURL           string             `json:"logo_url,omitempty"`
	Website           string             `json:"website,omitempty"`
	SFIDs             []string           `json:"sfids"`
	MembershipSummary *MembershipSummary `json:"membership_summary,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// MembershipSummary contains aggregated membership information for a member
type MembershipSummary struct {
	ActiveCount int                     `json:"active_count"`
	TotalCount  int                     `json:"total_count"`
	Memberships []MembershipSummaryItem `json:"memberships"`
}

// MembershipSummaryItem contains summary details for a single membership
type MembershipSummaryItem struct {
	UID            string  `json:"uid"`
	Name           string  `json:"name"`
	Status         string  `json:"status"`
	Year           string  `json:"year,omitempty"`
	Tier           string  `json:"tier,omitempty"`
	MembershipType string  `json:"membership_type"`
	AutoRenew      bool    `json:"auto_renew"`
	StartDate      string  `json:"start_date,omitempty"`
	EndDate        string  `json:"end_date,omitempty"`
	Product        Product `json:"product"`
	Project        Project `json:"project"`
}
