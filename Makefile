# personal-ai — run `make help` for the list of targets.

SHELL := /bin/bash
.DEFAULT_GOAL := help

# ---- settings (override on the command line, e.g. `make docker-build TAG=0.2.0`) ----
VERSION           ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE             ?= personal-ai
TAG               ?= dev
OVERLAY           ?= local
K8S_DIR           := deployments/kubernetes
K8S_NAMESPACE     ?= personal-ai
CONFIG            ?= config/config.example.yaml

TEST_DB_CONTAINER ?= personal-ai-test-pg
TEST_DB_PORT      ?= 55432
TEST_DB_URL       ?= postgres://postgres:test@localhost:$(TEST_DB_PORT)/pai?sslmode=disable

.PHONY: help build test test-integration cover vet fmt check leaks tidy clean \
        db-up db-down run-server run-telegram \
        docker-build docker-push \
        k8s-secrets k8s-render k8s-apply k8s-delete k8s-status k8s-logs k8s-port-forward \
        minikube-load

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)}' $(MAKEFILE_LIST)

##@ Development

build: ## Build server, telegram and CLI into bin/
	go build -trimpath -o bin/server ./cmd/server
	go build -trimpath -o bin/telegram ./cmd/telegram
	go build -trimpath -o bin/personal-ai ./cmd/cli

test: ## Unit tests with the race detector (PostgreSQL tests are skipped)
	go test -race ./...

test-integration: db-up ## All tests, including PostgreSQL and a full server boot
	PERSONAL_AI_TEST_DATABASE_URL='$(TEST_DB_URL)' go test -race -count=1 ./...

cover: ## Unit test coverage summary
	go test -coverprofile=coverage.out ./... >/dev/null && go tool cover -func=coverage.out | tail -1

vet: ## go vet
	go vet ./...

fmt: ## Format all Go code
	gofmt -l -w .

check: ## Formatting, vet, tests and secret scan (what CI runs)
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...
	@$(MAKE) --no-print-directory leaks

leaks: ## Scan git history and working tree for committed secrets (gitleaks)
	@command -v gitleaks >/dev/null || { echo "gitleaks is not installed (brew install gitleaks)"; exit 1; }
	gitleaks git --redact --no-banner .
	gitleaks dir --redact --no-banner .

tidy: ## go mod tidy
	go mod tidy

clean: ## Remove build outputs
	rm -rf bin/ coverage.out

db-up: ## Start a disposable PostgreSQL in Docker (port 55432 by default)
	@docker inspect $(TEST_DB_CONTAINER) >/dev/null 2>&1 || \
		docker run -d --name $(TEST_DB_CONTAINER) -e POSTGRES_PASSWORD=test -e POSTGRES_DB=pai \
			-p $(TEST_DB_PORT):5432 postgres:16-alpine >/dev/null
	@docker start $(TEST_DB_CONTAINER) >/dev/null
	@until docker exec $(TEST_DB_CONTAINER) pg_isready -U postgres -d pai >/dev/null 2>&1; do sleep 1; done
	@echo "PostgreSQL ready: $(TEST_DB_URL)"

db-down: ## Remove the disposable PostgreSQL
	-docker rm -f $(TEST_DB_CONTAINER)

run-server: build db-up ## Run the server locally against the Docker PostgreSQL (secrets from your env)
	PERSONAL_AI_DATABASE_URL="$${PERSONAL_AI_DATABASE_URL:-$(TEST_DB_URL)}" \
	PERSONAL_AI_LOG_FORMAT=text ./bin/server -config $(CONFIG)

run-telegram: build ## Run the Telegram bot locally (secrets from your env)
	PERSONAL_AI_LOG_FORMAT=text PERSONAL_AI_CONFIG=$(CONFIG) ./bin/telegram

##@ Container image

docker-build: ## Build the image $(IMAGE):$(TAG)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(TAG) .

docker-push: ## Push $(IMAGE):$(TAG) (set IMAGE to your registry path)
	docker push $(IMAGE):$(TAG)

##@ Kubernetes (OVERLAY=local|production)

k8s-secrets: ## Create secrets.env for the overlay from its template (never overwrites)
	@f=$(K8S_DIR)/overlays/$(OVERLAY)/secrets.env; \
	if [ -f $$f ]; then echo "$$f already exists"; exit 0; fi; \
	sed -e "s|^PERSONAL_AI_MANAGEMENT_TOKEN=.*|PERSONAL_AI_MANAGEMENT_TOKEN=$$(openssl rand -hex 32)|" \
	    -e "s|^PERSONAL_AI_INFERENCE_TOKEN=.*|PERSONAL_AI_INFERENCE_TOKEN=$$(openssl rand -hex 32)|" \
	    -e "s|^PERSONAL_AI_RUNTIME_KEY_SECRET=.*|PERSONAL_AI_RUNTIME_KEY_SECRET=$$(openssl rand -hex 32)|" \
	    $$f.example > $$f; \
	if [ "$(OVERLAY)" = local ]; then pw=$$(openssl rand -hex 16); \
	  sed -i.bak -e "s|change-me|$$pw|g" $$f && rm -f $$f.bak; fi; \
	chmod 600 $$f; \
	echo "created $$f with generated tokens; now set PERSONAL_AI_RUNPOD_API_KEY (and optional values)"

k8s-render: ## Print the manifests for the overlay
	@test -f $(K8S_DIR)/overlays/$(OVERLAY)/secrets.env || (echo "missing secrets.env: run 'make k8s-secrets OVERLAY=$(OVERLAY)'"; exit 1)
	kubectl kustomize $(K8S_DIR)/overlays/$(OVERLAY)

k8s-apply: ## Apply the overlay to the current kube context
	@test -f $(K8S_DIR)/overlays/$(OVERLAY)/secrets.env || (echo "missing secrets.env: run 'make k8s-secrets OVERLAY=$(OVERLAY)'"; exit 1)
	@echo "context: $$(kubectl config current-context)"
	kubectl apply -k $(K8S_DIR)/overlays/$(OVERLAY)
	kubectl -n $(K8S_NAMESPACE) rollout status deployment/personal-ai-server --timeout=5m

k8s-delete: ## Delete everything in the overlay, including the namespace and the in-cluster database
	kubectl delete -k $(K8S_DIR)/overlays/$(OVERLAY) --ignore-not-found

k8s-status: ## Show pods, services and deployments
	kubectl -n $(K8S_NAMESPACE) get pods,svc,deploy,sts

k8s-logs: ## Follow server logs
	kubectl -n $(K8S_NAMESPACE) logs -f deployment/personal-ai-server

k8s-port-forward: ## Expose the API on localhost:8080
	kubectl -n $(K8S_NAMESPACE) port-forward svc/personal-ai-server 8080:8080

minikube-load: docker-build ## Build the image and load it into minikube
	minikube image load $(IMAGE):$(TAG)
