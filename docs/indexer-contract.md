# Indexer Contract — Member Service

This document is the authoritative reference for all data the member service sends to the indexer service, which makes resources searchable via the [query service](https://github.com/linuxfoundation/lfx-v2-query-service).

**Update this document in the same PR as any change to indexer message construction.**

> **Note:** The `EventPublisher` interface (`internal/domain/port/event_publisher.go`) exists with placeholder `any` types. Concrete `indexertypes.IndexingConfig` and `fgasync.GenericFGAMessage` types will be wired in a later ticket. The schemas below reflect the intended contract.

---

## Resource Types

- [Membership Tier](#membership-tier)
- [Project Membership](#project-membership)
- [Key Contact](#key-contact)
- [B2B Org](#b2b-org)

---

## Membership Tier

**Source struct:** `internal/domain/model/member.go` — `MembershipTier`

**Indexed on:** create, update, delete of a membership tier (Salesforce `Product2`).

### Data Schema

| Field | Type | Description |
|---|---|---|
| `uid` | string | Tier unique identifier (invertible UUID v8 from `Product2.Id`) |
| `project_uid` | string | v2 UUID of the project this tier belongs to |
| `name` | string | Product name (e.g., `"Gold Corporate Membership"`) |
| `family` | string (optional) | Product family (e.g., `"Membership"`); omitted when empty |
| `product_type` | string (optional) | Product type (`Type__c` on `Product2`); omitted when empty |
| `created_at` | timestamp | Creation time (RFC3339) |
| `updated_at` | timestamp | Last update time (RFC3339) |

> `project_slug` is used internally for resolver round-trips and is **not** included in indexed data.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `{uid}` | `a1b2c3d4-...` | Direct lookup by UID |
| `membership_tier_uid:{uid}` | `membership_tier_uid:a1b2c3d4-...` | Namespaced lookup by UID |
| `project_uid:{value}` | `project_uid:cbef1ed5-...` | Find tiers for a project |

### Access Control (IndexingConfig)

| Field | Value |
|---|---|
| `access_check_object` | `project:{project_uid}` |
| `access_check_relation` | `auditor` |
| `history_check_object` | `project:{project_uid}` |
| `history_check_relation` | `auditor` |

### Search Behavior

| Field | Value |
|---|---|
| `fulltext` | `name`, `family` |
| `name_and_aliases` | `name` |
| `sort_name` | `name` |
| `public` | `false` (access-controlled by project membership) |

### Parent References

| Ref | Condition |
|---|---|
| `project:{project_uid}` | Always set |

---

## Project Membership

**Source struct:** `internal/domain/model/membership.go` — `ProjectMembership`

**Indexed on:** create, update, delete of a project membership (Salesforce `Asset`).

### Data Schema

| Field | Type | Description |
|---|---|---|
| `uid` | string | Membership unique identifier (invertible UUID v8 from `Asset.Id`) |
| `tier_uid` | string (may be empty) | UID of the associated `MembershipTier` (`Product2`); may be empty when the related `Asset` / tier relationship is unavailable or cannot be decoded |
| `project_uid` | string | v2 UUID of the project this membership belongs to |
| `project_slug` | string (optional) | URL slug of the associated project; omitted when empty |
| `b2b_org_uid` | string (optional) | UID of the member company (`B2BOrg`, derived from `Account.Id`); omitted when empty |
| `status` | string | Membership status (e.g., `"Active"`, `"Expired"`) |
| `year` | string (optional) | Membership year (e.g., `"2025"`); omitted when empty |
| `tier` | string (optional) | Membership tier label (e.g., `"Gold"`); omitted when empty |
| `auto_renew` | bool | Whether automatic renewal is enabled |
| `renewal_type` | string (optional) | Renewal cadence (e.g., `"Annual"`); omitted when empty |
| `price` | float64 (optional) | Current membership price; omitted when zero |
| `annual_full_price` | float64 (optional) | Full annual list price before discounts; omitted when zero |
| `payment_frequency` | string (optional) | Payment frequency; omitted when empty |
| `payment_terms` | string (optional) | Payment terms (e.g., `"Net 30"`); omitted when empty |
| `agreement_date` | string (optional) | Date agreement was signed; omitted when empty |
| `purchase_date` | string (optional) | Effective purchase date (`COALESCE` of `PurchaseDate`, `InstallDate`, `CreatedDate`); omitted when empty |
| `start_date` | string (optional) | Membership start date (`InstallDate`); omitted when empty |
| `end_date` | string (optional) | Membership end date (`UsageEndDate`); omitted when empty |
| `company_name` | string | Member company name (denormalized from `Account`) |
| `company_logo_url` | string (optional) | Member company logo URL (denormalized from `Account`); omitted when empty |
| `company_domain` | string (optional) | Member company website/domain (denormalized from `Account.Website`); omitted when empty |
| `tier_name` | string (optional) | Product name (denormalized from `Product2`); omitted when empty |
| `tier_family` | string (optional) | Product family (denormalized from `Product2`); omitted when empty |
| `tier_product_type` | string (optional) | Product type (denormalized from `Product2`); omitted when empty |
| `created_at` | timestamp | Creation time (RFC3339) |
| `updated_at` | timestamp | Last update time (RFC3339) |

> `AccountSFID` is used internally for write-path operations and is **not** included in indexed data.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `{uid}` | `d4e5f6a7-...` | Direct lookup by UID |
| `project_membership_uid:{uid}` | `project_membership_uid:d4e5f6a7-...` | Namespaced lookup by UID |
| `project_uid:{value}` | `project_uid:cbef1ed5-...` | Find memberships for a project |
| `tier_uid:{value}` | `tier_uid:a1b2c3d4-...` | Find memberships by tier |
| `b2b_org_uid:{value}` | `b2b_org_uid:e8f9a0b1-...` | Find memberships for a company |
| `status:{value}` | `status:Active` | Find memberships by status |
| `year:{value}` | `year:2025` | Find memberships by year |
| `company_name:{value}` | `company_name:The Linux Foundation` | Find memberships by company name |
| `company_domain:{value}` | `company_domain:https://linuxfoundation.org/about/` | Find memberships by company website value (may be a bare domain or full URL) |

> Tags for `b2b_org_uid`, `status`, `year`, `company_name`, and `company_domain` are only emitted when the source value is non-empty. For `company_domain`, the source value comes from the company website field and may be a bare domain or a full URL.

### Access Control (IndexingConfig)

| Field | Value |
|---|---|
| `access_check_object` | `project:{project_uid}` |
| `access_check_relation` | `auditor` |
| `history_check_object` | `project:{project_uid}` |
| `history_check_relation` | `auditor` |

### Search Behavior

| Field | Value |
|---|---|
| `fulltext` | `company_name`, `tier_name`, `tier` |
| `name_and_aliases` | `company_name`, `tier_name` (non-empty values only) |
| `sort_name` | `company_name` |
| `public` | `false` (access-controlled by project membership) |

### Parent References

| Ref | Condition |
|---|---|
| `project:{project_uid}` | Always set |
| `membership_tier:{tier_uid}` | Only when `tier_uid` is non-empty |
| `b2b_org:{b2b_org_uid}` | Only when `b2b_org_uid` is non-empty |

---

## Key Contact

**Source struct:** `internal/domain/model/key_contact.go` — `KeyContact`

**Indexed on:** create, update, delete of a key contact (Salesforce `Project_Role__c`).

### Data Schema

| Field | Type | Description |
|---|---|---|
| `uid` | string | Key contact unique identifier (invertible UUID v8 from `Project_Role__c.Id`) |
| `membership_uid` | string | UID of the associated `ProjectMembership` (`Asset`) |
| `tier_uid` | string (may be empty) | UID of the associated `MembershipTier` (`Product2`); may be empty when the related `Asset` / tier relationship is unavailable or cannot be decoded |
| `project_uid` | string | v2 UUID of the project this key contact belongs to |
| `b2b_org_uid` | string (optional) | UID of the member company (`B2BOrg`, derived from `Account.Id`); omitted when empty |
| `role` | string | Contact role designation (e.g., `"Voting Representative"`) |
| `status` | string | Role record status (e.g., `"Active"`) |
| `board_member` | bool | Whether this contact holds a board member role |
| `primary_contact` | bool | Whether this is the primary contact for the membership |
| `first_name` | string | Contact first name (denormalized from `Contact`) |
| `last_name` | string | Contact last name (denormalized from `Contact`) |
| `title` | string (optional) | Job title (denormalized from `Contact`); omitted when empty |
| `email` | string (optional) | Primary email from `Alternate_Email__c` where `Primary_Email__c = true`; omitted when empty |
| `company_name` | string | Member company name (denormalized from `Account` via membership `Asset`) |
| `company_logo_url` | string (optional) | Member company logo URL (denormalized from `Account`); omitted when empty |
| `company_domain` | string (optional) | Member company website/domain (denormalized from `Account.Website`); omitted when empty |
| `created_at` | timestamp | Creation time (RFC3339) |
| `updated_at` | timestamp | Last update time (RFC3339) |

> `project_slug` is used internally for resolver round-trips and is **not** included in indexed data.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `{uid}` | `b2c3d4e5-...` | Direct lookup by UID |
| `key_contact_uid:{uid}` | `key_contact_uid:b2c3d4e5-...` | Namespaced lookup by UID |
| `membership_uid:{value}` | `membership_uid:d4e5f6a7-...` | Find key contacts for a membership |
| `project_uid:{value}` | `project_uid:cbef1ed5-...` | Find key contacts for a project |
| `b2b_org_uid:{value}` | `b2b_org_uid:e8f9a0b1-...` | Find key contacts for a company |
| `role:{value}` | `role:Voting Representative` | Find key contacts by role |
| `status:{value}` | `status:Active` | Find key contacts by status |
| `email:{value}` | `email:user@example.com` | Find key contacts by email |
| `company_name:{value}` | `company_name:The Linux Foundation` | Find key contacts by company name |
| `company_domain:{value}` | `company_domain:https://linuxfoundation.org` | Find key contacts by company website/domain (`Account.Website` value) |

> Tags for `b2b_org_uid`, `role`, `status`, `email`, `company_name`, and `company_domain` are only emitted when the value is non-empty. `company_domain` is sourced from `Account.Website` and may be a full URL.

### Access Control (IndexingConfig)

| Field | Value |
|---|---|
| `access_check_object` | `project:{project_uid}` |
| `access_check_relation` | `auditor` |
| `history_check_object` | `project:{project_uid}` |
| `history_check_relation` | `auditor` |

### Search Behavior

| Field | Value |
|---|---|
| `fulltext` | `first_name`, `last_name`, `email`, `company_name`, `role` |
| `name_and_aliases` | `first_name`, `last_name`, `email` (non-empty values only) |
| `sort_name` | `last_name` |
| `public` | `false` (access-controlled by project membership) |

### Parent References

| Ref | Condition |
|---|---|
| `project:{project_uid}` | Always set |
| `project_membership:{membership_uid}` | Always set |
| `b2b_org:{b2b_org_uid}` | Only when `b2b_org_uid` is non-empty |

---

## B2B Org

**Source struct:** `internal/domain/model/b2b_org.go` — `B2BOrg`

**Indexed on:** create, update, delete of a B2B organization (Salesforce `Account`).

### Data Schema

| Field | Type | Description |
|---|---|---|
| `uid` | string | Organization unique identifier (invertible UUID v8 from `Account.Id`) |
| `name` | string | Organization display name |
| `website` | string (optional) | Website URL — always includes scheme (`http`/`https`); omitted when empty or unparseable |
| `primary_domain` | string (optional) | Normalized primary domain (bare host, e.g., `"example.com"`); omitted when empty or invalid |
| `domain_aliases` | array[string] (optional) | Additional normalized domains; each item normalized with the same rules as `primary_domain`; invalid items are dropped |
| `logo_url` | string (optional) | Organization logo image URL; omitted when empty |
| `created_at` | timestamp | Creation time (RFC3339) |
| `updated_at` | timestamp | Last update time (RFC3339) |

> `SFID` (raw Salesforce `Account.Id`) is used internally and is **not** included in indexed data.

### Tags

| Tag Format | Example | Purpose |
|---|---|---|
| `{uid}` | `e8f9a0b1-...` | Direct lookup by UID |
| `b2b_org_uid:{uid}` | `b2b_org_uid:e8f9a0b1-...` | Namespaced lookup by UID |
| `primary_domain:{value}` | `primary_domain:linuxfoundation.org` | Find org by primary domain |
| `domain_alias:{value}` | `domain_alias:lf.org` | Find org by an alias domain |
| `name:{value}` | `name:The Linux Foundation` | Find org by name |

> Tags for `primary_domain`, `domain_alias`, and `name` are only emitted when the value is non-empty. One `domain_alias:{value}` tag is emitted per entry in `domain_aliases`.

### Access Control (IndexingConfig)

| Field | Value |
|---|---|
| `access_check_object` | `b2b_org:{uid}` |
| `access_check_relation` | `viewer` |
| `history_check_object` | `b2b_org:{uid}` |
| `history_check_relation` | `auditor` |

### Search Behavior

| Field | Value |
|---|---|
| `fulltext` | `name`, `primary_domain`, `website` |
| `name_and_aliases` | `name`, `primary_domain` (non-empty values only) |
| `sort_name` | `name` |
| `public` | `false` (access-controlled) |

### Parent References

_(none — B2BOrg is a top-level entity)_
