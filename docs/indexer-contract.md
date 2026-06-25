# Indexer Contract — Member Service

This document is the authoritative reference for all data the member service sends to the indexer service, which makes resources searchable via the [query service](https://github.com/linuxfoundation/lfx-v2-query-service).

**Update this document in the same PR as any change to indexer message construction.**

---

## Resource Types

- [B2B Org](#b2b-org)
- [Project Membership](#project-membership)
- [Key Contact](#key-contact)
- [B2B Org Settings](#b2b-org-settings)
- [Org Workspace](#org-workspace)
- [Org Workspace Project](#org-workspace-project)

---

## Delete Wire Contract (all resource types)

For `action: deleted`, the indexer-service requires the message `data` field to be a plain JSON **string** holding the OpenSearch object ID — **not** a resource object. An object payload is rejected (`data must be a string (object ID) for deleted actions`) and the target document is never tombstoned, leaving a stale ("ghost") document in the query service.

This rule applies to **every** member resource type: `b2b_org`, `project_membership`, `key_contact`, `org_workspace`, and `org_workspace_project`. The object ID equals each type's `IndexingConfig.ObjectID`:

| Object type             | Delete `data` value                         |
|-------------------------|---------------------------------------------|
| `b2b_org`               | `org.UID`                                    |
| `project_membership`    | `pm.UID`                                     |
| `key_contact`           | `kc.UID`                                     |
| `org_workspace`         | `workspace.UID`                             |
| `org_workspace_project` | `"{workspaceUID}/{projectUID}"` (composite) |

Implementation: each publisher in `internal/service/messaging.go` routes through a per-resource `build<Type>IndexerInput(x, action)` helper that returns the bare UID string on `ActionDeleted` and the struct/view on create/update. The helper returns a raw Go string — `json.Marshal` then emits `{"data":"uid"}`; do not pre-quote the UID.

> **Already-missing endpoint deletes:** the `key_contact` (`DELETE /project_memberships/{membership_uid}/key_contacts/{uid}`) and `workspace` (`DELETE /b2b_orgs/{uid}/workspaces/{workspace_uid}`) endpoints publish a best-effort indexer delete (and, for key contacts, an FGA member_remove) keyed by the path UID even when the source record is already gone, then still return 404. This sweeps documents/tuples that outlived their source record. Cleanup runs only on a genuine NotFound — never on infrastructure errors, stale `If-Match`, or validation failures.

---

## B2B Org

**Object type:** `b2b_org`

**NATS subject:** `lfx.index.b2b_org`

**Source struct:** `internal/domain/model/b2b_org.go` — `B2BOrg`

**Indexed on:** create, update, delete of a B2B org — via HTTP write endpoints, CDC (`AccountChangeEvent`), and `/admin/reindex`.

### Data Schema

| Field                 | Type                | Description                                                                                                                                                  |
|-----------------------|---------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `uid`                 | string              | B2B org unique identifier                                                                                                                                    |
| `name`                | string              | Organization display name                                                                                                                                    |
| `description`         | string (optional)   | Free-text description                                                                                                                                        |
| `phone`               | string (optional)   | Contact phone number                                                                                                                                         |
| `website`             | string (optional)   | Website URL                                                                                                                                                  |
| `primary_domain`      | string (optional)   | Canonical primary domain                                                                                                                                     |
| `domain_aliases`      | []string (optional) | Additional normalized domains                                                                                                                                |
| `logo_url`            | string (optional)   | Logo image URL                                                                                                                                               |
| `industry`            | string (optional)   | Industry classification                                                                                                                                      |
| `sector`              | string (optional)   | Sector classification                                                                                                                                        |
| `crunch_base_url`     | string (optional)   | CrunchBase profile URL                                                                                                                                       |
| `number_of_employees` | int64 (optional)    | Employee count                                                                                                                                               |
| `status`              | string (optional)   | LF membership status                                                                                                                                         |
| `is_member`           | bool                | Whether the org is an active LF member                                                                                                                       |
| `parent_uid`          | string (optional)   | UID of the parent org                                                                                                                                        |
| `parent_detail`       | object (optional)   | Denormalized parent info: `uid`, `name`, `logo_url`                                                                                                          |
| `is_parent`           | bool (optional)     | `true` when this org has at least one direct member-eligible child. Omitted when false. To retrieve children, query the index for `parent_uid = <this uid>`. |
| `created_at`          | timestamp           | Creation time (RFC3339)                                                                                                                                      |
| `updated_at`          | timestamp           | Last update time (RFC3339)                                                                                                                                   |

### Tags

| Tag Format                 | Example                           | Purpose                           |
|----------------------------|-----------------------------------|-----------------------------------|
| `{uid}`                    | `0012M00002qnukOQAQ`              | Direct lookup by UID              |
| `b2b_org_uid:{uid}`        | `b2b_org_uid:0012M00002qnukOQAQ`  | Find orgs by UID                  |
| `parent_b2b_org_uid:{uid}` | `parent_b2b_org_uid:0014100000Te2ovAAB` | Find all children of a parent org |
| `is_member:{true\|false}`  | `is_member:true`                  | Filter by LF member status        |

> `parent_b2b_org_uid` tag is only emitted when `parent_uid` is non-empty.

### Access Control (IndexingConfig)

| Field                    | Value           |
|--------------------------|-----------------|
| `access_check_object`    | `b2b_org:{uid}` |
| `access_check_relation`  | `auditor`       |
| `history_check_object`   | `b2b_org:{uid}` |
| `history_check_relation` | `auditor`       |

### Search Behavior

| Field              | Value                                                         |
|--------------------|---------------------------------------------------------------|
| `fulltext`         | `name`, `primary_domain`, `description`, `industry`, `sector` |
| `name_and_aliases` | `name`, `primary_domain`, all `domain_aliases`                |
| `sort_name`        | `name` (lowercased)                                           |
| `public`           | `false`                                                       |

### Parent References

| Ref                    | Condition                     |
|------------------------|-------------------------------|
| `b2b_org:{parent_uid}` | Only when `parent_uid` is set |

---

## Project Membership

**Object type:** `project_membership`

**NATS subject:** `lfx.index.project_membership`

**Source struct:** `internal/domain/model/membership.go` — `ProjectMembership`

**Indexed on:** CDC (`AssetChangeEvent`) and `/admin/reindex`.

### Data Schema

| Field               | Type               | Description                                    |
|---------------------|--------------------|------------------------------------------------|
| `uid`               | string             | Membership unique identifier                   |
| `tier_uid`          | string             | UID of the associated membership tier          |
| `project_uid`       | string             | v2 project UUID (NATS slug-resolved)           |
| `project_slug`      | string (optional)  | URL slug of the project                        |
| `b2b_org_uid`       | string (optional)  | 18-char Salesforce Account SFID of the member company |
| `status`            | string             | Membership status, e.g. `Active`               |
| `year`              | string (optional)  | Membership year, e.g. `2025`                   |
| `tier`              | string (optional)  | Tier label, e.g. `Gold`                        |
| `auto_renew`        | bool               | Whether automatic renewal is enabled           |
| `renewal_type`      | string (optional)  | Renewal cadence                                |
| `price`             | float64 (optional) | Current membership price                       |
| `annual_full_price` | float64 (optional) | Full annual list price                         |
| `payment_frequency` | string (optional)  | Payment frequency                              |
| `payment_terms`     | string (optional)  | Payment terms                                  |
| `agreement_date`    | string (optional)  | Date the membership agreement was signed       |
| `purchase_date`     | string (optional)  | Effective purchase date                        |
| `start_date`        | string (optional)  | Membership start date                          |
| `end_date`          | string (optional)  | Membership end date                            |
| `company_name`      | string             | Member company name                            |
| `company_logo_url`  | string (optional)  | Member company logo URL                        |
| `company_domain`    | string (optional)  | Member company website/domain                  |
| `tier_name`         | string (optional)  | Product name, e.g. `Gold Corporate Membership` |
| `tier_family`       | string (optional)  | Product family, e.g. `Membership`              |
| `tier_product_type` | string (optional)  | Product type                                   |
| `created_at`        | timestamp          | Creation time (RFC3339)                        |
| `updated_at`        | timestamp          | Last update time (RFC3339)                     |

### Tags

| Tag Format                     | Example                                          | Purpose                            |
|--------------------------------|--------------------------------------------------|------------------------------------|
| `{uid}`                        | `02iB0000009ABCdIAM`                             | Direct lookup by UID               |
| `project_membership_uid:{uid}` | `project_membership_uid:02iB0000009ABCdIAM`      | Find memberships by UID            |
| `project_uid:{uid}`            | `project_uid:abc-...`                            | Find all memberships for a project |
| `project_sfid:{sfid}`          | `project_sfid:a2T2M000000ABCdUAG`                | Find memberships by Salesforce project SFID |
| `b2b_org_uid:{uid}`            | `b2b_org_uid:0012M00002qnukOQAQ`                 | Find all memberships for an org    |

### Access Control (IndexingConfig)

| Field                    | Value                      |
|--------------------------|----------------------------|
| `access_check_object`    | `project_membership:{uid}` |
| `access_check_relation`  | `auditor`                  |
| `history_check_object`   | `project_membership:{uid}` |
| `history_check_relation` | `auditor`                  |

### Search Behavior

| Field              | Value                                         |
|--------------------|-----------------------------------------------|
| `fulltext`         | `company_name`, `tier_name`, `status`, `year` |
| `name_and_aliases` | `company_name`, `company_domain`              |
| `sort_name`        | `company_name` (lowercased)                   |
| `public`           | `false`                                       |

### Parent References

| Ref                     | Condition                      |
|-------------------------|--------------------------------|
| `b2b_org:{b2b_org_uid}` | Only when `b2b_org_uid` is set |
| `project:{project_uid}` | Only when `project_uid` is set |

---

## Key Contact

**Object type:** `key_contact`

**NATS subject:** `lfx.index.key_contact`

**Source struct:** `internal/domain/model/key_contact.go` — `KeyContact`

**Indexed on:** create, update, delete via `/project_memberships/{uid}/key_contacts`, CDC (`Project_Role__ChangeEvent`), and `/admin/reindex`.

### Data Schema

| Field              | Type                | Description                                                                 |
|--------------------|---------------------|-----------------------------------------------------------------------------|
| `uid`              | string              | Key contact unique identifier                                               |
| `membership_uid`   | string              | UID of the associated project membership                                    |
| `tier_uid`         | string              | UID of the associated membership tier                                       |
| `project_uid`      | string              | v2 project UUID (NATS slug-resolved)                                        |
| `project_name`     | string (optional)   | Display name of the project — also indexed in `fulltext` for keyword search |
| `project_logo_url` | string (optional)   | Logo image URL for the project                                              |
| `b2b_org_uid`      | string (optional)   | 18-char Salesforce Account SFID of the member company                       |
| `role`             | string              | Contact role, e.g. `Voting Representative`                                  |
| `status`           | string              | Role record status, e.g. `Active`                                           |
| `board_member`     | bool                | Whether this contact holds a board member role                              |
| `primary_contact`  | bool                | Whether this is the primary contact for the membership                      |
| `first_name`       | string              | Contact's first name                                                        |
| `last_name`        | string              | Contact's last name                                                         |
| `title`            | string (optional)   | Contact's job title                                                         |
| `email`            | string (optional)   | Primary email address                                                       |
| `username`         | string (optional)   | Resolved LFID username                                                      |
| `emails`           | []string (optional) | Full list of email addresses                                                |
| `company_name`     | string              | Member company name                                                         |
| `company_logo_url` | string (optional)   | Member company logo URL                                                     |
| `company_domain`   | string (optional)   | Member company website/domain                                               |
| `created_at`       | timestamp           | Creation time (RFC3339)                                                     |
| `updated_at`       | timestamp           | Last update time (RFC3339)                                                  |

### Tags

| Tag Format                     | Example                                     | Purpose                            |
|--------------------------------|---------------------------------------------|------------------------------------|
| `{uid}`                        | `a0K2M000000ABCdUAG`                        | Direct lookup by UID               |
| `key_contact_uid:{uid}`        | `key_contact_uid:a0K2M000000ABCdUAG`        | Find contacts by UID               |
| `project_membership_uid:{uid}` | `project_membership_uid:02iB0000009ABCdIAM` | Find all contacts for a membership |
| `project_uid:{uid}`            | `project_uid:abc-...`                       | Find all contacts for a project    |
| `project_sfid:{sfid}`          | `project_sfid:a2T2M000000ABCdUAG`           | Find contacts by Salesforce project SFID |
| `b2b_org_uid:{uid}`            | `b2b_org_uid:0012M00002qnukOQAQ`            | Find all contacts for an org       |
| `role:{value}`                 | `role:Voting Representative`                | Filter contacts by role            |
| `status:{value}`               | `status:Active`                             | Filter contacts by status          |

> `project_sfid`, `role`, and `status` tags are only emitted when non-empty.

### Access Control (IndexingConfig)

| Field                    | Value                                 |
|--------------------------|---------------------------------------|
| `access_check_object`    | `project_membership:{membership_uid}` |
| `access_check_relation`  | `auditor`                             |
| `history_check_object`   | `project_membership:{membership_uid}` |
| `history_check_relation` | `auditor`                             |

### Search Behavior

| Field              | Value                                                                      |
|--------------------|----------------------------------------------------------------------------|
| `fulltext`         | `first_name`, `last_name`, `email`, `role`, `company_name`, `project_name` |
| `name_and_aliases` | Full name, `email`                                                         |
| `sort_name`        | `last_name first_name` (lowercased)                                        |
| `public`           | `false`                                                                    |
| `contacts`         | `[{lfx_principal: uid, name: full_name, emails: [...]}]`                   |

### Parent References

| Ref                                   | Condition                         |
|---------------------------------------|-----------------------------------|
| `b2b_org:{b2b_org_uid}`               | Only when `b2b_org_uid` is set    |
| `project:{project_uid}`               | Only when `project_uid` is set    |
| `project_membership:{membership_uid}` | Only when `membership_uid` is set |

---

## B2B Org Settings

**Object type:** `b2b_org_settings`

**NATS subject:** `lfx.index.b2b_org_settings`

**Source struct:** `internal/service/messaging.go` — `b2bOrgSettingsIndexerView` (adapter view; canonical state is `model.B2BOrgSettings` in the `org-settings` KV bucket)

**Trigger:** Any write that mutates org settings — `PUT /b2b_orgs/{uid}/settings`, `POST/PUT/DELETE /b2b_orgs/{uid}/settings/users`, or `invite_accepted` promotion — or `POST /admin/reindex` with `types=["b2b_org_settings"]` (backfill). The doc exists only when at least one principal has been written.

**Action mapping:** `ActionCreated` on first write (when no prior KV record exists); `ActionUpdated` on all subsequent writes.

**Note:** `ObjectID` equals the parent org UID (not a separate settings UID) so a single point-lookup retrieves both the org doc and the settings doc. Callers filter by `object_type=b2b_org_settings`.

### Payload Fields

Flat `members[]` array — role is a first-class field on each entry. Both accepted and pending entries are included; revoked and expired are excluded.

| Field        | Example value                                                |
|--------------|--------------------------------------------------------------|
| `uid`        | `0012M00002qnukOQAQ`                                         |
| `members`    | `[{username, email, name, role, invite_status, updated_at}]` |
| `created_at` | `2026-01-15T10:00:00Z`                                       |
| `updated_at` | `2026-05-20T14:30:00Z`                                       |

Per-member entry shape:

| Field           | Example value          | Notes                                                                 |
|-----------------|------------------------|-----------------------------------------------------------------------|
| `username`      | `<lfid>`               | Absent for pending invites                                            |
| `email`         | `user@example.org`     | Always present                                                        |
| `name`          | `Display Name`         | Optional                                                              |
| `role`          | `writer`               | `"writer"` or `"auditor"`; writer takes precedence if user holds both |
| `invite_status` | `accepted`             | `accepted`, `pending`                                                 |
| `updated_at`    | `2026-01-15T10:00:00Z` | Last modification to this membership row                              |

### Tags

| Tag                   | Condition                                                                      |
|-----------------------|--------------------------------------------------------------------------------|
| `{uid}`               | Always — bare org UID for direct lookup                                        |
| `b2b_org_uid:{uid}`   | Always — prefixed org UID; same tag as the `b2b_org` doc for cross-type lookup |
| `has_writers`         | ≥1 writer with `invite_status=accepted`                                        |
| `has_auditors`        | ≥1 auditor with `invite_status=accepted`                                       |
| `has_pending_invites` | ≥1 entry (writer or auditor) with `invite_status=pending`                      |
| `writer:{username}`   | One tag per accepted writer with a non-empty LFID username                     |
| `auditor:{username}`  | One tag per accepted auditor with a non-empty LFID username                    |
| `member:{username}`   | One tag per accepted user — role-agnostic union, deduped across writer+auditor |

> Revoked and expired entries do not trigger any tag — they are audit-trail data, not actionable state.
> Pending users (no username) are included in `members[]` but produce no `writer:`, `auditor:`, or `member:` tags.

### Query Patterns

**"Which orgs is user X a member of?" (role-agnostic)**
```
GET /query/resources?v=1&type=b2b_org_settings&tags=member:{username}
```
Returns one doc per org where the user is an accepted writer or auditor. Each doc contains `data.uid` (the org UID) and the full `data.members[]` array. The `member:` tag covers both roles so a single call suffices.

**"Which orgs is user X a writer on?" (role-specific)**
```
GET /query/resources?v=1&type=b2b_org_settings&tags=writer:{username}
```

**"Which orgs is user X an auditor on?" (role-specific)**
```
GET /query/resources?v=1&type=b2b_org_settings&tags=auditor:{username}
```

**"How many orgs does user X belong to?"**
```
GET /query/resources/count?type=b2b_org_settings&tags=member:{username}
```
Returns `{"count": N, "has_more": false}`. `has_more` is true when the result exceeds the aggregation bucket limit.

**"Who has access to org Y?"**
```
GET /query/resources?v=1&type=b2b_org_settings&object_id={org_uid}
```
Returns the single settings doc for that org with the full `members[]` roster.

> All queries are enforced by an FGA `auditor` check on `b2b_org:{uid}` — only the calling user's accessible orgs are returned regardless of filter.
> Tag values containing `:` or `|` must be URL-encoded in query strings: `:` → `%3A`, `|` → `%7C`.

### Access / History Check

| Field                    | Value                                                                      |
|--------------------------|----------------------------------------------------------------------------|
| `access_check_object`    | `b2b_org:{uid}` (parent, not self — settings has no separate FGA type)     |
| `access_check_relation`  | `auditor`                                                                  |
| `history_check_object`   | `b2b_org:{uid}`                                                            |
| `history_check_relation` | `writer` (history = write-side concern; matches project-service precedent) |

### Name and Aliases / Fulltext

| Field              | Contents                                                                                                         |
|--------------------|------------------------------------------------------------------------------------------------------------------|
| `name_and_aliases` | `[org.Name, org.PrimaryDomain, ...org.DomainAliases]` — domain typeahead works even with no writers configured   |
| `fulltext`         | Accepted writers/auditors: `Name + Email`. Pending entries: `Email + Name-if-present`. Revoked/expired excluded. |
| `sort_name`        | `lower(org.Name)`                                                                                                |

### Parent References

| Ref                    | Condition                          |
|------------------------|------------------------------------|
| `b2b_org:{uid}`        | Always (self-ref for point-lookup) |
| `b2b_org:{parent_uid}` | Only when `parent_uid` is set      |

---

## Org Workspace

**Object type:** `org_workspace`

**NATS subject:** `lfx.index.org_workspace`

**Source struct:** `internal/domain/model/workspace.go` — `Workspace` (metadata only; project associations are indexed separately as `org_workspace_project` documents)

**Trigger:** Any write that creates, updates, or deletes workspace metadata — `POST /b2b_orgs/{uid}/workspaces`, `PUT /b2b_orgs/{uid}/workspaces/{workspace_uid}`, `DELETE /b2b_orgs/{uid}/workspaces/{workspace_uid}`. Project add/remove operations do **not** trigger this subject.

**Note:** `ObjectID` equals the workspace UID (`workspace.UID`). Each workspace is a separate indexed document. The org UID is represented via tags (`b2b_org_uid:{orgUID}`) and parent references.

### Payload Fields

The indexed view is the `Workspace` metadata struct (no projects list).

| Field        | Type      | Description                                        |
|--------------|-----------|----------------------------------------------------|
| `uid`        | string    | Workspace unique identifier (UUID)                 |
| `name`       | string    | Workspace display name (unique within the org)     |
| `created_by` | string    | Principal who created the workspace                |
| `updated_by` | string    | Principal who last modified the workspace          |
| `created_at` | timestamp | Creation time (RFC3339)                            |
| `updated_at` | timestamp | Last update time (RFC3339)                         |

### Tags

| Tag Format                    | Example                                | Purpose                            |
|-------------------------------|----------------------------------------|------------------------------------|
| `{uid}`                       | `550e8400-e29b-41d4-a716-446655440000` | Direct lookup by workspace UID     |
| `b2b_org_workspace_uid:{uid}` | `b2b_org_workspace_uid:550e8400-...`   | Find workspaces by UID             |
| `b2b_org_uid:{orgUID}`        | `b2b_org_uid:0012M00002qnukOQAQ`       | Find all workspaces for an org     |

### Access Control (IndexingConfig)

| Field                    | Value                                                                     |
|--------------------------|---------------------------------------------------------------------------|
| `access_check_object`    | `b2b_org:{orgUID}` (parent — workspaces have no separate FGA object type) |
| `access_check_relation`  | `auditor`                                                                 |
| `history_check_object`   | `b2b_org:{orgUID}`                                                        |
| `history_check_relation` | `writer`                                                                  |

### Search Behavior

| Field      | Contents           |
|------------|--------------------|
| `fulltext` | `workspace.Name`   |

### Parent References

| Ref                | Condition |
|--------------------|-----------|
| `b2b_org:{orgUID}` | Always    |

### Query Patterns

**"Which workspaces does org X have?"**
```
GET /query/resources?v=1&type=org_workspace&tags=b2b_org_uid:{orgUID}
```

> All queries are enforced by an FGA `auditor` check on `b2b_org:{uid}`.

---

## Org Workspace Project

**Object type:** `org_workspace_project`

**NATS subject:** `lfx.index.org_workspace_project`

**Source struct:** `internal/domain/model/workspace.go` — `WorkspaceProject` (one document per workspace-project association)

**Trigger:** Any write that adds or removes a project from a workspace — `POST /b2b_orgs/{uid}/workspaces/{workspace_uid}/projects`, `POST /b2b_orgs/{uid}/workspaces/{workspace_uid}/projects/bulk`, `DELETE /b2b_orgs/{uid}/workspaces/{workspace_uid}/projects/{project_uid}`. Also triggered on workspace delete (cascade: one delete event per former association).

**Note:** `ObjectID` is `"{workspaceUID}/{projectUID}"` (composite key, see `WorkspaceProject.AssociationID`). Each association is a separate indexed document.

**Known limitation (enrichment staleness):** Project `name`, `slug`, and `sfid` stored in each document are a write-time snapshot captured via Salesforce SOQL when the project was added. If a project is later renamed or re-slugged in Salesforce, the stored copy becomes stale until the association is re-added. There is no read-path re-enrichment.

### Payload Fields

| Field                   | Type      | Description                                                |
|-------------------------|-----------|------------------------------------------------------------|
| `b2b_org_uid`           | string    | Org UID (owner of the workspace)                           |
| `b2b_org_workspace_uid` | string    | Workspace UID                                              |
| `project_uid`           | string    | v2 project UUID                                            |
| `project_sfid`          | string    | Salesforce Project__c.Id (write-time snapshot)             |
| `project_slug`          | string    | Project URL slug (write-time snapshot)                     |
| `project_name`          | string    | Project display name (write-time snapshot)                 |
| `created_by`            | string    | Principal who added the project to this workspace          |
| `updated_by`            | string    | Principal who last updated this association                |
| `created_at`            | timestamp | Time the project was added (RFC3339)                       |
| `updated_at`            | timestamp | Time this association was last updated (RFC3339)           |
| `collection_updated_at` | timestamp | `WorkspaceProjects.UpdatedAt` — last time any association in this workspace changed |

### Tags

| Tag Format                          | Example                                | Purpose                                            |
|-------------------------------------|----------------------------------------|----------------------------------------------------|
| `{workspaceUID}/{projectUID}`       | `550e8400-.../abc-123...`              | Direct lookup by association (composite key)        |
| `b2b_org_uid:{orgUID}`              | `b2b_org_uid:0012M00002qnukOQAQ`       | Find all associations for an org                   |
| `b2b_org_workspace_uid:{wsUID}`     | `b2b_org_workspace_uid:550e8400-...`   | Find all projects in a workspace                   |
| `project_uid:{projectUID}`          | `project_uid:abc-123...`               | Find all workspaces containing a project (by UID)  |
| `project_sfid:{projectSFID}`        | `project_sfid:a2T2M000000ABCdUAG`      | Find all workspaces containing a project (by SFID) |
| `project_slug:{slug}`               | `project_slug:cncf`                    | Find all workspaces containing a project (by slug) |

### Access Control (IndexingConfig)

| Field                    | Value                                                                     |
|--------------------------|---------------------------------------------------------------------------|
| `access_check_object`    | `b2b_org:{orgUID}` (parent — associations have no separate FGA object type)|
| `access_check_relation`  | `auditor`                                                                 |
| `history_check_object`   | `b2b_org:{orgUID}`                                                        |
| `history_check_relation` | `writer`                                                                  |

### Search Behavior

| Field            | Contents                                     |
|------------------|----------------------------------------------|
| `sort_name`      | `strings.ToLower(projectName)`               |
| `name_aliases`   | `[projectName]`                              |
| `fulltext`       | `projectName + " " + projectSlug`            |

### Parent References

| Ref                          | Condition |
|------------------------------|-----------|
| `org_workspace:{wsUID}`      | Always    |
| `b2b_org:{orgUID}`           | Always    |

### Query Patterns

**"Which projects are in workspace W?"**
```
GET /query/resources?v=1&type=org_workspace_project&tags=b2b_org_workspace_uid:{wsUID}
```

**"Which workspaces contain project Y?"**
```
GET /query/resources?v=1&type=org_workspace_project&tags=project_uid:{projectUID}
```

> All queries are enforced by an FGA `auditor` check on `b2b_org:{uid}`.

---

## NATS RPC Endpoints

