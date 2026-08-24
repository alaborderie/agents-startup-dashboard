# agents-startup-dashboard

A public, read-only web page listing **every venture the agent company has ever
attempted** — what shipped, what is in flight, and what was abandoned and why.

Live (once deployed): <https://agents-startup.antoinelaborderie.com>

> **Status:** specification only. The service described below has not been
> implemented yet, so the routes and local-development commands are not
> currently available from this repository.

This is **internal tooling, not a product**. There is no revenue model, no
signup, no admin UI, and no analytics. It exists so that the history of what the
company tried is visible at a glance from a phone.

## What it shows

| Column | Content |
|---|---|
| **Idea title** | A few words. From `venture.json.title`. |
| **Field** | `development` / `testing` / `finance` / … (enum in the company's `config.json .fields`). |
| **Status** | `in_progress`, `abandoned`, or `deployed`. |
| **Description** | A few lines: what it is, why it exists, how it works — plus how it could make money, when the venture is a product rather than internal tooling. |
| **Link** | URL to the live product if deployed; empty otherwise. |

Abandoned ventures show **why** they were abandoned. That is the most useful
information on the page and the main reason it is worth building. When a reason
was never recorded, the row renders a conspicuous *"reason not recorded"* marker
rather than hiding the gap.

## The snapshot-ingest model

Company state lives as JSON files on a laptop (`company/ventures/*/venture.json`)
that is only intermittently online. This service runs in k3s on a VPS. The laptop
therefore **pushes**; the server can never pull.

```
laptop (source of truth)                 VPS (projection)
  venture.json + NN-*.md   --bin/sync.sh-->  POST /ingest  --1 tx-->  Postgres  --> GET /
```

The rules that make this work:

- **Full-state snapshot, every push.** Each `POST /ingest` carries *every*
  venture, not a delta. Any single successful POST repairs arbitrary drift, so
  the recovery procedure for every conceivable inconsistency is "wait one tick".
  No laptop-side outbox, no retry state, no ordering machinery.
- **Idempotent upsert by `slug`.** `ON CONFLICT (slug) DO UPDATE`. Replaying the
  same snapshot twice produces the same table. There is a no-op short-circuit:
  a repeated `snapshot_id` returns `{"status":"unchanged"}` and writes nothing.
- **Status and stage may move backwards.** `deployed` → `abandoned`, or `BUILD`
  back to `ARCHITECTURE` (the ITERATE loop), are normal transitions. Any logic
  that requires either value to move only forward would be a bug, not a safety
  feature.
- **Retain forever; never delete by absence.** A venture missing from a snapshot
  keeps its row and its last known values, and gets `missing_since` stamped. It
  still renders. This makes ingest monotone in row existence: no snapshot — not
  even an empty or malformed one — can destroy history. The escape hatch for a
  genuine mistake is a `hidden` boolean flipped by hand with `psql`. No API.
- **Stale snapshots are skipped, not applied.** The upsert is guarded by
  `EXCLUDED.snapshot_generated_at >= ventures.snapshot_generated_at`, and skipped
  rows are counted back to the caller as `skipped_stale`.
- **Lenient row validation.** A bad row produces a warning in the response and in
  `ingest_snapshots.warnings`; it does not reject the envelope and discard 99
  good rows. Envelope-level problems (bad auth, empty `ventures`, body over 1 MB,
  `generated_at` more than an hour in the future) reject everything.
- **The laptop owns all interpretation.** Stage→status mapping, prose
  extraction, and abandoned-reason resolution happen in `bin/sync.sh`. The server
  stores finished plain text verbatim and renders it escaped. **There is no
  markdown parser in this service** — that is the security control for a public
  page carrying prose from internal artifacts.

The server is a projection, not a system of record. If Postgres is lost, the next
tick fully restores it.

## Stack

- Go 1.23+, stdlib `net/http` (`http.ServeMux` method+path patterns). No router.
- `html/template` + `embed.FS`. CSS inline in a `<style>` block. **Zero JS, no
  build step, no bundler.** Table above 640px, stacked cards below.
- Postgres via `jackc/pgx/v5`, pool max 4.
- **Schema migrations are embedded in the binary and run in-process at startup**,
  idempotently. They are deliberately *not* a Postgres `initdb` ConfigMap: that
  hook only runs when the data directory is empty, so every schema change after
  first boot would silently do nothing.
- Deployed as a single container to single-node k3s behind Traefik; image in
  GHCR.

## Routes

| Route | Auth | Purpose |
|---|---|---|
| `GET /` | public | The dashboard page. `Cache-Control: public, max-age=60`, `X-Robots-Tag: noindex`. |
| `GET /healthz` | public | Liveness/readiness probe. |
| `GET /robots.txt` | public | Disallow all. Public URL and indexed-by-Google are separable choices. |
| `POST /ingest` | `Authorization: Bearer $DASHBOARD_INGEST_TOKEN` | The only write path in the system. |

### Ingest request

```json
{
  "snapshot_id": "<sha256 of the canonicalised ventures array>",
  "generated_at": "2026-08-24T22:31:07+02:00",
  "cycle": 41,
  "source": "laptop",
  "ventures": [
    {
      "slug": "example-venture",
      "title": "Example venture",
      "field": "development",
      "status": "in_progress",
      "stage": "BUILD",
      "kind": "product",
      "description": "plain text, <= 600 chars",
      "monetization": "plain text, <= 300 chars, null for kind=internal",
      "abandoned_reason": null,
      "abandoned_at_stage": null,
      "url": null,
      "repo_url": "https://github.com/…",
      "created": "2026-08-01T10:00:00+02:00"
    }
  ]
}
```

### Ingest response

```json
{ "status": "applied", "applied": 12, "skipped_stale": 0, "warnings": [] }
```

## Configuration

All configuration is environment variables. **No secret value belongs in this
repo, in any manifest, or in any commit.**

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | Postgres connection string. From a Kubernetes Secret in production. |
| `DASHBOARD_INGEST_TOKEN` | yes | Shared bearer token for `POST /ingest`. Compared in constant time. From a Kubernetes Secret in production. |
| `PORT` | no | Defaults to `8080`. |
| `PUBLIC_URL` | no | Canonical public URL, used for links in the page. |

## Running locally

```sh
# 1. Postgres
docker run --rm -d --name dashboard-pg \
  -e POSTGRES_PASSWORD=dev -e POSTGRES_DB=dashboard \
  -p 5432:5432 postgres:16

# 2. The service — migrations run automatically at startup
export DATABASE_URL='postgres://postgres:dev@localhost:5432/dashboard?sslmode=disable'
export DASHBOARD_INGEST_TOKEN='local-dev-token-not-a-real-secret'
export PORT=8080
go run ./cmd/dashboard

# 3. Push a snapshot
curl -sS -X POST http://localhost:8080/ingest \
  -H "Authorization: Bearer $DASHBOARD_INGEST_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @testdata/snapshot.json | jq

# 4. Look at it
open http://localhost:8080
```

To point the real company tick at a local server instead of production:

```sh
export DASHBOARD_INGEST_URL='http://localhost:8080/ingest'
export DASHBOARD_INGEST_TOKEN='local-dev-token-not-a-real-secret'
/path/to/agent-startup/bin/sync.sh
```

## Repository layout

```
cmd/dashboard/      main: config, pool, routes, graceful shutdown
internal/store/     pgx access, embedded migrations, snapshot transaction
internal/web/       html/template handlers and the embedded template + CSS
testdata/           sample snapshots used by tests and by curl
Dockerfile          multi-stage; static binary on a minimal base
.github/workflows/  build and push ghcr.io/alaborderie/agents-startup-dashboard
```

Deployment manifests live in the separate `k3s-ovh-config` repository, and
`bin/sync.sh` lives in the separate `agent-startup` repository. Neither is
vendored here.

## Contributing

- Never push to `main`. Open a pull request. Always.
- Never commit a secret value, including in a Kubernetes manifest. Secret
  manifests ship with **empty `stringData` placeholders**; a human applies the
  real values out of band.
- Work is tracked as issues against milestones M1–M5. Each issue states its
  acceptance criteria; a PR closes its issue with `Closes #N`.
