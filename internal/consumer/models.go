// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package consumer

import "time"

// MessageAction represents the action taken on an indexed document.
type MessageAction string

const (
	// MessageActionUpdated indicates the document was created or updated.
	MessageActionUpdated MessageAction = "updated"
	// MessageActionDeleted indicates the document was deleted.
	MessageActionDeleted MessageAction = "deleted"
)

// IndexerMessage is the wire format for messages published to the LFX indexer.
type IndexerMessage struct {
	Action        MessageAction          `json:"action"`
	Headers       map[string]string      `json:"headers"`
	Data          any                    `json:"data"`
	Tags          []string               `json:"tags"`
	IndexingConfig *IndexingConfig       `json:"indexing_config,omitempty"`
}

// IndexingConfig is the pre-computed indexing metadata included with each upsert message.
// When present, the indexer bypasses its server-side enrichers and uses these values directly.
type IndexingConfig struct {
	ObjectID             string   `json:"object_id"`
	AccessCheckObject    string   `json:"access_check_object"`
	AccessCheckRelation  string   `json:"access_check_relation"`
	HistoryCheckObject   string   `json:"history_check_object"`
	HistoryCheckRelation string   `json:"history_check_relation"`
	Public               *bool    `json:"public,omitempty"`
	SortName             string   `json:"sort_name,omitempty"`
	NameAndAliases       []string `json:"name_and_aliases,omitempty"`
	ParentRefs           []string `json:"parent_refs,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
	Fulltext             string   `json:"fulltext,omitempty"`
}

// ---- Salesforce source structs (decoded from v1-objects KV) ----

// SFProduct2 represents a Salesforce Product2 record from salesforce_b2b.
type SFProduct2 struct {
	ID               string  `json:"Id" msgpack:"Id"`
	Name             string  `json:"Name" msgpack:"Name"`
	Family           string  `json:"Family" msgpack:"Family"`
	Type             string  `json:"Type__c" msgpack:"Type__c"`
	ProjectSFID      string  `json:"Project__c" msgpack:"Project__c"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// SFAsset represents a Salesforce Asset record from salesforce_b2b (membership).
type SFAsset struct {
	ID               string  `json:"Id" msgpack:"Id"`
	Name             string  `json:"Name" msgpack:"Name"`
	AccountID        string  `json:"AccountId" msgpack:"AccountId"`
	Product2ID       string  `json:"Product2Id" msgpack:"Product2Id"`
	ProductFamily    string  `json:"Product_Family__c" msgpack:"Product_Family__c"`
	ProjectsSFID     string  `json:"Projects__c" msgpack:"Projects__c"`
	Status           string  `json:"Status" msgpack:"Status"`
	Year             string  `json:"Year__c" msgpack:"Year__c"`
	Tier             string  `json:"Tier__c" msgpack:"Tier__c"`
	RecordTypeID     string  `json:"RecordTypeId" msgpack:"RecordTypeId"`
	AutoRenew        bool    `json:"Auto_Renew__c" msgpack:"Auto_Renew__c"`
	RenewalType      string  `json:"Renewal_Type__c" msgpack:"Renewal_Type__c"`
	Price            float64 `json:"Price" msgpack:"Price"`
	AnnualFullPrice  float64 `json:"Annual_Full_Price__c" msgpack:"Annual_Full_Price__c"`
	PaymentFrequency string  `json:"PaymentFrequency__c" msgpack:"PaymentFrequency__c"`
	PaymentTerms     string  `json:"PaymentTerms__c" msgpack:"PaymentTerms__c"`
	AgreementDate    string  `json:"Agreement_Date__c" msgpack:"Agreement_Date__c"`
	PurchaseDate     string  `json:"PurchaseDate" msgpack:"PurchaseDate"`
	InstallDate      string  `json:"InstallDate" msgpack:"InstallDate"`
	UsageEndDate     string  `json:"UsageEndDate" msgpack:"UsageEndDate"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// SFAccount represents a Salesforce Account record from salesforce_b2b.
type SFAccount struct {
	ID               string  `json:"Id" msgpack:"Id"`
	Name             string  `json:"Name" msgpack:"Name"`
	LogoURL          string  `json:"Logo_URL__c" msgpack:"Logo_URL__c"`
	Website          string  `json:"Website" msgpack:"Website"`
	SFIDB2B          string  `json:"SFID_B2B__c" msgpack:"SFID_B2B__c"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// SFProject represents a Salesforce Project__c record from salesforce_b2b.
type SFProject struct {
	ID               string  `json:"Id" msgpack:"Id"`
	Name             string  `json:"Name" msgpack:"Name"`
	Slug             string  `json:"Slug__c" msgpack:"Slug__c"`
	Status           string  `json:"Status__c" msgpack:"Status__c"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// SFContact represents a Salesforce Contact record from salesforce_b2b.
type SFContact struct {
	ID               string  `json:"Id" msgpack:"Id"`
	FirstName        string  `json:"FirstName" msgpack:"FirstName"`
	LastName         string  `json:"LastName" msgpack:"LastName"`
	Title            string  `json:"Title" msgpack:"Title"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// SFAlternateEmail represents a Salesforce Alternate_Email__c record from salesforce_b2b.
type SFAlternateEmail struct {
	ID                    string  `json:"Id" msgpack:"Id"`
	ContactIDC            string  `json:"Contact_ID__c" msgpack:"Contact_ID__c"`
	AlternateEmailAddress string  `json:"Alternate_Email_Address__c" msgpack:"Alternate_Email_Address__c"`
	PrimaryEmail          bool    `json:"Primary_Email__c" msgpack:"Primary_Email__c"`
	Active                bool    `json:"Active__c" msgpack:"Active__c"`
	IsDeleted             bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt          *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
}

// SFProjectRole represents a Salesforce Project_Role__c record from salesforce_b2b.
type SFProjectRole struct {
	ID               string  `json:"Id" msgpack:"Id"`
	AssetSFID        string  `json:"Asset__c" msgpack:"Asset__c"`
	ContactSFID      string  `json:"Contact__c" msgpack:"Contact__c"`
	Role             string  `json:"Role__c" msgpack:"Role__c"`
	Status           string  `json:"Status__c" msgpack:"Status__c"`
	BoardMember      bool    `json:"Board_Member__c" msgpack:"Board_Member__c"`
	PrimaryContact   bool    `json:"Primary_Contact__c" msgpack:"Primary_Contact__c"`
	IsDeleted        bool    `json:"IsDeleted" msgpack:"IsDeleted"`
	SDCDeletedAt     *string `json:"_sdc_deleted_at" msgpack:"_sdc_deleted_at"`
	CreatedDate      string  `json:"CreatedDate" msgpack:"CreatedDate"`
	LastModifiedDate string  `json:"LastModifiedDate" msgpack:"LastModifiedDate"`
}

// ---- Indexed document structs (published to LFX indexer) ----

// IndexedProjectMembershipTier is the indexer payload for a project_membership_tier document.
type IndexedProjectMembershipTier struct {
	UID         string    `json:"uid"`
	Name        string    `json:"name"`
	Aliases     []string  `json:"aliases,omitempty"`
	Family      string    `json:"family,omitempty"`
	Type        string    `json:"type,omitempty"`
	ProjectUID  string    `json:"project_uid"`
	ProjectName string    `json:"project_name,omitempty"`
	ProjectSlug string    `json:"project_slug,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IndexedProjectMembership is the indexer payload for a project_membership document.
type IndexedProjectMembership struct {
	UID             string    `json:"uid"`
	Name            string    `json:"name"`
	Aliases         []string  `json:"aliases,omitempty"`
	Status          string    `json:"status,omitempty"`
	Year            string    `json:"year,omitempty"`
	Tier            string    `json:"tier,omitempty"`
	AnnualFullPrice float64   `json:"annual_full_price,omitempty"`
	AgreementDate   string    `json:"agreement_date,omitempty"`
	PurchaseDate    string    `json:"purchase_date,omitempty"`
	StartDate       string    `json:"start_date,omitempty"`
	EndDate         string    `json:"end_date,omitempty"`
	CompanyName     string    `json:"company_name,omitempty"`
	CompanyLogoURL  string    `json:"company_logo_url,omitempty"`
	CompanyWebsite  string    `json:"company_website,omitempty"`
	ProductName     string    `json:"product_name,omitempty"`
	ProductFamily   string    `json:"product_family,omitempty"`
	ProductType     string    `json:"product_type,omitempty"`
	ProductUID      string    `json:"product_uid,omitempty"`
	ProjectUID      string    `json:"project_uid"`
	ProjectName     string    `json:"project_name,omitempty"`
	ProjectSlug     string    `json:"project_slug,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// IndexedKeyContact is the indexer payload for a key_contact document.
type IndexedKeyContact struct {
	UID            string    `json:"uid"`
	MembershipUID  string    `json:"membership_uid"`
	ProductUID     string    `json:"product_uid,omitempty"`
	Role           string    `json:"role,omitempty"`
	Status         string    `json:"status,omitempty"`
	BoardMember    bool      `json:"board_member"`
	PrimaryContact bool      `json:"primary_contact"`
	FirstName      string    `json:"first_name,omitempty"`
	LastName       string    `json:"last_name,omitempty"`
	Title          string    `json:"title,omitempty"`
	Email          string    `json:"email,omitempty"`
	CompanyName    string    `json:"company_name,omitempty"`
	CompanyLogoURL string    `json:"company_logo_url,omitempty"`
	CompanyWebsite string    `json:"company_website,omitempty"`
	ProjectUID     string    `json:"project_uid"`
	ProjectName    string    `json:"project_name,omitempty"`
	ProjectSlug    string    `json:"project_slug,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// DeleteRequest is sent to the indexer when a document should be removed.
type DeleteRequest struct {
	UID string `json:"uid"`
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool { return &b }

// projectInfo caches resolved project information for a given Salesforce project SFID.
// uid is the v2 project UID (resolved via NATS RPC to v1-sync-helper). name and slug
// are the v1 B2B values from salesforce_b2b-Project__c, which are expected to align
// with v2 project data and are safe to denormalize in v2 context.
type projectInfo struct {
	uid  string
	name string
	slug string
}
