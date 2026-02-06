.PHONY: test build clean migrate-up migrate-down mocks lint-python run-job run-stack stop-stack

# Database connection string used by the migrate container (connects via Docker network)
MIGRATE_DB_URL := postgres://postgres:localdev@postgres:5432/watchpoint?sslmode=disable

# Common docker run command for golang-migrate
MIGRATE_CMD := docker run --rm --network watchpoint_net \
	-v $(PWD)/migrations:/migrations \
	migrate/migrate \
	-path=/migrations \
	-database '$(MIGRATE_DB_URL)'

# Run all Go tests with race detection enabled
test:
	go test -race ./...

# Verify all cmd/ entry points compile
build:
	go build ./cmd/...

# Remove build artifacts
clean:
	go clean ./...
	rm -f coverage.out

# Apply all pending database migrations
migrate-up:
	$(MIGRATE_CMD) up

# Roll back all database migrations
migrate-down:
	$(MIGRATE_CMD) down -all

# Generate mocks for all interfaces in internal/external using mockery.
# Uses --name with a regex to target only true interfaces (excludes function types
# like RegistryOption and BaseClientOption which cannot be properly mocked).
mocks:
	@rm -rf internal/external/mocks
	go run github.com/vektra/mockery/v2@latest \
		--name 'BillingService|WebhookVerifier|EmailProvider|EmailVerifier|OAuthProvider|OAuthManager|OrgBillingLookup' \
		--case snake \
		--outpkg mocks \
		--output internal/external/mocks \
		--dir internal/external

# Lint Python code (eval worker and runpod) using black and mypy
lint-python:
	black --check worker/ runpod/
	mypy worker/ runpod/ --ignore-missing-imports

# Run a maintenance job locally, bypassing Lambda.
# Usage: make run-job TASK=aggregate_usage
#        make run-job TASK=cleanup_soft_deletes REF_TIME=2026-01-15T02:00:00Z
#        make run-job TASK=trigger_digests DRY_RUN=true
# Available tasks: make run-job LIST=true
run-job:
ifdef LIST
	go run ./cmd/tools/job-runner --list
else ifdef DRY_RUN
	go run ./cmd/tools/job-runner --task=$(TASK) --dry-run $(if $(REF_TIME),--reference-time=$(REF_TIME))
else
	go run ./cmd/tools/job-runner --task=$(TASK) $(if $(REF_TIME),--reference-time=$(REF_TIME))
endif

# Start the full local stack (API, eval worker, notification workers).
# Requires Docker Compose services to be running: docker compose up -d
# See scripts/start-local-stack.sh for details.
run-stack:
	./scripts/start-local-stack.sh

# Stop the local stack by sending SIGTERM to all service PIDs.
# Useful if the orchestrator was backgrounded or running in another terminal.
stop-stack:
	@if [ -f test_artifacts/.pids ]; then \
		while IFS='=' read -r name pid; do \
			if kill -0 "$$pid" 2>/dev/null; then \
				echo "Stopping $$name (PID $$pid)..."; \
				kill "$$pid" 2>/dev/null || true; \
			fi; \
		done < test_artifacts/.pids; \
		rm -f test_artifacts/.pids; \
		echo "Local stack stopped."; \
	else \
		echo "No PID file found at test_artifacts/.pids (stack may not be running)."; \
	fi
