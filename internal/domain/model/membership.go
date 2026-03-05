// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// Membership represents a membership entity
type Membership struct {
	UID              string    `json:"uid"`
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	Year             string    `json:"year,omitempty"`
	Tier             string    `json:"tier,omitempty"`
	MembershipType   string    `json:"membership_type"`
	AutoRenew        bool      `json:"auto_renew"`
	RenewalType      string    `json:"renewal_type,omitempty"`
	Price            float64   `json:"price,omitempty"`
	AnnualFullPrice  float64   `json:"annual_full_price,omitempty"`
	PaymentFrequency string    `json:"payment_frequency,omitempty"`
	PaymentTerms     string    `json:"payment_terms,omitempty"`
	AgreementDate    string    `json:"agreement_date,omitempty"`
	PurchaseDate     string    `json:"purchase_date,omitempty"`
	StartDate        string    `json:"start_date,omitempty"`
	EndDate          string    `json:"end_date,omitempty"`
	Account          Account   `json:"account"`
	Contact          Contact   `json:"contact"`
	Product          Product   `json:"product"`
	Project          Project   `json:"project"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Account represents an organization account
type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
	Website string `json:"website,omitempty"`
}

// Contact represents a person contact
type Contact struct {
	ID        string `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
	Title     string `json:"title,omitempty"`
}

// Product represents a product
type Product struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Family string `json:"family"`
	Type   string `json:"type,omitempty"`
}

// Project represents a project
type Project struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	LogoURL string `json:"logo_url,omitempty"`
	Slug    string `json:"slug,omitempty"`
	Status  string `json:"status,omitempty"`
}
