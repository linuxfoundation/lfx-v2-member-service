// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"database/sql"
	"time"
)

// SQLMembership represents the SQL scan struct for membership queries
type SQLMembership struct {
	ID               string         `db:"id"`
	Name             string         `db:"name"`
	ContactID        sql.NullString `db:"contactid"`
	Status           sql.NullString `db:"status"`
	Year             string         `db:"year"`
	Tier             string         `db:"tier"`
	MembershipType   sql.NullString `db:"membershiptype"`
	AutoRenew        bool           `db:"autorenew"`
	RenewalType      string         `db:"renewaltype"`
	Price            float64        `db:"price"`
	AnnualFullPrice  float64        `db:"annualfullprice"`
	PaymentFrequency string         `db:"paymentfrequency"`
	PaymentTerms     string         `db:"paymentterms"`
	AgreementDate    sql.NullTime   `db:"agreementdate"`
	PurchaseDate     sql.NullTime   `db:"purchasedate"`
	StartDate        sql.NullTime   `db:"startdate"`
	EndDate          sql.NullTime   `db:"enddate"`
	AccountID        string         `db:"accountid"`
	FirstName        string         `db:"firstname"`
	LastName         string         `db:"lastname"`
	Email            string         `db:"email"`
	ProductName      string         `db:"productname"`
	ProductFamily    string         `db:"productfamily"`
	ProductType      string         `db:"producttype"`
	ProductID        string         `db:"productid"`
	ProjectID        string         `db:"projectid"`
	CreatedDate      sql.NullTime   `db:"createddate"`
	LastModifiedAt   sql.NullTime   `db:"lastmodifiedat"`
	AccountName      sql.NullString `db:"accountname"`
	AccountLogoURL   sql.NullString `db:"accountlogourl"`
	AccountWebsite   sql.NullString `db:"accountwebsite"`
	ProjectName      sql.NullString `db:"projectname"`
	ProjectLogoURL   sql.NullString `db:"projectlogourl"`
	ProjectSlug      sql.NullString `db:"projectslug"`
	ProjectStatus    sql.NullString `db:"projectstatus"`
	ContactTitle     sql.NullString `db:"contacttitle"`
}

// SQLKeyContact represents the SQL scan struct for key contact queries
type SQLKeyContact struct {
	ID             string         `db:"id"`
	MembershipID   string         `db:"membershipid"`
	Role           sql.NullString `db:"role"`
	RoleStatus     sql.NullString `db:"rolestatus"`
	BoardMember    sql.NullBool   `db:"boardmember"`
	PrimaryContact sql.NullBool   `db:"primarycontact"`
	ContactID      sql.NullString `db:"contactid"`
	FirstName      string         `db:"firstname"`
	LastName       string         `db:"lastname"`
	Email          string         `db:"email"`
	Title          string         `db:"title"`
	OrgID          string         `db:"orgid"`
	OrgName        string         `db:"orgname"`
	OrgLogoURL     string         `db:"orglogourl"`
	OrgWebsite     string         `db:"orgwebsite"`
	ProjectID      string         `db:"projectid"`
	ProjectName    string         `db:"projectname"`
	ProjectLogoURL string         `db:"projectlogourl"`
	CreatedDate    sql.NullTime   `db:"createddate"`
	UpdatedAt      sql.NullTime   `db:"updatedat"`
}

// SQLMember represents the SQL scan struct for member (account) queries
type SQLMember struct {
	SFID             string       `db:"sfid"`
	Name             string       `db:"name"`
	LogoURL          string       `db:"logourl"`
	Website          string       `db:"website"`
	CreatedDate      sql.NullTime `db:"createddate"`
	LastModifiedDate sql.NullTime `db:"lastmodifieddate"`
}

// timeToString converts a sql.NullTime to a string in RFC3339 format
func timeToString(t sql.NullTime) string {
	if t.Valid {
		return t.Time.Format(time.RFC3339)
	}
	return ""
}

// nullStringValue returns the string value of a sql.NullString or empty string
func nullStringValue(ns sql.NullString) string {
	if ns.Valid {
		return ns.String
	}
	return ""
}
