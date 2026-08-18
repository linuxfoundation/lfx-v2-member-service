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

This matters for revocation. `Access` returning success means the local NATS client accepted the message onto the connection — it does **not** by itself mean the broker received it, and it never means OpenFGA has converged. Only the flushed paths below — the API key-contact deletion and the CDC genuine-delete purge — confirm broker receipt; every other path's success is client-side acceptance only. Between any successful publish and fga-sync applying the tuple change there is an asynchronous convergence window in which the old access is still live. Callers that need certainty must re-check against OpenFGA rather than infer it from a 2xx.

Waiting for a reply would not fix this and would actively mislead. Once the membership subjects are captured by a JetStream stream, a request on those subjects is answered by the broker acknowledging storage, not by fga-sync reporting completion — so a synchronous caller would receive a fast success that proves nothing about convergence.

**Revocations confirm delivery, not convergence.** The API key-contact deletion path flushes the NATS connection after publishing `member_remove`. Publishing alone only buffers the message locally, so a crash immediately afterwards could discard a revocation the API had already reported as done. The flush closes that window by confirming the server received the message. It still does not wait for fga-sync or for OpenFGA. A flush failure is reported as an error rather than as a successful deletion.

The CDC delete paths flush for the same reason: the key-contact delete before it clears the recorded grant address, and the genuine-delete `delete_access` purge for `b2b_org` / `project_membership`. A purge is the extreme case — the Salesforce record is gone, so neither the next CDC event nor `/admin/reindex` will re-emit it, and without the flush an unreachable broker drops it with no error to propagate at all. Unconfirmed delivery there is treated exactly like a publish failure: the error propagates and a recovery marker is written (see [Delete_access failure marker](./cdc-consumer.md#delete_access-failure-marker)).

Grants and upserts do not flush — nor does the email-change revocation that shares the deletion path's publication helper. These do not carry equal risk on publish failure: a failed grant/update is repairable via `POST /admin/reindex`, which re-publishes current state. A failed email-change or CDC removal is not — reindex only re-asserts the current grant, it never re-issues a `member_remove` for a superseded username, so a dangling grant from either path needs a targeted FGA sync or a manual re-send of the remove message (see `docs/cdc-consumer.md`'s `fga_revoke_failed_dangling_tuple` entry for the CDC case).

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
| `auditor` | `["team:{lfStaffTeamName}#member"]` | On every full-sync publish — create, update, CDC upsert, settings PUT, per-principal settings mutations and `/admin/reindex`. Not on reparenting/child-list messages, and not on delete. One entry per configured team; only the LF staff team is configured by default. See [lf-team-auditor-grants.md](./lf-team-auditor-grants.md) |

> `parent` and `child` relations are always excluded from `update_access` via `ExcludeRelations` and managed by separate reparenting messages.
>
> `writer` and `auditor` are excluded from `update_access` when the caller passes `nil` for that field (preserve existing tuples). When the caller passes an explicit slice (even empty), the full-sync runs and revokes any tuples not in the new list. Pending invites (entries without a resolved username) do not produce FGA tuples.
>
> Excluding `auditor` and writing the auditor team references in the same message is not a contradiction: fga-sync consults `ExcludeRelations` only in its delete branch. The exclusion suppresses reaping of the per-user auditor tuples the caller is not managing, while the team references are still written.

### Delete

On delete the service publishes `delete_access` to `lfx.fga-sync.delete_access`, carrying only `uid`:

```json
{ "object_type": "b2b_org", "operation": "delete_access", "data": { "uid": "0014100000Td9x0AAB" } }
```

There is no relation or exclusion field, so the message cannot be scoped to part of an object — which is precisely why it must only ever be sent for an object that is genuinely gone.

**Genuine deletions only.** The CDC consumer also routes records that are merely *absent* from its periodic query to a delete handler for index convergence. That path publishes no FGA message: an organization missing from the query may still exist, since a lapsed membership is enough to drop it, and purging it would revoke a live customer's administrators. The two cases use separate entry points (`handleAccountDelete` vs `handleAccountAbsent`) so the distinction is structural rather than a condition someone can forget. See [LFXV2-3034](https://linuxfoundation.atlassian.net/browse/LFXV2-3034).

**A purge does not leave zero tuples.** fga-sync never deletes a tuple whose subject begins with `team:`, so team-subject grants — including the staff-team reader granted to every org under LFXV2-2937 — survive and cannot be reaped by any service code path. They confer access to an object that no longer resolves, so they are inert, but any verification that asserts an empty tuple set for a deleted object will report a correct implementation as broken. Removing them requires a one-off script.

**A purge that is not confirmed delivered propagates and is durably recorded.** The CDC delete handlers return the publish error rather than swallowing it (`dispatchEntity` in `internal/service/cdc_consumer.go` logs it and moves on to the next ID, so the batch is unaffected), and they flush afterwards so that a broker that never received the purge is treated as a failure rather than as success. On either failure, the UID is written to the CDC repair KV bucket under a delete-specific reindex type (`ReindexTypeB2BOrgDeleteAccess` / `ReindexTypeProjectMembershipDeleteAccess`) so an operator can find and manually re-purge it. This is deliberately not wired into `/admin/reindex`'s automated drain: that path re-fetches and re-upserts the *live* Salesforce record, which cannot repair a purge — the record is gone, so the fetch reports "not found" and no `delete_access` is re-emitted.

**If that marker write also fails, the same event is retried in-process** before any later event is consumed — the only remaining route once a purge has been neither delivered nor recorded. This protects the channel's first event while the process remains alive; retries back off from 100 ms to 30 s, and cancellation exits without a cursor save. See [In-process authorization retry](./cdc-consumer.md#in-process-authorization-retry).

**A restored record rebuilds the access tuples the purge withdrew.** For `b2b_org`, the consumer republishes direct writers/auditors from `org-settings`, configured team references, the restored parent, and the org's authoritative child list. For `project_membership`, it republishes the structural `b2b_org`/`project` references and current key contacts from Salesforce `Project_Role__c`. `key-contact-grants` is refreshed by successful contact publishes but is not restoration truth because it can be incomplete for historical grants. Successful restore publishes are flushed before the replay cursor advances; batch-fetch, source, transient resolver/lookup, publish, current-grant recording, and flush failures retry the same event in-process. Structurally absent Salesforce associations and definitively unknown projects advance with an error log because retry cannot create missing source data. See [Grant restoration on UNDELETE](./cdc-consumer.md#grant-restoration-on-undelete).

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
| Delete B2B org | `b2b_org` | `lfx.fga-sync.delete_access` | Stub org (uid only). Withdraws the org's tuples; `team:`-subject grants survive by design |
| CDC `AccountChangeEvent` (delete) | `b2b_org` | `lfx.fga-sync.delete_access` | Same as delete. `DELETE` and `GAP_DELETE` only |
| CDC `AccountChangeEvent` (`UNDELETE` / `GAP_UNDELETE`) | `b2b_org` | `lfx.fga-sync.update_access` | Full-sync of accepted writers/auditors read from `org-settings`, rebuilding what the purge withdrew. Additive only; flushed before cursor advancement |
| CDC `AccountChangeEvent` (absent from SOQL) | `b2b_org` | *(none)* | Index tombstone only. The org may still exist — see [Delete](#delete) |
| Update org settings (`PUT /settings`) | `b2b_org` | `lfx.fga-sync.update_access` | `writer`/`auditor` relations; nil param = preserve existing tuples, explicit (even `[]`) = replace. Also carries the auditor team references |
| Add/update/delete settings user | `b2b_org` | `lfx.fga-sync.update_access` | Emitted by `AddPrincipal`, `UpdatePrincipalRole`, `DeletePrincipal` and `invite_accepted` promotion — all share the settings publish path, so all carry the auditor team references |
| Update project membership | `project_membership` | `lfx.fga-sync.update_access` | Sets `b2b_org` + `project` refs; excludes `key_contact` |
| CDC `AssetChangeEvent` | `project_membership` | `lfx.fga-sync.update_access` | Same as update |
| CDC `AssetChangeEvent` (delete) | `project_membership` | `lfx.fga-sync.delete_access` | Withdraws the membership's tuples. `DELETE` and `GAP_DELETE` only |
| CDC `AssetChangeEvent` (`UNDELETE` / `GAP_UNDELETE`) | `project_membership` | `lfx.fga-sync.member_put` | One per current Salesforce key contact with a resolved LFID; successful publishes refresh `key-contact-grants` and are flushed before cursor advancement |
| CDC `AssetChangeEvent` (absent from SOQL) | `project_membership` | *(none)* | Index tombstone only. `Product2.Family` may simply have flipped off "Membership" |
| Create key contact | `project_membership` | `lfx.fga-sync.member_put` | Only when contact has a resolved LFID username |
| Update key contact (username change) | `project_membership` | `lfx.fga-sync.member_put` + `lfx.fga-sync.member_remove` | Grants the new username before revoking the old one, so no window leaves the contact without access. Skipped entirely when the username resolves unchanged |
| CDC `Project_Role__ChangeEvent` | `project_membership` | `lfx.fga-sync.member_put` (+ `lfx.fga-sync.member_remove`) | LFID resolved via `UserReader.UsernameByEmail`; skipped when email has no LFID. A `member_remove` for the previously recorded `{membership_uid, username}` follows the put when the grant target changed (Salesforce-side reparent, or a changed LFID) |
| Delete key contact | `project_membership` | `lfx.fga-sync.member_remove` | Sent when a username is known, falling back to the one recorded in `key-contact-grants` when live LFID lookup yields nothing; the connection is flushed afterwards to confirm delivery |
| CDC `Project_Role__ChangeEvent` (delete) | `project_membership` | `lfx.fga-sync.member_remove` | `membership_uid` + `username` recovered from `key-contact-grants`. With no recorded grant, falls back to the contact's own SFID and an empty username, which fga-sync rejects (revokes nothing) |
| `invite_accepted` b2b_org event | `project_membership` | `lfx.fga-sync.member_put` | One grant per key contact in the org whose email matches `recipient.email`; `username = accepted_by` |
