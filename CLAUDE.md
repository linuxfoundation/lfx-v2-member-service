# Claude Development Guide for LFX V2 Member Service

This guide provides essential information for Claude instances working with the LFX V2 Member Service codebase. It includes build commands, architecture patterns, and key technical decisions.

## Project Overview

The LFX V2 Member Service is a read-only RESTful API service that provides membership data for the Linux Foundation's LFX platform. It exposes endpoints for listing and retrieving membership information synced from upstream sources.

### Key Technologies

- **Language**: Go 1.24+
- **API Framework**: Goa v3 (code generation framework)
- **Messaging**: NATS with JetStream for event-driven architecture
- **Storage**: NATS Key-Value stores (populated by sync job from PostgreSQL)
- **Authentication**: JWT with Heimdall middleware
- **Authorization**: OpenFGA for fine-grained access control
- **Container**: Chainguard distroless images
- **Orchestration**: Kubernetes with Helm charts

## Architecture Overview

The service follows **Clean Architecture** principles with clear separation of concerns:

```text
cmd/member-api/               # Presentation Layer (HTTP entry point)
├── design/                  # Goa API design specifications
│   ├── membership.go        # API endpoints definition (member-centric routes)
│   └── type.go              # Goa type definitions (Member, Membership, KeyContact)
├── service/                 # Service handlers (implements Goa interfaces)
│   ├── membership_service.go  # Main service handler with endpoint logic
│   ├── membership_service_response.go  # Response conversion helpers
│   ├── providers.go         # Dependency initialization (NATS, auth)
│   └── error.go             # Error mapping helpers
├── http.go                  # HTTP server setup and middleware
└── main.go                  # Application entry point

cmd/sync/                    # Sync job entry point (postgres → NATS)
└── main.go

gen/                         # Generated code (DO NOT EDIT MANUALLY)
├── membership_service/      # Generated service interfaces and endpoints
└── http/membership_service/ # Generated HTTP server/client code

internal/
├── domain/                  # Domain layer
│   ├── auth.go              # Authenticator interface
│   ├── model/               # Domain entities
│   │   ├── member.go        # Member, MembershipSummary models
│   │   ├── membership.go    # Membership, KeyContact, Account, Contact, Product, Project
│   │   └── list_params.go   # ListParams with Search support
│   └── port/                # Repository interfaces
│       ├── member_reader.go  # MemberReader interface (main read port)
│       └── membership_syncer.go  # Sync interfaces (source reader, KV writer)
├── infrastructure/          # Infrastructure layer
│   ├── auth/                # JWT authentication (Heimdall)
│   ├── mock/                # Mock repository for testing
│   ├── nats/                # NATS KV repository implementation
│   └── postgres/            # PostgreSQL repository (used by sync job)
│       ├── member_repo.go   # Fetches distinct accounts with memberships
│       └── membership_repo.go  # Fetches membership assets
├── middleware/              # HTTP middleware
│   ├── authorization.go     # Extracts Authorization header to context
│   └── request_id.go        # Request ID propagation
└── service/                 # Business logic / use case orchestration
    └── member_reader.go     # MemberReaderOrchestrator

pkg/
└── constants/               # Shared constants (HTTP headers, NATS buckets, etc.)

charts/                      # Helm chart for Kubernetes deployment
└── lfx-v2-member-service/
```

### Key Design Principles

1. **Read-Only API**: This service only exposes GET endpoints — all data mutations go through the sync job
2. **Database Independence**: Repository interfaces allow switching storage backends
3. **Testability**: Each layer can be tested in isolation using mocks
4. **Separation of Concerns**: Clear boundaries between layers

## API Endpoints

The API is structured around **Members** (accounts/organizations) as the top-level resource. Each member owns one or more memberships.

| Method | Path | Description | OpenFGA Check |
|--------|------|-------------|---------------|
| GET | `/members` | Search/list members with pagination, filtering, and search | `auditor` on `member` (allow_all) |
| GET | `/members/{member_id}/memberships/{id}` | Get a specific membership under a member | `auditor` on `member:{member_id}` |
| GET | `/members/{member_id}/memberships/{id}/key_contacts` | List key contacts for a membership | `auditor` on `member:{member_id}` |
| GET | `/readyz` | Readiness probe | None |
| GET | `/livez` | Liveness probe | None |
| GET | `/_memberships/openapi*.{json,yaml}` | OpenAPI spec files | None |

### Member Search & Filtering

The `/members` endpoint supports:

- **`search`** query parameter: Free-text case-insensitive substring match across member name, project names, and tier names
- **`filter`** query parameter: Key-value pairs separated by `;` (e.g., `filter=status=Active;tier=Gold`)

| Filter Key | Match Type | Example |
|------------|------------|---------|
| `name` | Case-insensitive contains | `name=Linux` |
| `member_id` | Exact (UID or account SFID) | `member_id=abc-123` |
| `project_id` | Exact (supports dual IDs) | `project_id=proj-123` |
| `project_name` | Case-insensitive contains | `project_name=Kubernetes` |
| `project_slug` | Case-insensitive exact | `project_slug=linux-foundation` |
| `tier` | Case-insensitive exact | `tier=Gold` |
| `status` | Case-insensitive exact | `status=Active` |
| `year` | Exact | `year=2026` |
| `product_name` | Case-insensitive contains | `product_name=Gold` |
| `membership_type` | Case-insensitive exact | `membership_type=Corporate` |

## Development Workflow

### Prerequisites

```bash
# Install Go 1.24+
# Install Goa framework
go install goa.design/goa/v3/cmd/goa@latest

# Install linting tools
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

### Common Development Tasks

#### 1. Generate API Code (REQUIRED after design changes)

```bash
make apigen
# or directly:
goa gen github.com/linuxfoundation/lfx-v2-member-service/cmd/member-api/design -o .
```

#### 2. Build the Service

```bash
make build
```

#### 3. Run Tests

```bash
make test              # Run unit tests
make test-verbose      # Verbose output
make test-coverage     # Generate coverage report
```

#### 4. Run the Service Locally

```bash
# Basic run
make run

# With debug logging
make debug

# With mock auth (bypasses Heimdall JWT validation)
export NATS_URL=nats://localhost:4222
export JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=test-user
make run
```

#### 5. Lint and Format Code

```bash
make fmt    # Format code
make lint   # Run golangci-lint
make check  # Check format and lint without modifying
```

## Code Generation (Goa Framework)

The service uses Goa v3 for API code generation. This is **critical** to understand:

1. **Design First**: API is defined in `cmd/member-api/design/` files
2. **Generated Code**: Running `make apigen` generates to `gen/`:
   - HTTP server/client code
   - Service interfaces
   - OpenAPI specifications
   - Type definitions
3. **Implementation**: You implement the generated interfaces in `cmd/member-api/service/membership_service.go`

### Adding New Endpoints

1. Update `cmd/member-api/design/membership.go` with new method
2. Run `make apigen` to regenerate code
3. Implement the new method in `cmd/member-api/service/membership_service.go`
4. Add tests for the new endpoint
5. Update Heimdall ruleset in `charts/lfx-v2-member-service/templates/ruleset.yaml`

## NATS Storage

The service reads from three NATS Key-Value stores (populated by the sync job):

- `members`: Member (account) records with precomputed MembershipSummary
- `memberships`: Membership records (with MemberUID linking back to member)
- `membership-contacts`: Key contacts for memberships

### Lookup Key Patterns

- Member by UID: `{uid}` → direct KV key in `members` bucket
- Member by SFID: `lookup/member-sfid/{sfid}` → member UID (supports both sfid and sfid_b2b)
- Member by project: `lookup/member-project/{project_id}/{member_uid}` → member UID
- Membership by member: `lookup/member-membership/{member_uid}/{membership_uid}` → membership UID
- Membership by project: `lookup/project/{project_id}/{membership_uid}` → membership UID
- Contact by membership: `lookup/membership/{membership_uid}/{contact_uid}` → contact UID

### Sync-time Denormalization

During sync, for each member:
1. All memberships are grouped by `MemberUID`
2. A `MembershipSummary` is precomputed (active count, total count, membership details)
3. Lookup keys are stored for both `sfid` and `sfid_b2b` (dual Salesforce ID support)
4. Member UIDs are deterministic: `uuid.NewSHA1(namespace, "lfx-member:{account_sfid}")`

## Authentication (JWT / Heimdall)

JWT authentication is implemented via `internal/infrastructure/auth/`:

- **`JWTAuth`**: Real implementation that validates tokens via Heimdall JWKS
- **`MockJWTAuth`**: Test mock that implements the `domain.Authenticator` interface

### Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `JWKS_URL` | Heimdall JWKS endpoint | `http://heimdall:4457/.well-known/jwks` |
| `AUDIENCE` | JWT audience | `lfx-v2-member-service` |
| `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` | Mock principal for local dev (bypasses JWT) | `""` (disabled) |

When `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` is set, the service skips JWT validation entirely and uses that value as the authenticated principal. **Only use for local development.**

### How Authentication Works

1. Heimdall intercepts requests and validates the OIDC token
2. Heimdall creates a signed JWT with `principal` claim and forwards to this service
3. This service validates the Heimdall JWT in `JWTAuth()` (the Goa security handler)
4. The principal is stored in context as `constants.PrincipalContextID`

## Authorization (OpenFGA)

The service uses the `member` type in the OpenFGA model (defined in lfx-v2-helm):

```dsl
type member
  relations
    define auditor: [user, team#member]
```

Authorization checks in Heimdall ruleset:
- **GET /members** — authenticated, allow_all (no object-level check)
- **GET /members/{member_id}/memberships/{id}** — requires `auditor` on `member:{member_id}`
- **GET /members/{member_id}/memberships/{id}/key_contacts** — requires `auditor` on `member:{member_id}`

## Testing Patterns

### Unit Tests

- Mock all external dependencies using `mock` package in `internal/infrastructure/mock/`
- Use `auth.MockJWTAuth` for authentication mocking
- Table-driven tests for comprehensive coverage
- Each function has exactly ONE corresponding test function with multiple cases
- Unit tests alongside implementation with `*_test.go` suffix

### Example Test Structure

```go
func TestEndpoint(t *testing.T) {
    tests := []struct {
        name       string
        payload    *membershipservice.Payload
        setupMocks func(*auth.MockJWTAuth)
        wantErr    bool
    }{
        // Test cases
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test logic
        })
    }
}
```

## Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
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
| `REPOSITORY_SOURCE` | Storage backend (`nats` or `mock`) | `nats` | No |

## Local Development Setup

### Option A: Full Platform Setup

For integration testing with complete LFX stack:

- Install lfx-platform Helm chart (includes NATS, Heimdall, OpenFGA, Authelia, Traefik)
- Use `make helm-install-local` with values.local.yaml
- Full authentication and authorization enabled

### Option B: Minimal Setup

For rapid development:

```bash
# Run NATS locally
docker run -d -p 4222:4222 nats:latest -js

# Create KV stores
nats kv add members --history=20 --storage=file
nats kv add memberships --history=20 --storage=file
nats kv add membership-contacts --history=20 --storage=file

# Run service with mock auth
export NATS_URL=nats://localhost:4222
export JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=test-user
make run
```

**Security Note**: Option B bypasses all authentication/authorization — only for local development.

## Docker Build

```bash
# Build from repository root
docker build -t lfx-v2-member-service:latest .

# The Dockerfile uses:
# - Chainguard Go image for building
# - Chainguard static image for runtime (distroless)
# - Multi-stage build for minimal image size
```

## Kubernetes Deployment

```bash
# Install Helm chart
helm install lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx

# Update deployment
helm upgrade lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx

# View generated manifests
helm template lfx-v2-member-service ./charts/lfx-v2-member-service/ -n lfx
```

### Helm Configuration

- OpenFGA can be disabled for local development (allows all requests)
- NATS KV buckets are created automatically via `nats-kv-buckets.yaml`
- Heimdall middleware handles JWT validation
- HTTPRoute for Gateway API routing

## CI/CD Pipeline

GitHub Actions workflows:

- **mega-linter.yml**: Comprehensive linting (Go, YAML, Docker, etc.)
- **member-api-build.yml**: Build and test on PRs
- **license-header-check.yml**: Ensure proper licensing

## Common Pitfalls and Solutions

### 1. Forgetting to Generate Code

**Problem**: Changes to design files not reflected in implementation
**Solution**: Always run `make apigen` after modifying design files

### 2. NATS Connection

**Problem**: Service fails to start due to NATS connection
**Solution**: Ensure NATS is running and NATS_URL is correct

### 3. Empty KV Stores

**Problem**: Service returns empty results
**Solution**: Run the sync job to populate NATS from PostgreSQL, or load mock data manually

### 4. JWT Validation in Local Dev

**Problem**: Every request returns 401 Unauthorized
**Solution**: Set `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL=local-dev-user`

## Key Implementation Details

### Service Architecture

The `membershipServicesrvc` struct in `membership_service.go` is the central service handler. It holds:
- `memberReaderOrchestrator`: Use case layer for member/membership business logic (`MemberReaderOrchestrator`)
- `storage`: Direct storage access (for readyz check, implements `port.MemberReader`)
- `auth`: `domain.Authenticator` for JWT validation

### JWTAuth Security Handler

The `JWTAuth` method is called automatically by Goa for all endpoints with `dsl.Security(JWTAuth)`. It:
1. Calls `auth.ParsePrincipal()` to validate and extract the principal
2. Stores the principal in context under `constants.PrincipalContextID`
3. Returns an error if authentication fails (results in HTTP 401)

### Error Handling

Domain errors are mapped to HTTP status codes in `cmd/member-api/service/error.go`:

- `ErrNotFound` → 404
- `ErrInternal` → 500
- `ErrServiceUnavailable` → 503

## Resources

- [Goa Framework Docs](https://goa.design/docs/)
- [NATS JetStream Docs](https://docs.nats.io/jetstream)
- [OpenFGA Docs](https://openfga.dev/docs)
- [Clean Architecture](https://blog.cleancoder.com/uncle-bob/2012/08/13/the-clean-architecture.html)
