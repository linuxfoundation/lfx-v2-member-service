// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// KeyContact represents a key contact for a membership
type KeyContact struct {
	UID            string       `json:"uid"`
	MembershipUID  string       `json:"membership_uid"`
	Role           string       `json:"role"`
	Status         string       `json:"status"`
	BoardMember    bool         `json:"board_member"`
	PrimaryContact bool         `json:"primary_contact"`
	Contact        Contact      `json:"contact"`
	Project        Project      `json:"project"`
	Organization   Organization `json:"organization"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// Organization represents an organization
type Organization struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
	Website string `json:"website,omitempty"`
}
