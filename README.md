# LFX V2 Member Service

This repository contains the source code for the LFX v2 platform member service.

## Overview

The LFX v2 Member Service is a read-only RESTful API service that provides membership data within the Linux Foundation's LFX platform. It exposes endpoints for querying memberships and their associated key contacts. Data is sourced from Salesforce via PostgreSQL and served through NATS key-value storage for fast retrieval.

## File Structure

```bash
├── .github/                        # Github files
│   └── workflows/                  # Github Action workflow files
├── charts/                         # Helm charts for running the service in kubernetes
├── cmd/                            # Services (main packages)
│   ├── member-api/                 # Member service API code
│   │   ├── design/                 # API design specifications (Goa)
│   │   ├── service/                # Service implementation
│   │   ├── main.go                 # Application entry point
│   │   └── http.go                 # HTTP server setup
│   └── sync/                       # Sync job (PostgreSQL -> NATS KV)
│       └── main.go                 # Sync entry point
├── gen/                            # Generated code from Goa design
├── internal/                       # Internal service packages
│   ├── domain/                     # Domain logic layer (business logic)
│   │   ├── model/                  # Domain models and entities
│   │   └── port/                   # Repository and service interfaces
│   ├── service/                    # Service logic layer (use cases)
│   ├── infrastructure/             # Infrastructure layer
│   │   ├── nats/                   # NATS storage implementation
│   │   ├── postgres/               # PostgreSQL repositories (read-only)
│   │   └── mock/                   # Mock implementations for testing
│   └── middleware/                 # HTTP middleware components
└── pkg/                            # Shared packages
```

## Key Features

- **RESTful API**: Read-only endpoints for membership and key contact queries
- **Sync Job**: Reads membership data from PostgreSQL (Salesforce schema) and writes to NATS KV
- **NATS Storage**: Uses NATS key-value buckets for fast membership data retrieval
- **Indexed Lookups**: Project-based lookup indexes for efficient filtered queries
- **Clean Architecture**: Follows hexagonal architecture with clear separation of domain, service, and infrastructure layers
- **Authorization**: JWT-based authentication with Heimdall middleware integration
- **Health Checks**: Built-in `/livez` and `/readyz` endpoints
- **Request Tracking**: Automatic request ID generation and propagation
- **Structured Logging**: JSON-formatted logs with contextual information
- **OpenTelemetry**: Integrated tracing, metrics, and log collection

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/memberships` | List memberships with pagination and filtering |
| GET | `/memberships/{uid}` | Get membership by UID |
| GET | `/memberships/{uid}/contacts` | List key contacts for a membership |
| GET | `/readyz` | Readiness check |
| GET | `/livez` | Liveness check |

### Filtering

Use the `filter` query parameter with semicolon-separated key=value pairs:

```
GET /memberships?filter=project_id=<id>
GET /memberships?filter=status=Active;tier=Gold
```

Supported filter keys: `project_id` (indexed), `status`, `membership_type`, `account_id`, `product_id`, `contact_id`, `year`, `tier`, `auto_renew`.

## Development

To contribute to this repository:

1. Fork the repository
2. Commit your changes to a feature branch in your fork. Ensure your commits
   are signed with the [Developer Certificate of Origin
   (DCO)](https://developercertificate.org/).
   You can use the `git commit -s` command to sign your commits.
3. Submit your pull request

### Building

```bash
make apigen    # Generate Goa API code
make build     # Build both member-api and sync binaries
make test      # Run tests
```

### Running Locally

```bash
# With NATS
NATS_URL=nats://localhost:4222 ./bin/member-api

# With mock data
REPOSITORY_SOURCE=mock AUTH_SOURCE=mock ./bin/member-api

# Run sync job
RDSDB=<postgres-connection-string> NATS_URL=nats://localhost:4222 ./bin/sync
```

## License

Copyright The Linux Foundation and each contributor to LFX.

This project's source code is licensed under the MIT License. A copy of the
license is available in `LICENSE`.

This project's documentation is licensed under the Creative Commons Attribution
4.0 International License (CC-BY-4.0). A copy of the license is available in
`LICENSE-docs`.
