# CLAUDE.md

## Project Overview

gascurve shows live and historical gas pricing for Arbitrum Nitro chains (Robinhood Chain, Robinhood Testnet, Arbitrum One, Arbitrum Sepolia): the multi-constraint base fee pricer, per-constraint backlogs, owner parameter changes, fee destinations, and L1 posting costs. Single repo, three deployables:

- **collector** (`cmd/collector`): the only RPC client. Follows chain heads, samples precompiles, replays the pricer, writes PostgreSQL, emits `NOTIFY gascurve_live`.
- **api** (`cmd/api`): Chi REST API plus WebSocket at `/api/v1/ws`, reads PostgreSQL only.
- **web** (`web/`): Next.js App Router frontend. Talks to the api only. The browser never calls an RPC.

`docs/ARCHITECTURE.md` is the contract between them (schema, endpoints, JSON shapes, WS protocol). `docs/SPEC.md` has the chain mechanics and verified RPC behaviour.

## Commands

```bash
make tools              # install the hosted CI versions of required Go tools under .tools/bin
make ci                 # the CI workflow locally: fmt-check, vet, lint, staticcheck, govulncheck, test-coverage, test-race, build, mod-verify, web-install, web-ci
make ci-integration     # go-integration job, needs TEST_DB_URL (Postgres)
make ci-docker          # docker job: build the three images and Trivy-scan them (needs docker, trivy)
make ci-chart           # chart-test workflow: helm lint --strict, template checks, ct lint if installed
make build              # gascurve-collector, gascurve-api, gascurve-migrate
make test               # go test ./...
make test-coverage      # 90% line coverage gate over ./internal/...
make test-race          # go test -race
make test-integration   # needs TEST_DB_URL (Postgres); tagged `integration`
make lint / lint-fix    # golangci-lint (.golangci.yml)
make fmt / fmt-check    # gofmt -s + goimports -local github.com/tirante-dev/gascurve (fmt-check never writes)
make db-up / db-migrate / db-rollback
make web-dev / web-lint / web-typecheck / web-test / web-test-coverage / web-build
```

After any Go change run at least `make fmt && make lint && make test-coverage`. After any web change run `make web-lint && make web-typecheck && make web-test-coverage`.

## Go conventions

- Module `github.com/tirante-dev/gascurve`, Go 1.26. Packages under `internal/`: `config` (Viper), `logger` (Zap wrapper), `chains` (network registry), `nitro` (JSON-RPC client, batching, precompile ABI, internal-tx and OwnerActs decoding, token-bucket pacing), `pricer` (pure pricing model and replay), `db` (sqlx, migrations), `collector`, `api`.
- `internal/pricer` is pure and must match nitro's `arbos/l2pricing/model.go` bit for bit (integer basis points, `ApproxExpBasisPoints` with accuracy 4). Never "simplify" it to floating point.
- The collector is the only package that may import `nitro` for network calls. The api must not.
- Wei is `*big.Int` in Go, `NUMERIC(40,0)` in Postgres, decimal string in JSON. Gas and bips are `uint64`/`int64`, JSON numbers.
- Tests live next to the code. Integration tests are behind `//go:build integration`. RPC is mocked with `httptest`; the DB layer is behind an interface so handlers and the collector are unit-testable.
- Logging through `internal/logger`. Config through `internal/config` (YAML then env overrides, `NETWORK_<NAME>_*` for per-network values).

## Web conventions

- Next.js App Router, React 19, TypeScript strict, Tailwind, Recharts, Vitest with jsdom. `@/*` maps to `src/*`.
- Network-aware routes: `/[network]`. API client in `src/lib/api/` (`core.ts` does timeout and retry). Live data via `useLive` (WebSocket with reconnect, polling fallback). Shapes in `src/types/` mirror `docs/ARCHITECTURE.md` exactly.
- Tailwind utility classes only, no CSS modules. Dark and light both supported.
- Coverage gate (90% lines) covers `src/lib/**`, `src/hooks/**`, `src/utils/**`.

## Writing style

Never use em dashes anywhere: UI copy, comments, commit messages, PR titles, docs. Use a period, comma, colon, or parentheses.

## CI standards

Never make CI less restrictive. No `eslint-disable`, `@ts-ignore`, `any` casts, `//nolint` without a reason, skipped tests, or lowered thresholds to get a build green. Fix the cause.

## Pull request titles

Conventional Commits: `type: subject` or `type(scope): subject`. Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `deps`, `revert`. `deps` is reserved for Dependabot. No tooling prefixes such as `[codex]`.

## Releases

release-please keeps a release PR open on `main`. Merging it tags a release, which publishes the three Docker images. One version for the whole repo; `web/package.json` is bumped as an extra file.

## RPC etiquette (matters for public endpoints)

The Robinhood public RPC meters JSON-RPC calls per IP, including items inside a batch request, rejects with 429 in bursts of a few hundred calls or roughly 4,000 calls per minute, and blocks non-browser user agents at the WAF. Keep the per-network budget at `calls_per_second` (default 4), never send concurrent batches for one network, always set a `User-Agent`, and back off on 429. State calls (`eth_call` at a block) only work for the last few minutes of blocks; history must come from headers, logs, and internal transactions.
