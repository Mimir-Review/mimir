# Mimir

Open-source AI PR review harness. Builds semantic context slices per changed function and routes them through configurable model and static analysis pipelines to produce high-signal, low-noise code review findings.

## Status

Early development (M1 in progress). The service skeleton is functional — webhook reception, job queue, database, and migrations all work. The review pipeline stages (index, planner, runtime, policy) are not yet implemented.

## Project Maintenance
- Lead Maintainer: [Dan Getz](https://github.com/quizwedge)
- Major Contributor: [Evan Volgas](https://github.com/evanvolgas)

## Architecture

- **Language:** Go 1.23+ (ADR-0001)
- **Database:** PostgreSQL 16+ only (ADR-0002)
- **Job queue:** riverqueue/river, PostgreSQL-backed (ADR-0003)
- **Semantic index:** Tree-sitter, heuristic (ADR-0004, under discussion)
- **Service mode:** Single binary, dual runtime mode (ADR-0005, under discussion)

See `adr/` for locked decisions and `scratchpad/` for in-progress design notes.

## Local Development

**Prerequisites:** Go 1.23+, Docker, [sqlc](https://docs.sqlc.dev/en/latest/overview/install.html)

```bash
# Start PostgreSQL (port 5433 to avoid conflicts with local PG)
docker compose up -d

# Run database migrations (River tables + app schema)
make build
./bin/mimir migrate up

# Verify migrations applied
./bin/mimir migrate status

# Start webhook server + River workers
./bin/mimir serve --addr :8080 --workers 4

# Run tests (requires PostgreSQL running)
make test
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | `postgres://mimir:mimir@localhost:5433/mimir?sslmode=disable` | PostgreSQL connection string |
| `MIMIR_WEBHOOK_SECRET` | _(empty)_ | GitHub webhook HMAC secret. If empty, signature validation is skipped. |

### CLI Commands

```
mimir serve              Start webhook receiver + River workers
  --addr :8080           HTTP listen address
  --workers 4            Number of concurrent review workers
  --http=false           Worker-only mode (no HTTP server)
  --workers=0            HTTP-only mode (no workers)

mimir review             One-shot PR review (not yet implemented)
  --repo owner/name      Repository in owner/name format
  --pr 123               Pull request number

mimir migrate up         Apply all pending migrations (River + app schema)
mimir migrate down       Roll back the last app migration
mimir migrate status     Show migration status
```

### Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/webhooks/github` | GitHub webhook receiver (PR opened/synchronize) |
| `GET` | `/healthz` | Health check (pings database) |

### Code Generation

SQL queries are managed by [sqlc](https://sqlc.dev). After editing files in `internal/store/queries/`:

```bash
go generate ./internal/store/...
```

## Repository Structure

```
adr/              Accepted Decision Records (immutable once accepted)
scratchpad/       In-progress design notes and open questions
cmd/mimir/        CLI entry point (cobra)
internal/
  core/           Domain types and hash functions (no I/O)
  ingest/         GitHub webhook handler
  index/          Semantic repo map (tree-sitter) [not yet implemented]
  planner/        ReviewTask generation and risk scoring [not yet implemented]
  runtime/        Model + tool execution with fan-out [not yet implemented]
  policy/         Finding filter, dedup, and escalation [not yet implemented]
  eval/           Metrics and replay [not yet implemented]
  queue/          River job types and worker
  store/          PostgreSQL persistence (sqlc-generated queries + goose migrations)
    dbsqlc/       Generated code (do not edit)
    migrations/   Goose SQL migrations
    queries/      sqlc query definitions
pkg/adapter/      Exported plugin interface types
docker/           Dockerfile (multi-stage, distroless)
sqlc.yaml         sqlc configuration
```

## License

Apache 2.0
