# Docker Proxy Webhook Makefile

# Variables
BINARY_NAME = docker-proxy-webhook
IMG ?= ghcr.io/nathan-mittelette/kubernetes-webhooks/docker-proxy-webhook:latest
GO_VERSION = 1.26.0
COVERAGE_FILE = coverage.out
ENVTEST_K8S_VERSION = 1.33.0
LOCALBIN ?= $(shell pwd)/bin
ENVTEST ?= $(LOCALBIN)/setup-envtest

# Default target
.PHONY: all
all: build

# Build the binary
.PHONY: build
build: fmt vet
	@echo "Building binary..."
	go build -o bin/$(BINARY_NAME) main.go

# Run tests
.PHONY: test
test: fmt vet
	@echo "Running tests..."
	go test ./... -race -coverprofile=$(COVERAGE_FILE)

# Run tests with coverage report
.PHONY: test-coverage
test-coverage: test
	@echo "Generating coverage report..."
	go tool cover -html=$(COVERAGE_FILE) -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Download setup-envtest and the envtest binaries (kube-apiserver, etcd) used
# by the controllers/ package's real-API-server integration tests. Without
# this, those tests self-skip (see controllers/secret_replication_controller_test.go).
.PHONY: envtest
envtest:
	@mkdir -p $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# Run the full test suite, including the controllers/ envtest-based
# integration tests, against a real (ephemeral) API server + etcd.
.PHONY: test-envtest
test-envtest: envtest
	@echo "Running tests (including envtest integration tests)..."
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -race -coverprofile=$(COVERAGE_FILE)

# Format code
.PHONY: fmt
fmt:
	@echo "Formatting code..."
	go fmt ./...

# Run go vet
.PHONY: vet
vet:
	@echo "Running go vet..."
	go vet ./...

# Run the webhook locally (requires kubeconfig)
.PHONY: run
run: fmt vet
	@echo "Running webhook locally..."
	go run main.go

# Build Docker image
.PHONY: docker-build
docker-build:
	@echo "Building Docker image..."
	docker build -t $(IMG) .

# Push Docker image
.PHONY: docker-push
docker-push:
	@echo "Pushing Docker image..."
	docker push $(IMG)

# Build and push Docker image
.PHONY: docker
docker: docker-build docker-push

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/
	rm -f $(COVERAGE_FILE)
	rm -f coverage.html

# Install dependencies
.PHONY: deps
deps:
	@echo "Installing dependencies..."
	go mod download
	go mod tidy

# Security check
.PHONY: security
security:
	@echo "Running security checks..."
	go list -json -m all | nancy sleuth

# Lint code (requires golangci-lint)
.PHONY: lint
lint:
	@echo "Running linter..."
	golangci-lint run

# Deploy the base manifest (requires kubectl and configured cluster).
# TLS is NOT provisioned by this target alone: follow up with either
# `make deploy-cert-manager` (default, recommended) or `make manual-certs`.
.PHONY: deploy
deploy:
	@echo "Deploying to Kubernetes..."
	kubectl apply -f manifests/k8s.yaml

# Deploy the base manifest plus cert-manager-managed TLS (option A, default).
.PHONY: deploy-cert-manager
deploy-cert-manager: deploy
	@echo "Deploying cert-manager Certificate..."
	kubectl apply -f manifests/cert-manager.yaml

# Generate and install a manual, non-cert-manager TLS certificate (option B).
# Pass extra flags via CERT_ARGS, e.g.:
#   make manual-certs CERT_ARGS="-n docker-proxy --dry-run"
.PHONY: manual-certs
manual-certs:
	@echo "Generating manual TLS certificate..."
	./hack/generate-certs.sh $(CERT_ARGS)

# Remove from Kubernetes
.PHONY: undeploy
undeploy:
	@echo "Removing from Kubernetes..."
	kubectl delete -f manifests/k8s.yaml

# Help
.PHONY: help
help:
	@echo "Available targets:"
	@echo "  all           - Build the binary (default)"
	@echo "  build         - Build the binary"
	@echo "  test          - Run tests with coverage"
	@echo "  test-coverage - Run tests and generate HTML coverage report"
	@echo "  envtest       - Download setup-envtest (kube-apiserver, etcd test binaries)"
	@echo "  test-envtest  - Run the full test suite incl. controllers/ envtest integration tests"
	@echo "  fmt           - Format code"
	@echo "  vet           - Run go vet"
	@echo "  run           - Run webhook locally"
	@echo "  docker-build  - Build Docker image"
	@echo "  docker-push   - Push Docker image"
	@echo "  docker        - Build and push Docker image"
	@echo "  clean         - Clean build artifacts"
	@echo "  deps          - Install and tidy dependencies"
	@echo "  security      - Run security checks (requires nancy)"
	@echo "  lint          - Run linter (requires golangci-lint)"
	@echo "  deploy        - Deploy the base manifest to Kubernetes (no TLS provisioning)"
	@echo "  deploy-cert-manager - Deploy base manifest + cert-manager Certificate (default TLS mode)"
	@echo "  manual-certs  - Generate and install a manual TLS certificate (no cert-manager)"
	@echo "  undeploy      - Remove from Kubernetes"
	@echo "  help          - Show this help message"
