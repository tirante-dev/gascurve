# Database migrations

Database migrations are a production API. Once a migration reaches `main`, do not edit, rename, reorder, or delete either its up or down file. Correct mistakes with a new, higher-numbered migration.

The current production baseline is application release `v1.0.1` at schema version 1. Every migration on `main` is frozen by a checksum in `internal/db/migration_policy_test.go`, whether or not it has been released yet, and the same file records the version numbering and up/down pairing rules. Migration files and the checksum policy require owner review through `CODEOWNERS`.

## Creating a migration

1. Rebase on `main` and inspect the highest migration number in `internal/db/migrations`.
2. Use the next sequential six-digit number for matching `<version>_<name>.up.sql` and `<version>_<name>.down.sql` files.
3. Make the change safe for a rolling deployment. Prefer additive changes first. Deploy readers and writers that tolerate both shapes before a later migration removes an old shape.
4. Keep transactions short. Document table rewrites, locks, backfills, required free space, and whether historical data must be recomputed in the pull request.
5. Add the final SHA-256 values to `immutableMigrationSHA256` in `internal/db/migration_policy_test.go`. This freezes the files when the schema pull request merges.
6. Add or update integration assertions for both a fresh install and an upgrade with representative existing data.
7. Run `make migration-check`, the focused integration tests, `make fmt`, `make lint`, and `make test-coverage`.

Never repair a deployed schema by changing an older file, even when a fresh database would benefit from the edit. A fresh database and an existing database must traverse the same forward migration chain.

## Fresh and upgrade coverage

CI has two migration-specific controls:

- `Migration Immutability` runs `make migration-check` and fails when a migration file does not match its owner-reviewed checksum, is missing a checksum, is missing its opposite direction, or breaks the sequential numbering.
- The PostgreSQL integration job installs all migrations into an empty schema, then separately installs the last production schema version and upgrades it to head. It compares columns, constraints, and indexes between both results and verifies representative production data survives.

`productionRelease` and `productionSchemaVersion` in `internal/db/migration_policy_test.go` identify the upgrade baseline. After a release containing a schema migration is deployed successfully, update both constants in a focused follow-up pull request. Do not advance them before rollout, because the release candidate must continue testing the real production-to-head path.

## Release and rollout

Before merging a release that contains a migration:

1. Confirm the migration files already have immutable checksums and the release candidate still upgrades from the currently deployed production version.
2. Review compatibility across the old application, new application, old schema, and new schema. Choose an explicit order for the migration and application rollout.
3. Take or verify a restorable database backup. Record any expected lock duration, backfill duration, and post-migration data checks.
4. Run the migration once with the dedicated migrate command or the controlled `database.run_migrations` setting. Do not let multiple application replicas race to perform an operationally sensitive migration.
5. Verify the reported schema version, application health, collector progress, API reads, and any recomputation before completing rollout.
6. After production is healthy, advance the production baseline constants so the next schema change tests upgrades from the new deployed version.

Release-please still owns application version tags. Schema version numbers are independent and always increase from the files in `internal/db/migrations`.

## Corrections and rollback

Production corrections are forward-only. If a released migration is incomplete or wrong, add the next migration to repair the schema or data. State the dependency and required rollout order in that pull request.

Down migrations support local development, automated tests, and a controlled rollback before the new application has depended on the schema. They are not the default production recovery mechanism. After writes occur against a new schema, reversing DDL can discard or misinterpret data. Prefer a forward repair. For destructive failures, stop writers and restore the verified backup or point-in-time recovery target according to the incident plan.

If a migration fails and leaves the migration table dirty, stop the rollout. Inspect the database and the partially applied statements before changing version state. Do not rerun, force, or edit the old migration until the exact database state and recovery plan are understood.

## Concurrent schema pull requests

Migration numbers encode merge order, not authoring order. Coordinate the next number with other active schema pull requests and list any dependency in each pull request.

- If two branches choose the same number, the first merged branch keeps it. Rebase the other branch on `main`, rename both of its migration files to the next number, update its checksum entry and tests, then rerun all migration checks.
- If one schema change depends on another, merge the prerequisite first. Rebase the dependent branch and record the required merge and rollout order in its pull request.
- Never insert a migration below a version already on `main`, reuse a number, or renumber a migration that has merged.

These rules also apply when schema pull requests are developed in parallel but intended for the same application release.
