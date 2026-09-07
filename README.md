# gascurve

Live and historical gas pricing for Arbitrum Nitro chains. gascurve explains the multi-constraint base fee pricer and shows it working: per-block backlogs, each constraint's share of the fee, owner parameter changes, where the fees go, and the batch-posting cost ArbOS attributes to batch posters, over the last hour, day, month, and all time.

Networks: Robinhood Chain, Robinhood Chain Testnet, Arbitrum One, Arbitrum Sepolia. Adding another Nitro chain is a config entry.

## How it works

- **collector** (Go) is the only thing that talks to an RPC. It updates live gas data on new heads when a WebSocket feed is configured, or every 3 seconds by default on public RPCs. It replays the pricer over every block and folds history into buckets. Its slow sample refreshes L1 pricing, fee balances, owner actions, and batch posting reports every 60 seconds.
- **api** (Go) serves REST and a WebSocket from PostgreSQL. It never calls an RPC.
- **web** (Next.js) renders the page and updates live over the WebSocket. It never calls an RPC.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the contract between the parts and [docs/SPEC.md](docs/SPEC.md) for the chain mechanics and the RPC findings the design rests on.

## Quick start

```bash
cp .env.example .env
make db-up && make db-migrate
make run-collector        # terminal 1
make run-api              # terminal 2
make web-install && make web-dev   # terminal 3, http://localhost:3000
```

Run `make tools` once, then `make ci` to reproduce the CI workflow locally. `make ci-integration`, `make ci-docker` and `make ci-chart` cover the rest. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Pre-1.0. Shapes in `docs/ARCHITECTURE.md` may change between minor versions.

## License

MIT, see [LICENSE](LICENSE).
