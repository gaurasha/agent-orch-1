# Agent orchestration platform.
#
# `make demo` is the one command a reviewer needs.

SHELL      := /bin/bash
GO         ?= go
BACKEND    := backend
UI         := ui
SCRATCH    := .make
BIN        := $(SCRATCH)/agentorch
PG_DSN     ?= postgres://agentorch:agentorch@127.0.0.1:5432/agentorch?sslmode=disable
TEST_DSN   ?= postgres://agentorch:agentorch@127.0.0.1:5432/agentorch_test?sslmode=disable
WORKDIR    ?= /var/lib/agentorch

.DEFAULT_GOAL := help
.PHONY: help demo demo-docker build build-ui test test-safety test-durability test-load \
        test-all lint fmt run kind-up kind-test kind-down argocd-up argocd-test clean k8s-validate

help: ## Show this help
	@echo "Agent orchestration platform"
	@echo
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
	@echo
	@echo "Start with:  make demo"

$(SCRATCH):
	@mkdir -p $(SCRATCH)

build: $(SCRATCH) ## Build the platform binary
	@cd $(BACKEND) && $(GO) build -o ../$(BIN) ./cmd/agentorch
	@echo "built $(BIN)"

build-ui: ## Build the operator console
	@cd $(UI) && npm install --no-audit --no-fund --silent && npm run build
	@echo "built $(UI)/dist"

demo: build ## Run the end-to-end demonstration (Linux, needs root for namespaces)
	@./scripts/preflight.sh demo || true
	@mkdir -p $(WORKDIR)/ws $(WORKDIR)/sbx
	@$(BIN) demo --dsn "$(PG_DSN)" \
	   --workspace-dir $(WORKDIR)/ws --sandbox-dir $(WORKDIR)/sbx --log-level error

demo-docker: ## Run the demo with Docker Compose (macOS / Windows / no root)
	@./scripts/preflight.sh docker
	@$(MAKE) build-ui
	@cd deploy/compose && docker compose up --build

run: build build-ui ## Run the platform with the console at http://localhost:8080
	@mkdir -p $(WORKDIR)/ws $(WORKDIR)/sbx
	@$(BIN) serve --dsn "$(PG_DSN)" \
	   --workspace-dir $(WORKDIR)/ws --sandbox-dir $(WORKDIR)/sbx \
	   --ui-dir $(UI)/dist

test: ## Unit tests (no infrastructure required)
	@cd $(BACKEND) && $(GO) test ./internal/... -count=1

test-safety: ## The sandbox isolation tests (Linux, needs root)
	@cd $(BACKEND) && $(GO) test ./internal/sandbox/ -v -count=1

test-durability: ## Kill-a-worker-mid-task recovery tests
	@cd $(BACKEND) && AGENTORCH_TEST_DSN="$(TEST_DSN)" $(GO) test ./test/durability/ -v -count=1

test-load: ## 500 concurrent agents
	@cd $(BACKEND) && AGENTORCH_TEST_DSN="$(TEST_DSN)" $(GO) test ./test/load/ -v -count=1 -timeout 20m

test-all: ## Everything, including the Postgres conformance suite
	@cd $(BACKEND) && AGENTORCH_TEST_DSN="$(TEST_DSN)" $(GO) test ./... -count=1 -timeout 25m

lint: ## go vet + UI typecheck
	@cd $(BACKEND) && $(GO) vet ./...
	@cd $(UI) && npm run typecheck --silent

fmt: ## gofmt
	@cd $(BACKEND) && $(GO) fmt ./...

k8s-validate: ## Build and schema-check the Kubernetes manifests
	@kubectl kustomize deploy/k8s/base           > $(SCRATCH)/base.yaml
	@kubectl kustomize deploy/k8s/overlays/local > $(SCRATCH)/local.yaml
	@echo "base:";  kubeconform -strict -summary -kubernetes-version 1.31.0 $(SCRATCH)/base.yaml
	@echo "local:"; kubeconform -strict -summary -kubernetes-version 1.31.0 $(SCRATCH)/local.yaml

kind-up: ## Create a kind cluster and deploy the platform
	@./scripts/kind-up.sh

kind-test: ## Verify the deployed cluster enforces what it claims
	@./scripts/kind-test.sh

kind-down: ## Delete the kind cluster
	@kind delete cluster --name agentorch

argocd-up: ## Install Argo CD and register this repository
	@./scripts/argocd-up.sh

argocd-test: ## Verify Argo CD reconciles (including selfHeal)
	@./scripts/argocd-test.sh

clean: ## Remove build artefacts
	@rm -rf $(SCRATCH) $(UI)/dist $(UI)/node_modules
	@echo "cleaned"
