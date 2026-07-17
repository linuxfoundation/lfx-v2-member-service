# FGA Contract — Member Service

This document is the authoritative reference for all messages the member service sends to the fga-sync service, which writes and deletes [OpenFGA](https://openfga.dev/) relationship tuples to enforce access control.

The full OpenFGA type definitions (relations, schema) for all object types are defined in the [platform model](https://github.com/linuxfoundation/lfx-v2-helm/blob/main/charts/lfx-platform/templates/openfga/model.yaml).

**Update this document in the same PR as any change to FGA message construction.**

---

## Object Types

- [B2B Org](#b2b-org)
- [Project Membership](#project-membership)
- [Org Workspace](#org-workspace) — no FGA messages; access inherited from parent `b2b_org`

---

## Message Format

All messages use the generic FGA message format on the following NATS subjects:

| Subject | Used for |
|---|---|
| `lfx.fga-sync.update_access` | Create and update operations |
| `lfx.fga-sync.delete_access` | Delete operations |
| `lfx.fga-sync.member_put` | Grant a user a relation on an object |
| `lfx.fga-sync.member_remove` | Revoke a user's relation on an object |

---

## B2B Org

**Source struct:** `internal/domain/model/b2b_org.go` — `B2BOrg`

**Synced on:** create, update, reparent, delete of a B2B org.

### Access Config

| Field | Value |
|---|---|
| `object_type` | `b2b_org` |
| `public` | `false` |

### Relations

| Relation | Value | Condition |
|---|---|---|
| `global_org_admin` | `"team:{globalOrgAdminTeamUID}"` | On create only, when `globalOrgAdminTeamUID` is non-empty |
| `parent` | `"b2b_org:{ParentUID}"` | When `ParentUID` changes; empty clears the tuple |
| `child` | `["b2b_org:{child_uid}", ...]` | Updated on old/new parent when `ParentUID` changes |
| `writer` | LFID username string (one per accepted writer, e.g. `"alice"`) | When org settings are updated with a non-nil writers field |
| `auditor` | LFID username string (one per accepted auditor) | When org settings are updated with a non-nil auditors field |

> `parent` and `child` relations are always excluded from `update_access` via `ExcludeRelations` and managed by separate reparenting messages.
>
> `writer` and `auditor` are excluded from `update_access` when the caller passes `nil` for that field (preserve existing tuples). When the caller passes an explicit slice (even empty), the full-sync runs and revokes any tuples not in the new list. Pending invites (entries without a resolved username) do not produce FGA tuples.

### Delete

On delete, only `uid` is sent — all FGA tuples for `b2b_org:{uid}` are removed by the fga-sync service.

---

## Project Membership

The member service issues two kinds of FGA messages for `project_membership`:

### 1. Membership references (`update_access`)

**Source struct:** `internal/domain/model/membership.go` — `ProjectMembership`

**Subject:** `lfx.fga-sync.update_access`

Sets the parent object references. `key_contact` is always excluded from this message — it is managed separately by the key-contact write path.

| Relation | Value | Condition |
|---|---|---|
| `b2b_org` | `"b2b_org:{B2BOrgUID}"` | When `B2BOrgUID` is non-empty |
| `project` | `"project:{ProjectUID}"` | When `ProjectUID` is non-empty |

> **`ExcludeRelations`**
> - `BuildProjectMembershipFGAMessage` (authoritative/default publish path): contains only `"key_contact"`. If `B2BOrgUID` or `ProjectUID` is empty, that relation is reconciled to empty (tuple cleared).
> - `BuildProjectMembershipFGAMessagePreserveMissingRefs` (transient resolver-failure path in CDC/backfill): contains `"key_contact"` and appends `"b2b_org"`/`"project"` when those refs are empty, so unresolved parents are preserved while the rest of the object still reconciles.

### 2. Key contact relation (`member_put` / `member_remove`)

**Source struct:** `internal/domain/model/key_contact.go` — `KeyContact`

Manages the `key_contact` relation on `project_membership` objects.

| Relation | Value | Condition |
|---|---|---|
| `key_contact` | Contact's LFID username | On create/update via `member_put`; on delete/username-change via `member_remove`; on `invite_accepted` b2b_org event for matching contacts |

> **CDC upsert path:** the CDC consumer now resolves the LFID via `UserReader.UsernameByEmail` before publishing. If the email has no LFID, the `username` remains empty and the grant is skipped (pending until the user accepts an invite).

> **CDC delete path:** when a `Project_Role__c` DELETE event arrives, `username` is empty (not available from the CDC payload). The fga-sync service performs cleanup by object-id when `username` is empty. Org-dashboard access is also not revoked on CDC delete (the pre-deletion `B2BOrgUID` + email are unavailable after Salesforce removes the record); revocation only happens via the API delete endpoint.

---

## Org Workspace

Workspace CRUD operations (`POST/PUT/DELETE /b2b_orgs/{uid}/workspaces/…`) and workspace-project mutations (`POST/DELETE /b2b_orgs/{uid}/workspaces/{uid}/projects/…`) emit **no FGA messages**. Workspaces have no dedicated FGA object type. Access control is enforced entirely through the parent `b2b_org` — the indexer's `access_check_object` and `history_check_object` both point to `b2b_org:{orgUID}`.

---

## Triggers

| Operation | Object Type | Subject | Notes |
|---|---|---|---|
| Create B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Sets `global_org_admin` tuple |
| Update B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Always sent |
| CDC `AccountChangeEvent` | `b2b_org` | `lfx.fga-sync.update_access` | Same as update; `globalOrgAdminTeamUID` always set (not create-only) |
| Reparent B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Up to 3 messages: org's own `parent`, old parent's `child` list, new parent's `child` list |
| Delete B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Stub org (uid only); fga-sync handles cleanup |
| CDC `AccountChangeEvent` (delete) | `b2b_org` | `lfx.fga-sync.update_access` | Same as delete |
| Update org settings (`PUT /settings`) | `b2b_org` | `lfx.fga-sync.update_access` | `writer`/`auditor` relations; nil param = preserve existing tuples, explicit (even `[]`) = replace |
| Add/update/delete settings user | `b2b_org` | `lfx.fga-sync.update_access` | Emitted by `AddPrincipal`, `UpdatePrincipalRole`, `DeletePrincipal` and `invite_accepted` promotion |
| Update project membership | `project_membership` | `lfx.fga-sync.update_access` | Sets `b2b_org` + `project` refs; excludes `key_contact` |
| CDC `AssetChangeEvent` | `project_membership` | `lfx.fga-sync.update_access` | Same as update |
| Create key contact | `project_membership` | `lfx.fga-sync.member_put` | Only when contact has a resolved LFID username |
| Update key contact (username change) | `project_membership` | `lfx.fga-sync.member_remove` + `lfx.fga-sync.member_put` | Revokes old username, grants new username |
| CDC `Project_Role__ChangeEvent` | `project_membership` | `lfx.fga-sync.member_put` | LFID resolved via `UserReader.UsernameByEmail`; skipped when email has no LFID |
| Delete key contact | `project_membership` | `lfx.fga-sync.member_remove` | Always sent when username is known |
| CDC `Project_Role__ChangeEvent` (delete) | `project_membership` | `lfx.fga-sync.member_remove` | `username` is empty — fga-sync cleans up by object-id |
| `invite_accepted` b2b_org event | `project_membership` | `lfx.fga-sync.member_put` | One grant per key contact in the org whose email matches `recipient.email`; `username = accepted_by` |
