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

## Delivery Semantics

**All FGA publication from the member service is asynchronous.** No FGA flow uses NATS request/reply, and no flow waits for or inspects an fga-sync response. The publisher API has no synchronous selector, so a callsite cannot opt back into request/reply.

This matters for revocation. `Access` returning success means the local NATS client accepted the message onto the connection — it does **not** by itself mean the broker received it, and it never means OpenFGA has converged. Only the flushed key-contact deletion path below confirms broker receipt; every other path's success is client-side acceptance only. Between any successful publish and fga-sync applying the tuple change there is an asynchronous convergence window in which the old access is still live. Callers that need certainty must re-check against OpenFGA rather than infer it from a 2xx.

Waiting for a reply would not fix this and would actively mislead. Once the membership subjects are captured by a JetStream stream, a request on those subjects is answered by the broker acknowledging storage, not by fga-sync reporting completion — so a synchronous caller would receive a fast success that proves nothing about convergence.

**One exception confirms delivery, not convergence.** The API key-contact deletion path flushes the NATS connection after publishing `member_remove`. Publishing alone only buffers the message locally, so a crash immediately afterwards could discard a revocation the API had already reported as done. The flush closes that window by confirming the server received the message. It still does not wait for fga-sync or for OpenFGA. A flush failure is reported as an error rather than as a successful deletion.

No other membership publication flushes — not the email-change revocation that shares the deletion path's publication helper, not CDC removals, and no grants. These do not carry equal risk on publish failure: a failed grant/update is repairable via `POST /admin/reindex`, which re-publishes current state. A failed email-change or CDC removal is not — reindex only re-asserts the current grant, it never re-issues a `member_remove` for a superseded username, so a dangling grant from either path needs a targeted FGA sync or a manual re-send of the remove message (see `docs/cdc-consumer.md`'s `fga_revoke_failed_dangling_tuple` entry for the CDC case).

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
| `auditor` | `["team:{lfStaffTeamName}#member", "team:{lfContractorTeamName}#member"]` | On every full-sync publish — create, update, CDC upsert, settings PUT, per-principal settings mutations and `/admin/reindex`. Not on reparenting/child-list messages, and not on delete. See [lf-team-auditor-grants.md](./lf-team-auditor-grants.md) |

> `parent` and `child` relations are always excluded from `update_access` via `ExcludeRelations` and managed by separate reparenting messages.
>
> `writer` and `auditor` are excluded from `update_access` when the caller passes `nil` for that field (preserve existing tuples). When the caller passes an explicit slice (even empty), the full-sync runs and revokes any tuples not in the new list. Pending invites (entries without a resolved username) do not produce FGA tuples.
>
> Excluding `auditor` and writing the auditor team references in the same message is not a contradiction: fga-sync consults `ExcludeRelations` only in its delete branch. The exclusion suppresses reaping of the per-user auditor tuples the caller is not managing, while the team references are still written.

### Delete

On delete, only `uid` is sent, and the message asserts **no** team references — neither `global_org_admin` nor the auditor teams.

Note that fga-sync does not remove every tuple for `b2b_org:{uid}`. It never deletes a tuple whose subject begins with `team:`, so team-subject grants survive the delete and cannot be reaped by any service code path. That is why the delete message stops asserting them: a team reference written for an org that no longer exists is a permanent orphan on a dead object. Removing existing ones requires a one-off script.

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

> **CDC delete path:** a `Project_Role__c` DELETE event carries only the key contact's own SFID, and the Salesforce record is already gone when it is handled — neither the parent membership nor the granted username can be re-read. Both are recovered from the `key-contact-grants` KV bucket, which records every `member_put` this service publishes; the `member_remove` targets `project_membership:{membership_uid}` with the recorded username, and the entry is deleted afterwards.
>
> A contact with no recorded grant (granted before that bucket existed) falls back to the contact's own SFID with an empty `username`. **fga-sync rejects a `member_remove` with an empty username outright and performs no cleanup** — it does *not* clean up by object-id, contrary to what this document previously stated (verified against `lfx-v2-fga-sync@v0.2.17` `handler_generic.go:455-457`). That fallback therefore revokes nothing and is logged with `fga_revoke_failed_dangling_tuple=true`; it is retained only so no case behaves worse than before the index existed.
>
> Org-dashboard access is also not revoked on CDC delete (the pre-deletion `B2BOrgUID` + email are unavailable after Salesforce removes the record); revocation only happens via the API delete endpoint.

---

## Org Workspace

Workspace CRUD operations (`POST/PUT/DELETE /b2b_orgs/{uid}/workspaces/…`) and workspace-project mutations (`POST/DELETE /b2b_orgs/{uid}/workspaces/{uid}/projects/…`) emit **no FGA messages**. Workspaces have no dedicated FGA object type. Access control is enforced entirely through the parent `b2b_org` — the indexer's `access_check_object` and `history_check_object` both point to `b2b_org:{orgUID}`.

---

## Triggers

| Operation | Object Type | Subject | Notes |
|---|---|---|---|
| Create B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Sets `global_org_admin` tuple + auditor team references |
| Update B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Always sent. Carries the auditor team references but no `global_org_admin`, which is create-only |
| CDC `AccountChangeEvent` | `b2b_org` | `lfx.fga-sync.update_access` | Same as update; `globalOrgAdminTeamUID` always set (not create-only) |
| Reparent B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Up to 3 messages: org's own `parent`, old parent's `child` list, new parent's `child` list |
| Delete B2B org | `b2b_org` | `lfx.fga-sync.update_access` | Stub org (uid only); no team references asserted. fga-sync cleans up non-team tuples only — `team:`-subject grants survive |
| CDC `AccountChangeEvent` (delete) | `b2b_org` | `lfx.fga-sync.update_access` | Same as delete |
| Update org settings (`PUT /settings`) | `b2b_org` | `lfx.fga-sync.update_access` | `writer`/`auditor` relations; nil param = preserve existing tuples, explicit (even `[]`) = replace. Also carries the auditor team references |
| Add/update/delete settings user | `b2b_org` | `lfx.fga-sync.update_access` | Emitted by `AddPrincipal`, `UpdatePrincipalRole`, `DeletePrincipal` and `invite_accepted` promotion — all share the settings publish path, so all carry the auditor team references |
| Update project membership | `project_membership` | `lfx.fga-sync.update_access` | Sets `b2b_org` + `project` refs; excludes `key_contact` |
| CDC `AssetChangeEvent` | `project_membership` | `lfx.fga-sync.update_access` | Same as update |
| Create key contact | `project_membership` | `lfx.fga-sync.member_put` | Only when contact has a resolved LFID username |
| Update key contact (username change) | `project_membership` | `lfx.fga-sync.member_put` + `lfx.fga-sync.member_remove` | Grants the new username before revoking the old one, so no window leaves the contact without access. Skipped entirely when the username resolves unchanged |
| CDC `Project_Role__ChangeEvent` | `project_membership` | `lfx.fga-sync.member_put` (+ `lfx.fga-sync.member_remove`) | LFID resolved via `UserReader.UsernameByEmail`; skipped when email has no LFID. A `member_remove` for the previously recorded `{membership_uid, username}` follows the put when the grant target changed (Salesforce-side reparent, or a changed LFID) |
| Delete key contact | `project_membership` | `lfx.fga-sync.member_remove` | Sent when a username is known, falling back to the one recorded in `key-contact-grants` when live LFID lookup yields nothing; the connection is flushed afterwards to confirm delivery |
| CDC `Project_Role__ChangeEvent` (delete) | `project_membership` | `lfx.fga-sync.member_remove` | `membership_uid` + `username` recovered from `key-contact-grants`. With no recorded grant, falls back to the contact's own SFID and an empty username, which fga-sync rejects (revokes nothing) |
| `invite_accepted` b2b_org event | `project_membership` | `lfx.fga-sync.member_put` | One grant per key contact in the org whose email matches `recipient.email`; `username = accepted_by` |
