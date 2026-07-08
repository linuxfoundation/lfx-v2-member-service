# LFX V2 Member Service

This repository contains the source code for the LFX v2 platform member service.

## Overview

The LFX v2 Member Service is a RESTful API service that provides membership data
within the Linux Foundation's LFX platform. It exposes endpoints for querying
project memberships and key contacts, write endpoints for managing B2B orgs
(create, update, access-control settings) and key contacts (create, update,
delete), and an admin reindex endpoint for pushing data into OpenSearch. Data is
sourced directly from Salesforce via SOQL queries, with a per-record NATS
Key-Value cache to minimise round-trips.

## File Structure

```bash
├── .github/                        # Github files
│   └── workflows/                  # Github Action workflow files
├── charts/                         # Helm charts for running the service in kubernetes
├── cmd/                            # Services (main packages)
│   └── member-api/                 # Member service API code
│       ├── design/                 # API design specifications (Goa)
│       ├── service/                # Service implementation
│       ├── main.go                 # Application entry point
│       └── http.go                 # HTTP server setup
├── gen/                            # Generated code from Goa design
├── internal/                       # Internal service packages
│   ├── domain/                     # Domain logic layer (business logic)
│   │   ├── model/                  # Domain models and entities
│   │   └── port/                   # Repository and service interfaces
│   ├── service/                    # Service logic layer (use cases)
│   ├── infrastructure/             # Infrastructure layer
│   │   ├── nats/                   # NATS KV cache and project RPC
│   │   ├── salesforce/             # Salesforce SOQL client and repositories
│   │   ├── project/                # ProjectResolver (UID ↔ slug ↔ SFID)
│   │   └── mock/                   # Mock implementations for testing
│   └── middleware/                 # HTTP middleware components
└── pkg/                            # Shared packages
```

## Key Features

- **RESTful API**: Endpoints for querying project memberships and key contacts,
  write endpoints for B2B orgs and key contacts, org access-control settings
  (writers/auditors), and an admin reindex endpoint.
- **Salesforce-backed**: All membership data is fetched directly from Salesforce
  via SOQL queries; no PostgreSQL dependency at runtime.
- **NATS KV cache**: Six buckets — `membership-cache` (Salesforce-backed records,
  6 h stale / 23 h expire / 24 h TTL); `member-service-cache` (sObject REST
  responses, no soft-TTL); `org-settings` (org ACL, no TTL); `pubsub-state`
  (CDC replay cursors); `org-workspaces` (workspace registry, no TTL);
  `org_workspace_projects` (workspace-project associations, no TTL).
- **Project ID resolution**: The `ProjectResolver` translates v2 project UIDs to
  Salesforce `Project__c.Id` values by chaining a NATS RPC to the project-service
  and a SOQL lookup, backed by the same KV cache.
- **NATS RPC endpoints**: Exposes request/reply subjects so other services can
  resolve identifiers without querying Salesforce directly:
  - `lfx.member.project-id-map.lookup` — v2 project UID → Salesforce `Project__c.Id`
  - `lfx.member.b2b_org_lookup` — validate a `b2b_org` id and return the canonical 18-char Account SFID
- **Clean Architecture**: Follows hexagonal architecture with clear separation of
  domain, service, and infrastructure layers.
- **Authorization**: JWT-based authentication with Heimdall middleware integration
  and OpenFGA fine-grained access control.
- **Health Checks**: Built-in `/livez` and `/readyz` endpoints.
- **Request Tracking**: Automatic request ID generation and propagation.
- **Structured Logging**: JSON-formatted logs with contextual information.

## API Endpoints

### Project membership

| Method | Path | Description |
|--------|------|-------------|
| GET | `/project_memberships/{uid}` | Get a project membership |

### Key contacts (nested under project_membership)

| Method | Path | Description |
|--------|------|-------------|
| GET | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Get a key contact |
| POST | `/project_memberships/{membership_uid}/key_contacts` | Create a key contact |
| PUT | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Update a key contact |
| DELETE | `/project_memberships/{membership_uid}/key_contacts/{uid}` | Remove a key contact |

### B2B orgs

| Method | Path | Description |
|--------|------|-------------|
| POST | `/b2b_orgs` | Create a B2B org from a Salesforce Account SFID |
| PUT | `/b2b_orgs/{uid}` | Partial update of a B2B org |
| GET | `/b2b_orgs/{uid}` | Get a B2B org |
| GET | `/b2b_orgs/{uid}/settings` | Get org access-control settings (writers, auditors) |
| PUT | `/b2b_orgs/{uid}/settings` | Update org access-control settings (writers, auditors) |

### Admin

| Method | Path | Description |
|--------|------|-------------|
| POST | `/admin/reindex` | Trigger a full or incremental reindex of cached entities into OpenSearch (requires global org-admin membership) |

### Utility

| Method | Path | Description |
|--------|------|-------------|
| GET | `/readyz` | Readiness check |
| GET | `/livez` | Liveness check |

> **Note:** The legacy `/members/*` and `/memberships/*` endpoints are no longer
> registered and return `404 Not Found`; use the replacement paths above.

## NATS API

In addition to the HTTP API, the service handles NATS request/reply subjects
that allow other services to resolve identifiers without querying Salesforce or
this service's HTTP layer directly.

### Project ID Map Lookup

Resolves a v2 project UUID to its corresponding Salesforce `Project__c.Id`.
Resolution is cached in the `membership-cache` KV bucket; a cache miss triggers
a NATS RPC to the project-service (to obtain the project slug) followed by a
Salesforce SOQL query.

| Field | Value |
|-------|-------|
| **Subject** | `lfx.member.project-id-map.lookup` |
| **Transport** | NATS core request/reply |

**Request body (JSON):**

```json
{"project_uid": "<v2 project UUID>"}
```

**Response — success (HTTP 200 equivalent):**

```json
{"project_sfid": "<Salesforce Project__c.Id>"}
```

**Response — not found:**

```json
{"error": "project not found"}
```

**Response — bad request:**

```json
{"error": "project_uid is required"}
```

The reply is always valid JSON. Check for the presence of the `"error"` key to
detect failure.

### B2B Org Lookup

Validates that an organization id resolves to an indexed `b2b_org` and returns
the canonical 18-char Salesforce Account SFID. Used by write-path validators in
consumer services (e.g. committee-service) per LFXV2-2400. Resolution chains:
sObject cache → Salesforce Account fetch via `GetB2BOrg`.

| Field | Value |
|-------|-------|
| **Subject** | `lfx.member.b2b_org_lookup` |
| **Transport** | NATS core request/reply |

**Request body (JSON):**

```json
{"id": "<b2b_org uid or 15/18-char Account SFID>"}
```

**Response — success:**

```json
{"id": "<canonical 18-char Account SFID>"}
```

**Response — not found:**

```json
{"error": "b2b org not found"}
```

**Response — bad request:**

```json
{"error": "id is required"}
```

**Response — lookup failure (infrastructure error):**

```json
{"error": "b2b org lookup failed"}
```

The reply is always valid JSON. Check for the presence of the `"error"` key to
detect failure. A not-found response means the id does not resolve to a `b2b_org`
(e.g. CDP UUID, unknown SFID); infrastructure failures return a distinct error
message.

### B2B Org UID Resolution

As of LFXV2-2049, `b2b_org.uid` equals the canonical 18-char Salesforce Account
SFID. Callers that already hold a verified Account SFID can use it directly as
the `b2b_org` UID without further translation. When a caller receives an opaque
`organization.id` (15/18-char SFID or other shape), use `lfx.member.b2b_org_lookup`
to confirm the org exists and obtain the normalized 18-char SFID.

## Development

To contribute to this repository:

1. Fork the repository.
2. Commit your changes to a feature branch in your fork. Ensure your commits
   are signed with the [Developer Certificate of Origin
   (DCO)](https://developercertificate.org/).
   You can use the `git commit -s` command to sign your commits.
3. Submit your pull request.

### Building

```bash
make apigen    # Generate Goa API code
make build     # Build the member-api binary
make test      # Run tests
```

### Running Locally

```bash
# With NATS and Salesforce credentials
export NATS_URL=nats://localhost:4222
export SF_INSTANCE_URL=https://linuxfoundation.my.salesforce.com
export SF_CLIENT_ID=<connected-app-client-id>
export SF_CLIENT_SECRET=<connected-app-client-secret>
./bin/member-api

# With mock data (no NATS or Salesforce required)
REPOSITORY_SOURCE=mock AUTH_SOURCE=mock ./bin/member-api

# With mock auth only (NATS + Salesforce still required)
export JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=local-dev-user
./bin/member-api
```

### Salesforce Credentials

The service reads credentials from environment variables. A pre-existing
Kubernetes Secret is required in production; see the Helm chart `values.yaml`
`salesforce.secrets` stanza for configuration.

| Variable | Description | Required |
|----------|-------------|----------|
| `SF_INSTANCE_URL` | Salesforce instance URL | Yes |
| `SF_CLIENT_ID` | Connected-app consumer key | Yes |
| `SF_CLIENT_SECRET` | Consumer secret (username/password or client-credentials flow) | Conditional |
| `SF_USERNAME` | Salesforce username (username/password or JWT flow) | Conditional |
| `SF_PASSWORD` | Salesforce password (username/password flow) | Conditional |
| `SF_SECURITY_TOKEN` | Security token appended to password | No |
| `SF_CONSUMER_RSA_PEM` | PEM-encoded RSA private key (JWT bearer flow) | Conditional |
| `SF_API_VERSION` | Salesforce REST API version (default: `v60.0`) | No |

At least one complete authentication flow must be configured:

- **JWT bearer**: `SF_USERNAME` + `SF_CONSUMER_RSA_PEM`
- **Username/password**: `SF_USERNAME` + `SF_PASSWORD` + `SF_CLIENT_SECRET`
- **Client-credentials**: `SF_CLIENT_SECRET` (without `SF_USERNAME`)

## License

Copyright The Linux Foundation and each contributor to LFX.

This project's source code is licensed under the MIT License. A copy of the
license is available in `LICENSE`.

This project's documentation is licensed under the Creative Commons Attribution
4.0 International License (CC-BY-4.0). A copy of the license is available in
`LICENSE-docs`.