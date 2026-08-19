<!-- Copyright The Linux Foundation and each contributor to LFX. -->
<!-- SPDX-License-Identifier: MIT -->

# Salesforce Integration

Salesforce REST is the source of truth for all membership data in this service.
SOQL queries flow through `github.com/k-capehart/go-salesforce/v3`. Cached
records and the project-UID-to-Salesforce-ID resolver are documented in
[`salesforce-cache.md`](salesforce-cache.md).

## Environment Variables

### Service configuration

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `PORT` | HTTP listen port | `8080` | No |
| `NATS_URL` | NATS server URL | `nats://localhost:4222` | No |
| `NATS_TIMEOUT` | NATS connection timeout | `10s` | No |
| `NATS_MAX_RECONNECT` | Max NATS reconnect attempts | `3` | No |
| `NATS_RECONNECT_WAIT` | Wait between reconnects | `2s` | No |
| `LOG_LEVEL` | Log level (debug/info/warn/error) | `info` | No |
| `LOG_ADD_SOURCE` | Include source location in logs | `true` | No |
| `JWKS_URL` | Heimdall JWKS endpoint for JWT verification | `http://heimdall:4457/.well-known/jwks` | No |
| `AUDIENCE` | JWT audience | `lfx-v2-member-service` | No |
| `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` | Mock auth for local dev | `""` | No |
| `REPOSITORY_SOURCE` | Reader backend (`salesforce` or `mock`) | `salesforce` | No |
| `GLOBAL_ORG_ADMIN_TEAM_NAME` | Stable global org-admin team name (FGA `global_org_admin` reference on b2b_org create) | `global_org_admin` | Yes (deploy) |
| `RUN_MODE` | `server` (HTTP API) or `consumer` (Salesforce Pub/Sub CDC consumer) | `server` | No |
| `SF_PUBSUB_ENDPOINT` | Salesforce Pub/Sub gRPC endpoint (e.g. `api.pubsub.salesforce.com:7443`) | `""` | Consumer mode |
| `SF_ORG_ID` | Salesforce org ID for the Pub/Sub tenant | `""` | Consumer mode |
| `SF_CDC_CHANNEL` | CDC channel to subscribe to | `/data/ChangeEvents` | No |

### Salesforce credentials

Credentials are injected from a pre-existing Kubernetes Secret (see the Helm
chart `values.yaml` `salesforce.secrets` stanza). At least one complete
authentication flow must be configured.

| Variable | Description | Required |
| --- | --- | --- |
| `SF_INSTANCE_URL` | Salesforce instance URL (e.g. `https://linuxfoundation.my.salesforce.com`) | Yes |
| `SF_CLIENT_ID` | Connected-app consumer key | Yes |
| `SF_CLIENT_SECRET` | Consumer secret (username/password or client-credentials flow) | Conditional |
| `SF_USERNAME` | Salesforce username (username/password or JWT bearer flow) | Conditional |
| `SF_PASSWORD` | Salesforce password (username/password flow) | Conditional |
| `SF_SECURITY_TOKEN` | Security token appended to password | No |
| `SF_CONSUMER_RSA_PEM` | PEM-encoded RSA private key (JWT bearer flow) | Conditional |
| `SF_API_VERSION` | Salesforce REST API version | `v63.0` |

### Authentication flows (one must be satisfiable)

- JWT bearer: `SF_USERNAME` + `SF_CONSUMER_RSA_PEM`.
- Username/password: `SF_USERNAME` + `SF_PASSWORD` + `SF_CLIENT_SECRET`.
- Client-credentials: `SF_CLIENT_SECRET` (without `SF_USERNAME`).

## Local Development

### Option A: Full Platform Setup

For integration testing with the complete LFX stack:

- Install the lfx-platform Helm chart (includes NATS, Heimdall, OpenFGA,
  Authelia, Traefik).
- This repo exposes `make helm-install` and `make helm-templates` for its
  service chart; use platform-local values from the deployment checkout that
  owns the full stack.
- Full authentication and authorization enabled.

### Option B: Minimal Setup

For rapid development:

```bash
# Run NATS locally
docker run -d -p 4222:4222 nats:latest -js

# Optionally pre-create the buckets; the service also creates them on startup
nats kv add membership-cache --history=1 --storage=file --ttl=24h
nats kv add member-service-cache --history=1 --storage=file --ttl=168h
# org-settings is authoritative state — no TTL (local-dev only may add one to tear down)
nats kv add org-settings --history=20 --storage=file

# Run service with mock auth and Salesforce credentials
export NATS_URL=nats://localhost:4222
export JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=test-user
export SF_INSTANCE_URL=https://linuxfoundation.my.salesforce.com
export SF_CLIENT_ID=<client-id>
export SF_CLIENT_SECRET=<client-secret>
make run
```

Security note: `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` bypasses all
authentication and authorization. Use only for local development.

## Kubernetes Deployment

```bash
# Install Helm chart
helm install lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx

# Update deployment
helm upgrade lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx

# View generated manifests
helm template lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx
```

### Helm configuration

- Salesforce credentials are read from a pre-existing Kubernetes Secret. Create
  the secret out-of-band (e.g. via ESO, Sealed Secrets, or `kubectl`) before
  deploying. Configure the secret name and key mappings in `values.yaml` under
  `salesforce.secrets`.
- The `membership-cache` NATS KV bucket is created automatically via
  `nats-kv-buckets.yaml`.
- The `member-service-cache` NATS KV bucket is also created automatically for
  sObject conditional-GET cache support.
- The `org-settings` NATS KV bucket is created automatically for authoritative
  b2b_org access-control state. It must not carry a production TTL (the chart
  comments warn against it) since TTL eviction would silently revoke org
  writers and auditors.
- The `pubsub-state` NATS KV bucket is created automatically for the CDC
  consumer's replay cursors (no TTL — losing a cursor silently falls back to
  LATEST and drops events).
- When `consumer.enabled` is true, the chart renders a separate single-replica
  Deployment (`deployment-consumer.yaml`, `RUN_MODE=consumer`, Recreate
  strategy) running the Salesforce Pub/Sub CDC consumer.
- Heimdall middleware handles JWT validation.
- HTTPRoute for Gateway API routing.
- OpenFGA can be disabled for local development (allows all requests).

## Common Pitfalls

### Empty project-scoped SOQL results

Project-scoped SOQL queries (membership reads and the reindex backfill) return
zero rows.
Cause: the `ProjectResolver` failed to translate the v2 project UUID to a
Salesforce `Project__c.Id`, so the SOQL `WHERE` clause bound a UUID Salesforce
does not store. Check that the project-service NATS RPC subjects
(`lfx.projects-api.get_slug`, `lfx.projects-api.slug_to_uid`) are reachable and
that the project slug exists in Salesforce.

### Salesforce authentication failure

Service starts but all reads return errors; logs show
`salesforce authentication failed`.
Cause: missing or wrong credentials for the chosen flow. Verify
`SF_INSTANCE_URL`, `SF_CLIENT_ID`, and the credentials for the selected auth
flow are all set correctly.

### JWT validation in local dev

Every request returns 401 Unauthorized.
Set `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=local-dev-user`.

### Forgetting to generate code

Changes to design files are not reflected in implementation.
Always run `make apigen` after modifying files in `cmd/member-api/design/`.

### Mock repository still starts shared dependencies

`REPOSITORY_SOURCE=mock` swaps membership and B2B readers to in-memory mocks,
but `main.go` still initializes NATS/Salesforce for the project-id-map RPC
handler and key-contact writer wiring. Do not treat it as a fully offline
runtime mode unless that startup wiring is changed.
