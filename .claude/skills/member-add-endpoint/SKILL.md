---
name: member-add-endpoint
description: Use when adding a new HTTP endpoint to the lfx-v2-member-service (b2b_org, b2b_org settings, project membership, key contacts, admin actions, or related membership resources). Covers the Goa design update, regeneration, handler implementation, test scaffolding, and the Heimdall ruleset update that authorization requires. Also use when modifying an existing endpoint's signature in a way that triggers a regen + ruleset update.
paths:
  - 'cmd/member-api/design/**'
  - 'cmd/member-api/service/**'
  - 'charts/lfx-v2-member-service/templates/ruleset.yaml'
allowed-tools: Read, Glob, Grep, Edit, Write, Bash
---

<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Add a New Member Service Endpoint

The lfx-v2-member-service uses Goa v3 for code generation. Every new endpoint flows through the design DSL, regenerates the HTTP layer, gets implemented in the service struct, gets tested, and must be added to the Heimdall ruleset for authorization. Skipping the ruleset step ships an unauthorized endpoint.

For the broader development reference (Makefile targets, JWT/Heimdall flow, OpenFGA model and ruleset, error mapping), see `.claude/skills/member-service-dev/references/development-workflow.md`.

## Workflow

1. **Update the Goa design** in `cmd/member-api/design/membership.go`. Add the new `Method` block with payload, result, and HTTP binding. Declare `dsl.Security(JWTAuth)` unless the endpoint is intentionally public (none currently are).

2. **Regenerate the API layer**:

   ```bash
   make apigen
   ```

   This writes to `gen/`. Never hand-edit `gen/`; those files are overwritten on every regen.

3. **Implement the method** on the `membershipServicesrvc` struct in `cmd/member-api/service/membership_service.go`. The struct holds `storage` (a `port.MemberReader`, also used for the readyz check and key-contact reads), `auth`, `b2bOrgReader`, `projectMembershipReader`, `b2bOrgSettingsReader`, `b2bOrgWriter`, `keyContactWriter`, `orgSettingsWriter`, and `backfillRunner`. Route reads through the matching reader port and writes through the matching writer use-case; do not put business logic in the handler. Return early on validation errors with `pkg/errors` domain types so `cmd/member-api/service/error.go` can map them. Mutating handlers run the `If-Match` → LFX-ETag guard before writing (mirror `UpdateKeyContact`), and publish indexer/FGA events via the writer/orchestrator after a successful write.

4. **Add unit tests** in `cmd/member-api/service/membership_service_test.go` following the table-driven shape already used:

   ```go
   func TestEndpoint(t *testing.T) {
       tests := []struct {
           name       string
           payload    *membershipservice.Payload
           wantErr    bool
       }{
           // cases
       }
       for _, tt := range tests {
           t.Run(tt.name, func(t *testing.T) {
               // ...
           })
       }
   }
   ```

   Each function has exactly one corresponding test function with multiple cases. Mock data dependencies via `internal/infrastructure/mock/`. Existing service tests usually build auth with `auth.NewJWTAuth(auth.JWTAuthConfig{MockLocalPrincipal: "test-user"})`; use `auth.MockJWTAuth` only when the test needs explicit authenticator expectations.

5. **Update the Heimdall ruleset** in `charts/lfx-v2-member-service/templates/ruleset.yaml`. Add an `openfga_check` entry for the new path and verb, wrapped in the `{{- if .Values.openfga.enabled }}` / `allow_all` fallback that every other rule uses. The current authorization shape is:

   - `GET /b2b_orgs/{uid}` and `GET /b2b_orgs/{uid}/settings` require `auditor` on `b2b_org:{uid}`.
   - `PUT /b2b_orgs/{uid}` and `PUT /b2b_orgs/{uid}/settings` require `writer` on `b2b_org:{uid}`.
   - `POST /b2b_orgs/{uid}/settings/users` and PUT/DELETE `/b2b_orgs/{uid}/settings/users/{email}` (per-principal settings users) require `writer` on `b2b_org:{uid}`.
   - `POST /b2b_orgs` and `POST /admin/reindex` require `member` on `team:{{ .Values.app.globalOrgAdminTeamName }}` (machine callers).
   - `GET /b2b_orgs/member-tiers/{username}` requires `member` on `team:{{ .Values.app.memberTiersCallerTeamName }}` (machine callers; default `member_tiers_caller`, empty falls back to `globalOrgAdminTeamName`).
   - `GET /project_memberships` (list, no path param — PCC-1610) requires `auditor_guard` on `project:{project_uid}`, read from a query param via `{{ "{{- with .Request.URL.Query.project_uid }}{{ index . 0 }}{{ end -}}" }}` rather than a path capture — `auditor_guard`, not the bare `auditor` relation, since `project_membership.auditor` itself resolves through `auditor_guard from project` (see `ARCHITECTURE.md`). **`index`, not bare field access, is required here:** `.Request.URL.Query` is Heimdall's `url.Values` (`map[string][]string`), unlike `.Request.URL.Captures` (`map[string]string`) that every path-capture-keyed rule in this chart uses — bare `.Request.URL.Query.project_uid` renders Go's slice syntax (`project:[a27394a3-...]`), which matches no OpenFGA tuple and denies every request when `openfga.enabled` is true. `index .Request.URL.Query.project_uid 0` selects the first value, matching Goa's own generated decoder (`r.URL.Query().Get`, also first-value). **`with`, not bare `index`, is required too:** `project_uid` is `dsl.Required`, so Goa would normally 400 a request missing it — but this template executes before that validation runs, and bare `index` on the nil slice from an absent key is a template *execution error* (not an empty string), which Heimdall surfaces as a 5xx instead of letting the request reach Goa's 400. Guarding with `with` makes an absent or empty value skip the `index` entirely so the object interpolates as `"project:"`, matching no tuple — denying (fails closed) instead of crashing. This is the first rule in this chart keyed off a query param rather than a path capture; verify against a deployed Heimdall (or a rendered-template assertion) before relying on new query-param-keyed rules, and note `lfx-v2-meeting-service`'s `GET /itx/meeting_count` rule should be checked against the same bare-field-access and bare-`index` pitfalls.
   - `GET /project_memberships/{uid}` requires `auditor` on `project_membership:{uid}`.
   - `GET/POST/PUT/DELETE /project_memberships/{membership_uid}/key_contacts[/{uid}]` require `auditor` (reads) / `writer` (mutations) on `project_membership:{membership_uid}`; the POST rule also runs the `json_content_type` platform authorizer.

   New per-object routes should follow the same relation pattern (`auditor` for reads, `writer` for writes) keyed on the object's own type, unless the endpoint genuinely needs different semantics. Also add the route to `httproute.yaml` so the gateway forwards it.

6. **Verify**:

   ```bash
   make fmt     # if Go files changed
   make lint    # lint when practical
   make test    # unit suite with race and coverage profile
   make build   # ensures regen + impl compile
   ```

## Error mapping reminder

Return the existing domain errors so the mapper in `cmd/member-api/service/error.go` produces the right HTTP status:

- `NotFound` -> 404
- `Validation` -> 400
- `Conflict` -> 409
- `ServiceUnavailable` -> 503
- `PreconditionFailed` -> 412
- `NotImplemented` -> 501
- anything else -> 500

A handler can only return a status its Goa method declares via `dsl.Error`/`dsl.Response`, so declare the matching error in the design. Do not return raw `error` values from new methods; wrap them through the domain error types.

## Anti-patterns

- Shipping a Goa method without updating `ruleset.yaml` in the same PR. Heimdall will reject the request (or worse, allow it under a stale rule) and the endpoint will be effectively broken.
- Editing files under `gen/` directly. They are clobbered on the next `make apigen`.
- Adding new dependencies to `membershipServicesrvc` for logic that belongs in an existing port or use-case. The service struct should stay thin; business logic belongs in a reader port (`b2bOrgReader`, `projectMembershipReader`, `b2bOrgSettingsReader`) or a writer use-case (`b2bOrgWriter`, `keyContactWriter`, `orgSettingsWriter`), or a sibling use-case type if one is genuinely needed.
  **Exemption:** `ListProjectMemberships` (PCC-1610) calls `s.storage.ListMembershipsForProject`
  (`port.MemberReader`) directly rather than through `projectMembershipReader`, and does its
  `b2b_org_uid` page-walk/dedupe/truncation logic in the handler itself. This was a deliberate
  trade-off, not an oversight — see the doc comment at `membership_service.go:239` — because this
  endpoint has no second caller today that would justify extracting a port method. Revisit once a
  second caller needs the same walk logic; don't let that become a precedent for other handlers to
  skip their reader port without the same justification.
- Adding the `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` path into production code paths. That env var is local-dev only.

## References

- `../member-service-dev/references/development-workflow.md`: Makefile targets, auth flow, OpenFGA model and ruleset shape, error mapping table, service architecture notes.
- `cmd/member-api/design/membership.go`: Goa DSL entry point.
- `cmd/member-api/service/membership_service.go`: service struct and existing method implementations.
- `cmd/member-api/service/error.go`: domain error to HTTP status mapping.
- `charts/lfx-v2-member-service/templates/ruleset.yaml`: Heimdall authorization rules.
