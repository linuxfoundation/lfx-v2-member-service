// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package salesforce

// soqlAsset represents a Salesforce Asset record returned by a SOQL query.
// Relationship fields (Account, Product2, Project) are populated via SOQL
// relationship sub-selects. The Contact sub-select is intentionally omitted —
// the billing contact on an Asset is not a key contact; key contacts are fetched
// separately via Project_Role__c queries.
//
// Both salesforce and json tags are present so that the struct can be decoded
// by either the go-salesforce library (client.Query, which uses mapstructure with
// the salesforce tag) or by json.Unmarshal (used in QueryPage, which decodes raw
// Salesforce REST JSON directly).
type soqlAsset struct {
	ID               string            `salesforce:"Id"               json:"Id"`
	Name             string            `salesforce:"Name"             json:"Name"`
	Status           *string           `salesforce:"Status"           json:"Status"`
	AccountID        string            `salesforce:"AccountId"        json:"AccountId"`
	Product2ID       string            `salesforce:"Product2Id"       json:"Product2Id"`
	IsDeleted        bool              `salesforce:"IsDeleted"        json:"IsDeleted"`
	Year             *string           `salesforce:"Year__c"          json:"Year__c"`
	Tier             *string           `salesforce:"Tier__c"          json:"Tier__c"`
	AutoRenew        bool              `salesforce:"Auto_Renew__c"    json:"Auto_Renew__c"`
	RenewalType      *string           `salesforce:"Renewal_Type__c"  json:"Renewal_Type__c"`
	Price            float64           `salesforce:"Price"            json:"Price"`
	AnnualFullPrice  float64           `salesforce:"Annual_Full_Price__c" json:"Annual_Full_Price__c"`
	PaymentFrequency *string           `salesforce:"PaymentFrequency__c"  json:"PaymentFrequency__c"`
	PaymentTerms     *string           `salesforce:"PaymentTerms__c"  json:"PaymentTerms__c"`
	AgreementDate    *string           `salesforce:"Agreement_Date__c" json:"Agreement_Date__c"`
	PurchaseDate     *string           `salesforce:"PurchaseDate"     json:"PurchaseDate"`
	InstallDate      *string           `salesforce:"InstallDate"      json:"InstallDate"`
	UsageEndDate     *string           `salesforce:"UsageEndDate"     json:"UsageEndDate"`
	ProjectsID       *string           `salesforce:"Projects__c"      json:"Projects__c"`
	CreatedDate      string            `salesforce:"CreatedDate"      json:"CreatedDate"`
	LastModifiedDate string            `salesforce:"LastModifiedDate" json:"LastModifiedDate"`
	Account          *soqlAssetAccount `salesforce:"Account"          json:"Account"`
	Product2         *soqlAssetProduct `salesforce:"Product2"         json:"Product2"`
	Project          *soqlAssetProject `salesforce:"Projects__r"      json:"Projects__r"`
}

// soqlAssetAccount is the inline Account relationship on an Asset query.
type soqlAssetAccount struct {
	ID      string  `salesforce:"Id"          json:"Id"`
	Name    string  `salesforce:"Name"        json:"Name"`
	LogoURL *string `salesforce:"Logo_URL__c" json:"Logo_URL__c"`
	Website *string `salesforce:"Website"     json:"Website"`
}

// soqlAssetProduct is the inline Product2 relationship on an Asset query.
type soqlAssetProduct struct {
	ID     string  `salesforce:"Id"       json:"Id"`
	Name   string  `salesforce:"Name"     json:"Name"`
	Family *string `salesforce:"Family"   json:"Family"`
	Type   *string `salesforce:"Type__c"  json:"Type__c"`
}

// soqlAssetProject is the inline Project__c relationship on an Asset query.
type soqlAssetProject struct {
	ID      string  `salesforce:"Id"              json:"Id"`
	Name    string  `salesforce:"Name"            json:"Name"`
	LogoURL *string `salesforce:"Project_Logo__c" json:"Project_Logo__c"`
	Slug    *string `salesforce:"Slug__c"         json:"Slug__c"`
	Status  *string `salesforce:"Status__c"       json:"Status__c"`
}

// soqlProjectRole represents a Salesforce Project_Role__c record returned by a
// SOQL query. This is the key contact object linking a Contact, Account, Asset,
// and Project with a role designation.
type soqlProjectRole struct {
	ID             string                  `salesforce:"Id"              json:"Id"`
	AssetID        string                  `salesforce:"Asset__c"        json:"Asset__c"`
	ContactID      *string                 `salesforce:"Contact__c"      json:"Contact__c"`
	Role           *string                 `salesforce:"Role__c"         json:"Role__c"`
	Status         *string                 `salesforce:"Status__c"       json:"Status__c"`
	BoardMember    bool                    `salesforce:"BoardMember__c"  json:"BoardMember__c"`
	PrimaryContact bool                    `salesforce:"PrimaryContact__c" json:"PrimaryContact__c"`
	IsDeleted      bool                    `salesforce:"IsDeleted"       json:"IsDeleted"`
	CreatedDate    string                  `salesforce:"CreatedDate"     json:"CreatedDate"`
	SystemModstamp string                  `salesforce:"SystemModstamp"  json:"SystemModstamp"`
	Contact        *soqlProjectRoleContact `salesforce:"Contact__r"      json:"Contact__r"`
	Asset          *soqlProjectRoleAsset   `salesforce:"Asset__r"        json:"Asset__r"`
}

// soqlProjectRoleContact is the inline Contact relationship on a Project_Role__c
// query. Only identity and personal attributes are fetched here; company
// (Account) data is sourced from the Asset relationship instead, so it is
// consistent with the associated ProjectMembership record.
type soqlProjectRoleContact struct {
	ID        string  `salesforce:"Id"        json:"Id"`
	FirstName *string `salesforce:"FirstName" json:"FirstName"`
	LastName  *string `salesforce:"LastName"  json:"LastName"`
	Title     *string `salesforce:"Title"     json:"Title"`
	Email     *string `salesforce:"Email"     json:"Email"`
}

// soqlProjectRoleAssetAccount is the inline Account on the Asset relationship in
// a Project_Role__c query (Asset__r.Account). Company data for key contacts is
// sourced here rather than from Contact__r.Account so it matches the membership.
type soqlProjectRoleAssetAccount struct {
	ID      string  `salesforce:"Id"          json:"Id"`
	Name    string  `salesforce:"Name"        json:"Name"`
	LogoURL *string `salesforce:"Logo_URL__c" json:"Logo_URL__c"`
	Website *string `salesforce:"Website"     json:"Website"`
}

// soqlProjectRoleAsset is the inline Asset relationship on a Project_Role__c
// query. Includes the Asset's Account and Project relationships so that company
// and project data can be denormalized onto the key contact record.
type soqlProjectRoleAsset struct {
	ID         string                       `salesforce:"Id"          json:"Id"`
	AccountID  string                       `salesforce:"AccountId"   json:"AccountId"`
	Product2ID string                       `salesforce:"Product2Id"  json:"Product2Id"`
	ProjectsID *string                      `salesforce:"Projects__c" json:"Projects__c"`
	Account    *soqlProjectRoleAssetAccount `salesforce:"Account"     json:"Account"`
	Project    *soqlAssetProject            `salesforce:"Projects__r" json:"Projects__r"`
}

// soqlAlternateEmail represents a Salesforce Alternate_Email__c record.
// ContactID is sourced from Contact_Name__c, which is the lookup relationship
// field holding the full 18-char Contact SFID (not a human-readable name).
type soqlAlternateEmail struct {
	ID        string `salesforce:"Id"`
	ContactID string `salesforce:"Contact_Name__c"`
	Email     string `salesforce:"Alternate_Email_Address__c"`
	Primary   bool   `salesforce:"Primary_Email__c"`
}

// soqlAlternateEmailContact is the result of a contact lookup via
// Alternate_Email__c. Contact_Name__c is a lookup field holding the B2B
// Contact SFID.
type soqlAlternateEmailContact struct {
	ContactNameID string `salesforce:"Contact_Name__c"`
}

// soqlContactByEmail is the result of a contact lookup via Contact.Email.
type soqlContactByEmail struct {
	ID string `salesforce:"Id"`
}

// soqlProject represents a Salesforce Project__c record used for slug-based
// lookups against the Project__c object.
type soqlProject struct {
	ID   string  `salesforce:"Id"`
	Name string  `salesforce:"Name"`
	Slug *string `salesforce:"Slug__c"`
}

// soqlAccountParent is the parent Account sub-object returned by the SOQL
// relationship sub-select (e.g. SELECT ..., Parent.Name, Parent.Logo_URL__c
// FROM Account). Both salesforce and json tags match the SOQL field aliases.
type soqlAccountParent struct {
	ID      string  `salesforce:"Id"          json:"Id"`
	Name    string  `salesforce:"Name"        json:"Name"`
	LogoURL *string `salesforce:"Logo_URL__c" json:"Logo_URL__c"`
}

// soqlAccount represents a Salesforce Account record returned by a SOQL query.
// Used for B2BOrg search and list operations. Only non-deleted Accounts that
// have at least one membership Asset are returned — the WHERE clause filters
// by IsDeleted = false and a semi-join on Asset.
//
// Both salesforce and json tags are present so that the struct can be decoded
// by either the go-salesforce library (client.Query) or by json.Unmarshal
// (used in QueryPage, which decodes raw Salesforce REST JSON directly).
type soqlAccount struct {
	ID      string  `salesforce:"Id"          json:"Id"`
	Name    string  `salesforce:"Name"        json:"Name"`
	LogoURL *string `salesforce:"Logo_URL__c" json:"Logo_URL__c"`
	// Website is the free-text URL field (Account.Website). May contain bare
	// domains, full URIs, or arbitrary garbage — callers should treat it as
	// an opaque link string and not assume any particular format.
	Website *string `salesforce:"Website" json:"Website"`
	// PrimaryDomain is the canonical primary domain (Account.Account_Domain__c).
	// Intended to be a bare domain (e.g. "example.com") but may contain garbage
	// data — callers should normalize and warn on unexpected values.
	PrimaryDomain *string `salesforce:"Account_Domain__c" json:"Account_Domain__c"`
	// DomainAlias is a comma-separated list of additional domains
	// (Account.Domain_Alias__c). Each item should be treated with the same
	// normalization rules as PrimaryDomain.
	DomainAlias *string `salesforce:"Domain_Alias__c"  json:"Domain_Alias__c"`
	Description *string `salesforce:"Description"      json:"Description"`
	Phone       *string `salesforce:"Phone"            json:"Phone"`
	// ParentID is the Salesforce Id of the parent Account, if any.
	ParentID *string `salesforce:"ParentId" json:"ParentId"`
	// Industry is the standard SF Account.Industry field.
	Industry *string `salesforce:"Industry" json:"Industry"`
	// Sector is the custom SF field Account.Sector__c.
	Sector *string `salesforce:"Sector__c" json:"Sector__c"`
	// CrunchBaseURL is the custom SF field Account.CrunchBase_URL__c.
	CrunchBaseURL *string `salesforce:"CrunchBase_URL__c" json:"CrunchBase_URL__c"`
	// NumberOfEmployees is the standard SF Account.NumberOfEmployees (Integer).
	NumberOfEmployees *int64 `salesforce:"NumberOfEmployees" json:"NumberOfEmployees"`
	// Status is the custom SF field Account.LF_Membership_Status__c.
	Status *string `salesforce:"LF_Membership_Status__c" json:"LF_Membership_Status__c"`
	// IsMember is the custom SF field Account.IsMember__c.
	IsMember         *bool  `salesforce:"IsMember__c"             json:"IsMember__c"`
	CreatedDate      string `salesforce:"CreatedDate"             json:"CreatedDate"`
	LastModifiedDate string `salesforce:"LastModifiedDate"        json:"LastModifiedDate"`
	// Parent is the parent Account sub-object from the SOQL relationship sub-select.
	// Present when the query includes `Parent.Name, Parent.Logo_URL__c` in the SELECT clause.
	Parent *soqlAccountParent `salesforce:"Parent"                  json:"Parent"`
}

// derefString returns the string value of a *string pointer, or an empty string
// if the pointer is nil.
func derefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}
