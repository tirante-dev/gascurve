# gascurve

Live and historical gas pricing for Arbitrum Nitro chains. gascurve explains the multi-constraint base fee pricer and shows it working: per-block backlogs, each constraint's share of the fee, owner parameter changes, where the fees go, and what the chain actually pays Ethereum, over the last hour, day, month, and all time.

Networks: Robinhood Chain, Robinhood Chain Testnet, Arbitrum One, Arbitrum Sepolia. Adding another Nitro chain is a config entry.

## How it works

- **collector** (Go) is the only thing that talks to an RPC. It follows each chain's head, samples the gas precompiles every second, replays the pricer over every block, folds history into buckets, and reads owner actions and batch posting reports out of the chain itself.
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

`make ci` runs every check CI runs. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Pre-1.0. Shapes in `docs/ARCHITECTURE.md` may change between minor versions.

## License

MIT, see [LICENSE](LICENSE).
