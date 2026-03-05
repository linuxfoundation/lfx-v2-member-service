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
GOA_VERSION := v3.22.6

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
test: ## Run tests
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...

.PHONY: build
build: ## Build the application for local OS
	@echo "Building application for local development..."
	go build \
		-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)" \
		-o bin/member-api ./cmd/member-api
	go build \
		-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME) -X main.GitCommit=$(GIT_COMMIT)" \
		-o bin/sync ./cmd/sync

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
