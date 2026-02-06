.PHONY: test build clean migrate-up migrate-down mocks lint-python

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
