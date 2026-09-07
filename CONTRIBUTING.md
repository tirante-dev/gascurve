# Contributing to gascurve

Thanks for helping out. The short version: run `make ci` before you push, keep PR titles in Conventional Commit form, and never make CI less strict to get a green build.

## Development setup

Prerequisites: Go 1.26+, Node 22+, PostgreSQL 16 (Docker is fine), and Make. Install the non-standard Go commands at the hosted CI versions into the ignored `.tools/bin` directory before running checks:

```bash
make tools
```

`make ci` checks the toolchain before starting any work. If a command is missing or has the wrong version, it stops with the bootstrap command to run. The pinned versions have one source of truth in `scripts/tool-versions.env`, which is shared by local and hosted CI.

```bash
cp .env.example .env            # edit DB_URL and RPC URLs if needed
make db-up                      # local Postgres in Docker
make db-migrate
make run-collector              # in one terminal
make run-api                    # in another
make web-dev                    # http://localhost:3000
```

## Checks

After `make tools`, `make ci` runs the CI workflow locally: Go format check, vet, lint, staticcheck, govulncheck, tests with the coverage gate, race tests, build, module verify, then the web install, lint, typecheck, tests with coverage gate, and build. `make ci-integration` (needs `TEST_DB_URL`), `make ci-docker` and `make ci-chart` cover the remaining jobs. Individual targets are listed in the Makefile and in `CLAUDE.md`.

## Pull requests

- Title in Conventional Commit form: `feat: ...`, `fix: ...`, `docs: ...`, `deps: ...` (Dependabot only), etc. CI enforces this and release-please builds the changelog from it.
- One logical change per PR. Include tests. Colocate Go tests with the package and web tests next to the source file.
- Do not add lint or type-check exceptions to get a build green. Fix the cause.
- No em dashes anywhere: code, comments, docs, commit messages.

## Releases

release-please maintains a release PR on `main`. Merging it tags a release and publishes the Docker images.
