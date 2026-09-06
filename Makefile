.PHONY: all build build-collector build-api build-migrate run-collector run-api test test-coverage test-race test-integration lint lint-fix vet fmt staticcheck govulncheck mod-verify ci clean db-up db-down db-migrate db-rollback docker-build web-install web-dev web-lint web-typecheck web-test web-test-coverage web-build web-ci

GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
MODULE=github.com/tirante-dev/gascurve
COLLECTOR_BINARY=gascurve-collector
API_BINARY=gascurve-api
MIGRATE_BINARY=gascurve-migrate
COVERAGE_THRESHOLD ?= 90
COVERAGE_PACKAGES ?= ./internal/...
COVERAGE_DIR=coverage
COVERAGE_FILE=$(COVERAGE_DIR)/coverage.out
COVERAGE_HTML=$(COVERAGE_DIR)/coverage.html
NPM=npm --prefix web
DB_URL ?= postgres://postgres:postgres@127.0.0.1:5432/gascurve?sslmode=disable

all: test build

# ---------------------------------------------------------------- Go

build: build-collector build-api build-migrate

build-collector:
	$(GOBUILD) -o $(COLLECTOR_BINARY) ./cmd/collector

build-api:
	$(GOBUILD) -o $(API_BINARY) ./cmd/api

build-migrate:
	$(GOBUILD) -o $(MIGRATE_BINARY) ./cmd/migrate

run-collector: build-collector
	./$(COLLECTOR_BINARY)

run-api: build-api
	./$(API_BINARY)

test:
	$(GOTEST) ./...

test-coverage:
	@mkdir -p $(COVERAGE_DIR)
	$(GOTEST) -coverprofile=$(COVERAGE_FILE) -covermode=atomic $(COVERAGE_PACKAGES)
	$(GOCMD) tool cover -html=$(COVERAGE_FILE) -o $(COVERAGE_HTML)
	@COVERAGE=$$($(GOCMD) tool cover -func=$(COVERAGE_FILE) | grep total | awk '{print $$NF}' | tr -d '%'); \
	echo "Total coverage: $${COVERAGE}%"; \
	if awk -v cov="$${COVERAGE}" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN {exit !(cov < threshold)}'; then \
		echo "FAIL: Coverage $${COVERAGE}% is below $(COVERAGE_THRESHOLD)% threshold"; \
		exit 1; \
	fi; \
	echo "OK: Coverage $${COVERAGE}% meets $(COVERAGE_THRESHOLD)% threshold"

test-race:
	$(GOTEST) -race ./...

# Integration tests need a throwaway Postgres in TEST_DB_URL. Gated by the
# `integration` build tag so they never run by accident.
test-integration:
	$(GOTEST) -tags integration -count=1 -v ./internal/db/... ./internal/collector/... ./internal/api/...

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

vet:
	$(GOCMD) vet ./...

fmt:
	gofmt -s -w .
	goimports -w -local $(MODULE) .

staticcheck:
	staticcheck ./...

govulncheck:
	govulncheck ./...

mod-verify:
	$(GOCMD) mod verify
	$(GOCMD) mod tidy
	@if [ -n "$$(git diff -- go.mod go.sum)" ]; then echo "FAIL: go.mod/go.sum not tidy"; git diff -- go.mod go.sum; exit 1; fi

clean:
	rm -f $(COLLECTOR_BINARY) $(API_BINARY) $(MIGRATE_BINARY)
	rm -rf $(COVERAGE_DIR) web/.next web/coverage

# ---------------------------------------------------------------- Database

db-up:
	docker run -d --name gascurve-postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=gascurve -p 5432:5432 postgres:16-alpine || docker start gascurve-postgres

db-down:
	docker rm -f gascurve-postgres

db-migrate: build-migrate
	DB_URL="$(DB_URL)" ./$(MIGRATE_BINARY) up

db-rollback: build-migrate
	DB_URL="$(DB_URL)" ./$(MIGRATE_BINARY) down 1

# ---------------------------------------------------------------- Docker

docker-build:
	docker build -f Dockerfile.collector -t $(COLLECTOR_BINARY) .
	docker build -f Dockerfile.api -t $(API_BINARY) .
	docker build -f Dockerfile.web -t gascurve-web .

# ---------------------------------------------------------------- Web

web-install:
	$(NPM) ci

web-dev:
	$(NPM) run dev

web-lint:
	$(NPM) run lint

web-typecheck:
	$(NPM) run typecheck

web-test:
	$(NPM) run test

web-test-coverage:
	$(NPM) run test-coverage
	@COVERAGE=$$(node -e "const s=require('./web/coverage/coverage-summary.json').total.lines.pct; console.log(s);"); \
	echo "Web coverage: $${COVERAGE}%"; \
	if awk -v cov="$${COVERAGE}" -v threshold="$(COVERAGE_THRESHOLD)" 'BEGIN {exit !(cov < threshold)}'; then \
		echo "FAIL: Web coverage $${COVERAGE}% is below $(COVERAGE_THRESHOLD)% threshold"; \
		exit 1; \
	fi; \
	echo "OK: Web coverage $${COVERAGE}% meets $(COVERAGE_THRESHOLD)% threshold"

web-build:
	$(NPM) run build

web-ci: web-lint web-typecheck web-test-coverage web-build

# ---------------------------------------------------------------- CI

# Everything GitHub Actions runs, in order. Catch problems here first.
ci: fmt vet lint staticcheck test-coverage build mod-verify web-ci
	@echo "All CI checks passed."
