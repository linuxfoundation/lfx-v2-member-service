# Copyright The Linux Foundation and each contributor to LFX.
# SPDX-License-Identifier: MIT

APP_NAME := lfx-v2-member-service
VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")

# Docker
DOCKER_REGISTRY := ghcr.io/linuxfoundation
DOCKER_IMAGE := $(DOCKER_REGISTRY)/$(APP_NAME)
DOCKER_TAG := $(VERSION)

# Helm variables
HELM_CHART_PATH=./charts/lfx-v2-member-service
HELM_RELEASE_NAME=lfx-v2-member-service
HELM_NAMESPACE=lfx

# Go
GO_VERSION := 1.24.5
GOOS := linux
GOARCH := amd64
GOA_VERSION := v3.25.3
PROTOC_VERSION := 35.0
PROTOC_OS := osx-aarch_64

# Linting
GOLANGCI_LINT_VERSION := v2.2.2
LINT_TIMEOUT := 10m
LINT_TOOL=$(shell go env GOPATH)/bin/golangci-lint
GO_FILES=$(shell find . -name '*.go' -not -path './gen/*' -not -path './vendor/*')

##@ Development

.PHONY: setup-dev
setup-dev: ## Setup development tools
	@echo "Installing development tools..."
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: setup
setup: ## Setup development environment
	@echo "Setting up development environment..."
	go mod download
	go mod tidy

.PHONY: deps
deps: ## Install dependencies
	@echo "Installing dependencies..."
	go install goa.design/goa/v3/cmd/goa@$(GOA_VERSION)

.PHONY: apigen
apigen: deps #@ Generate API code using Goa
	goa gen github.com/linuxfoundation/lfx-v2-member-service/cmd/member-api/design

# PROTO_GEN: one-time regeneration of Salesforce Pub/Sub API gRPC stubs.
# Only needed when api/salesforce/pubsub/pubsub_api.proto is updated.
# The generated files (internal/infrastructure/salesforce/pubsub/proto/*.pb.go)
# are committed to git so normal builds never require protoc.
#
# Usage:
#   make protoc-install   # download protoc binary to /tmp (macOS ARM)
#   make protoc-gen       # regenerate pb.go stubs from the vendored .proto
#
# To use on macOS x86_64 override: make protoc-install PROTOC_OS=osx-x86_64
# To use on Linux x86_64 override:  make protoc-install PROTOC_OS=linux-x86_64

PROTOC_BIN := /tmp/protoc-$(PROTOC_VERSION)-bin/bin/protoc
PROTO_SRC   := api/salesforce/pubsub/pubsub_api.proto
PROTO_OUT   := internal/infrastructure/salesforce/pubsub/proto
PROTO_PKG   := github.com/linuxfoundation/lfx-v2-member-service/$(PROTO_OUT)

.PHONY: protoc-install
protoc-install: ## Download protoc $(PROTOC_VERSION) binary (no root / brew required)
	@echo "==> Downloading protoc $(PROTOC_VERSION) for $(PROTOC_OS)..."
	@curl -fsSL "https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-$(PROTOC_OS).zip" \
		-o /tmp/protoc-$(PROTOC_VERSION).zip
	@unzip -qo /tmp/protoc-$(PROTOC_VERSION).zip -d /tmp/protoc-$(PROTOC_VERSION)-bin
	@$(PROTOC_BIN) --version

.PHONY: protoc-gen
protoc-gen: ## Regenerate pb.go stubs from api/salesforce/pubsub/pubsub_api.proto
	@echo "==> Installing Go protoc plugins..."
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "==> Generating stubs → $(PROTO_OUT)/"
	@mkdir -p $(PROTO_OUT)
	$(PROTOC_BIN) \
		--proto_path=api/salesforce/pubsub \
		--go_out=$(PROTO_OUT) \
		--go_opt=paths=source_relative \
		--go_opt=M$(notdir $(PROTO_SRC))=$(PROTO_PKG) \
		--go-grpc_out=$(PROTO_OUT) \
		--go-grpc_opt=paths=source_relative \
		--go-grpc_opt=M$(notdir $(PROTO_SRC))=$(PROTO_PKG) \
		--plugin=protoc-gen-go=$(shell go env GOPATH)/bin/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(shell go env GOPATH)/bin/protoc-gen-go-grpc \
		$(PROTO_SRC)
	@echo "==> Done. Commit the updated files in $(PROTO_OUT)/"

.PHONY: lint
lint: ## Run golangci-lint (local Go linting)
	@echo "Running golangci-lint..."
	@which golangci-lint >/dev/null 2>&1 || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION))
	@golangci-lint run ./... && echo "==> Lint OK"

# Format code
.PHONY: fmt
fmt:
	@echo "==> Formatting code..."
	@go fmt ./...
	@gofmt -s -w $(GO_FILES)

.PHONY: test
test: test-scripts ## Run tests
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...

.PHONY: test-scripts
test-scripts: ## Run shell migration tests
	@bash scripts/test-global-org-admin-team-migration.sh

.PHONY: build
build: ## Build the application for local OS
	@echo "Building application for local development..."
	go build \
		-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)" \
		-o bin/member-api ./cmd/member-api

.PHONY: run
run: build ## Run the application for local development
	@echo "Running application for local development..."
	./bin/member-api

##@ Docker

.PHONY: docker-build
docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .
	docker tag $(DOCKER_IMAGE):$(DOCKER_TAG) $(DOCKER_IMAGE):latest

.PHONY: docker-run
docker-run: ## Run Docker container locally
	@echo "Running Docker container..."
	docker run \
		--name $(APP_NAME) \
		-p 8080:8080 \
		-e NATS_URL=nats://lfx-platform-nats.lfx.svc.cluster.local:4222 \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

##@ Helm/Kubernetes
.PHONY: helm-install
helm-install:
	@echo "==> Installing Helm chart..."
	helm upgrade --install $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE)
	@echo "==> Helm chart installed: $(HELM_RELEASE_NAME)"

.PHONY: helm-templates
helm-templates:
	@echo "==> Printing templates for Helm chart..."
	helm template $(HELM_RELEASE_NAME) $(HELM_CHART_PATH) --namespace $(HELM_NAMESPACE)
	@echo "==> Templates printed for Helm chart: $(HELM_RELEASE_NAME)"

.PHONY: helm-uninstall
helm-uninstall:
	@echo "==> Uninstalling Helm chart..."
	helm uninstall $(HELM_RELEASE_NAME) --namespace $(HELM_NAMESPACE)
	@echo "==> Helm chart uninstalled: $(HELM_RELEASE_NAME)"
