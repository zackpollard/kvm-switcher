.PHONY: all build build-frontend build-backend \
       run dev dev-backend dev-frontend dev-stack dev-stack-down \
       test test-backend test-frontend test-e2e clean help

all: build

# ── Build ────────────────────────────────────────────────────────────

build: build-frontend build-backend ## Build frontend and backend

build-frontend: ## Build the SvelteKit frontend
	cd web && npm install && npm run build

build-backend: ## Build the Go backend binary
	go build -o kvm-switcher ./cmd/server/

# ── Run ──────────────────────────────────────────────────────────────

run: build ## Build and run the server
	./kvm-switcher -config configs/servers.yaml -web web/build

# ── Development ──────────────────────────────────────────────────────

dev: dev-stack ## Full dev: start observability stack, run backend with metrics
	KVM_METRICS_ENABLED=true go run ./cmd/server/ -config configs/servers.yaml -web web/build

dev-backend: ## Run the Go backend (no metrics stack)
	go run ./cmd/server/ -config configs/servers.yaml -web web/build

dev-frontend: ## Run the SvelteKit dev server with hot reload
	cd web && npm run dev

dev-stack: ## Start Prometheus + Grafana (docker-compose)
	docker compose -f dev/docker-compose.yml up -d
	@echo ""
	@echo "  Prometheus  → http://localhost:9090"
	@echo "  Grafana     → http://localhost:3001  (admin/admin, or anonymous viewer)"
	@echo "  Dashboard   → http://localhost:3001/d/kvm-switcher/kvm-switcher"
	@echo ""

dev-stack-down: ## Stop Prometheus + Grafana
	docker compose -f dev/docker-compose.yml down

# ── Testing ──────────────────────────────────────────────────────────

test: test-backend test-frontend ## Run all tests

test-backend: ## Run Go tests with race detector
	go test ./... -count=1 -race

test-frontend: ## Run frontend vitest tests
	cd web && npm run test

test-e2e: ## Run E2E browser tests (requires server running with BMC access)
	cd tests/e2e && npm install && npx playwright test

# ── Cleanup ──────────────────────────────────────────────────────────

clean: dev-stack-down ## Stop dev stack and remove build artifacts
	rm -f kvm-switcher
	rm -rf web/build web/.svelte-kit

# ── Help ─────────────────────────────────────────────────────────────

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'
