.PHONY: all build build-collector build-api build-migrate build-matrix run-collector run-api test test-coverage test-race test-integration migration-check lint lint-fix vet fmt fmt-check staticcheck govulncheck mod-verify tools tools-check tool-goimports tool-golangci-lint tool-staticcheck tool-govulncheck check-goimports check-golangci-lint check-staticcheck check-govulncheck test-tooling ci ci-integration ci-docker ci-chart clean db-up db-down db-migrate db-rollback docker-build docker-scan chart-lint chart-template web-install web-dev web-lint web-typecheck web-test web-test-coverage web-build web-ci

GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOTOOLCHAIN:=$(shell awk '$$1 == "toolchain" { print $$2; exit }' go.mod)
export GOTOOLCHAIN
MODULE=github.com/tirante-dev/gascurve
TOOLS_BIN_DIR ?= $(CURDIR)/.tools/bin
GOIMPORTS=$(TOOLS_BIN_DIR)/goimports
GOLANGCI_LINT=$(TOOLS_BIN_DIR)/golangci-lint
STATICCHECK=$(TOOLS_BIN_DIR)/staticcheck
GOVULNCHECK=$(TOOLS_BIN_DIR)/govulncheck
COLLECTOR_BINARY=gascurve-collector
API_BINARY=gascurve-api
MIGRATE_BINARY=gascurve-migrate
COVERAGE_THRESHOLD ?= 90
COVERAGE_PACKAGES ?= ./internal/...
COVERAGE_DIR=coverage
COVERAGE_FILE=$(COVERAGE_DIR)/coverage.out
COVERAGE_HTML=$(COVERAGE_DIR)/coverage.html
# Cross-build matrix, same as the go-build job in .github/workflows/ci.yml.
BUILD_PLATFORMS ?= linux/amd64 linux/arm64 darwin/arm64
DIST_DIR=dist
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

# Static cross-builds for every platform CI builds, into $(DIST_DIR)/.
build-matrix:
	@set -e; for platform in $(BUILD_PLATFORMS); do \
		goos=$${platform%/*}; goarch=$${platform#*/}; \
		echo "build $$goos/$$goarch"; \
		for cmd in collector api migrate; do \
			GOOS=$$goos GOARCH=$$goarch CGO_ENABLED=0 $(GOBUILD) -o $(DIST_DIR)/gascurve-$$cmd-$$goos-$$goarch ./cmd/$$cmd; \
		done; \
	done

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

migration-check:
	$(GOTEST) -count=1 -run '^TestReleasedMigrationsImmutable$$' ./internal/db

# Install all non-standard Go commands at the exact versions used by hosted CI.
# They stay inside the repository so unrelated global tools cannot affect checks.
tools:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh install

tool-goimports:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh install goimports

tool-golangci-lint:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh install golangci-lint

tool-staticcheck:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh install staticcheck

tool-govulncheck:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh install govulncheck

tools-check:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh check
	@echo "OK: required CI tools are available at their pinned versions"

check-goimports:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh check goimports

check-golangci-lint:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh check golangci-lint

check-staticcheck:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh check staticcheck

check-govulncheck:
	@TOOLS_BIN_DIR="$(TOOLS_BIN_DIR)" scripts/tools.sh check govulncheck

test-tooling:
	@sh scripts/tools_test.sh

lint: check-golangci-lint
	"$(GOLANGCI_LINT)" run --timeout=5m ./...

lint-fix: check-golangci-lint
	"$(GOLANGCI_LINT)" run --timeout=5m --fix ./...

vet:
	$(GOCMD) vet ./...

fmt: check-goimports
	@GOFMT="$$( $(GOCMD) env GOROOT )/bin/gofmt"; "$$GOFMT" -s -w .
	"$(GOIMPORTS)" -w -local $(MODULE) .

# Same check CI runs; never writes files.
fmt-check: check-goimports
	@set -eu; \
	GOFMT="$$( $(GOCMD) env GOROOT )/bin/gofmt"; \
	GOFMT_FILES="$$("$$GOFMT" -l -s .)"; \
	GOIMPORTS_FILES="$$("$(GOIMPORTS)" -l -local $(MODULE) .)"; \
	if [ -n "$$GOFMT_FILES" ] || [ -n "$$GOIMPORTS_FILES" ]; then \
		echo "FAIL: formatting issues, run 'make fmt'"; \
		echo "gofmt:"; echo "$$GOFMT_FILES"; \
		echo "goimports:"; echo "$$GOIMPORTS_FILES"; \
		exit 1; \
	fi; \
	echo "OK: formatting"

staticcheck: check-staticcheck
	"$(STATICCHECK)" ./...

govulncheck: check-govulncheck
	"$(GOVULNCHECK)" ./...

# `go mod tidy -diff` prints what tidy would change and exits non-zero; it
# never writes go.mod or go.sum.
mod-verify:
	$(GOCMD) mod verify
	@if ! $(GOCMD) mod tidy -diff; then echo "FAIL: go.mod/go.sum not tidy, run 'go mod tidy'"; exit 1; fi

clean:
	rm -f $(COLLECTOR_BINARY) $(API_BINARY) $(MIGRATE_BINARY)
	rm -rf $(DIST_DIR) $(COVERAGE_DIR) web/.next web/coverage

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

# Same policy as the CI Trivy step.
docker-scan:
	trivy image --exit-code 1 --severity CRITICAL,HIGH --ignore-unfixed $(COLLECTOR_BINARY)
	trivy image --exit-code 1 --severity CRITICAL,HIGH --ignore-unfixed $(API_BINARY)
	trivy image --exit-code 1 --severity CRITICAL,HIGH --ignore-unfixed gascurve-web

# ---------------------------------------------------------------- Chart

CHART=charts/gascurve

# values.schema.json requires a database whenever the collector or api is
# enabled, so lint runs once per documented configuration in $(CHART)/ci
# (the same files ct lint uses). Same as .github/workflows/chart-test.yml.
chart-lint:
	@set -e; for values in $(CHART)/ci/*-values.yaml; do \
		echo "== helm lint --strict --values $$values"; \
		helm lint $(CHART) --strict --values "$$values"; \
	done

# The render assertions live in one script so this target and
# .github/workflows/chart-test.yml can never drift apart.
chart-template:
	bash scripts/chart-checks.sh

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

# `make ci` is the CI workflow (.github/workflows/ci.yml) minus the jobs that
# need external services or tools: it never writes tracked files (fmt-check,
# not fmt; go mod tidy -diff, not tidy), cross-builds the same platform matrix
# as the go-build job (build-matrix) and is self-contained on a fresh clone
# after `make tools` installs the pinned Go commands (web-install). The
# remaining CI jobs
# have their own targets so they can be run when the prerequisites exist:
#   ci-integration  Postgres in TEST_DB_URL (go-integration job)
#   ci-docker       docker + trivy (docker job)
#   ci-chart        helm, optionally ct (chart-test.yml)
# Secret scanning (secrets-scan.yml, gitleaks) has no local target.
# The recursive invocation makes the preflight a strict phase boundary, even
# under parallel make. No check starts until every required command is present.
ci: tools-check
	@$(MAKE) migration-check fmt-check vet lint staticcheck govulncheck test-tooling test-coverage test-race build-matrix mod-verify web-install web-ci
	@echo "All CI checks passed."

ci-integration: test-integration

ci-docker: docker-build docker-scan

ci-chart: chart-lint chart-template
	@if command -v ct > /dev/null 2>&1; then ct lint --config charts/ct.yaml --all; else echo "ct not installed, skipping ct lint"; fi
