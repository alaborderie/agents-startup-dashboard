# agents-startup-dashboard

A public, read-only web page listing **every venture the agent company has ever
attempted** — what shipped, what is in flight, and what was abandoned and why.

Live (once deployed): <https://agents-startup.antoinelaborderie.com>

> **Status:** M1 and M2 implemented. The service, snapshot ingest, embedded
> migrations, and public dashboard are available; deployment work follows in M3.

This is **internal tooling, not a product**. There is no revenue model, no
signup, no admin UI, and no analytics. It exists so that the history of what the
company tried is visible at a glance from a phone.

## What it shows

| Column | Content |
|---|---|
| **Idea title** | A few words. From `venture.json.title`. |
| **Field** | `development` / `testing` / `finance` / … (enum in the company's `config.json .fields`). |
| **Status** | `in_progress`, `abandoned`, or `deployed`; `DESIGN_REVIEW` is shown prominently while a design awaits human review. |
| **Description** | A few lines: what it is, why it exists, how it works — plus how it could make money, when the venture is a product rather than internal tooling. |
| **Link** | URL to the live product, repository, and published design canvas when applicable. |

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

### The live feed

The same push-only rule governs the "running now" panel. While a tick runs,
`bin/live-daemon.sh` posts the currently running agents every few seconds to
`POST /ingest/live`, and the page polls the public `GET /live.json` (every 3 s,
`no-store`) to animate them. `bin/live-snapshot.py` redacts anything resembling a
credential from each log tail before it leaves the laptop; the service renders
what it is given and never reads the laptop's disk.

A run whose engine writes a JSON event stream (opencode workers, the Claude
designer) arrives as typed steps in `log_events` instead of a text tail: the
agent's messages, its thinking, each tool call (`shell`, `read`, `edit`, `web`,
`task`, `todo`, `skill`, `other`) with its outcome, errors, and the final result.
The public page and HQ draw them with one shared component
(`static/livelog.js`, `static/livelog.css`): a rectangle per step, its border
colour by type, tool calls as one-line rows that open for the command, output or
diff, and the agent's own messages as the standout cards. `log_tail` remains for
plain-text logs.

**Treat every log tail as public.** `GET /live.json` is unauthenticated, so the
redaction is the only thing standing between a worker's raw output and the open
internet. The redactor is deliberately fail-closed (it drops long opaque blobs
and any assignment to a credential-shaped name, and aligns the tail to a line
boundary), but it is a heuristic, not a guarantee: do not run a worker whose
output is genuinely confidential. The endpoint is `noindex`, `no-store`, bounded
(64 runs, 64 KB per tail, 250 steps per run with per-field caps), and the panel is
scoped to a personal dashboard. The same redactor runs over every string of every
step.

Unlike snapshot ingest, the live feed is a **replace-the-whole-set** write: every
push deletes the current `live_runs` and inserts exactly what it carries. A push
with no runs therefore clears the panel. `live_state` records one row with the
laptop's `generated_at`, cycle and tick flag; the service adds `age_seconds` on
read so the page can grey out a feed whose laptop has gone away. Absence is
never treated as history here — there is nothing worth retaining about a run that
is no longer running.

### Tick schedule and queued instructions

Each full snapshot also carries the laptop's actual systemd timer `last` and
`next` firing times and the latest five `/note` instructions per venture that
have not yet reached that venture's next worker start. Sending `/note` triggers
an immediate snapshot push. The public `GET /snapshot-status.json` is polled by
the page every 15 seconds; its countdown updates locally each second. If the
timer is inactive, its scheduled time has passed, or the latest snapshot is
over three hours old, the page labels the schedule unavailable or stale rather
than predicting a firing time. **Instructions are public text** and rendered as
plain text, without HTML interpretation.

### HQ control room

`/hq` is the founder's private, real-time view of the company, and the web
equivalent of the Telegram bot: approve or reject gates, request changes, add
notes, answer blocked ventures, suggest subjects for future ventures and start
a tick. The push-only rule still holds. The laptop pushes a private HQ document
(`POST /ingest/hq`) next to the public snapshot, and every founder action is
**queued** in Postgres and **pulled** by `bin/hq-bridge.py` on the laptop, which
long-polls `GET /ingest/commands`, runs the same code the Telegram bot runs, and
reports the result. Browsers receive every push and every command result over
a WebSocket (`/hq/ws`).

There is no password. Sending `/web` to the company Telegram bot (or running
`bin/hq-bridge.py login` on the laptop) mints a single-use link valid for 15
minutes; the token travels in the URL fragment, so it never reaches a log or a
link-preview bot, and only its SHA-256 is stored. Unlike the public page, the HQ
shows gate artifacts, blocked questions and activity, so it is session-only.
The full protocol is in [`docs/hq.md`](docs/hq.md).

## Stack

- Go 1.27, stdlib `net/http` (`http.ServeMux` method+path patterns). No router.
- `html/template` + `embed.FS`. The content-hashed stylesheets and vanilla
  scripts are embedded in the binary and the dashboard uses one card layout at
  every width. **No build step, no bundler, no framework** — the live panel is
  progressive enhancement over server-rendered HTML and degrades to the static
  page without JS. The HQ page is rendered by its own script.
- WebSockets via `github.com/coder/websocket`; the hub is in-process because
  the deployment runs one replica.
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
| `GET /live.json` | public | Running agents and redacted log tails. `Cache-Control: no-store`, `X-Robots-Tag: noindex`. |
| `GET /snapshot-status.json` | public | Last/next timer firing and pending venture instructions. `Cache-Control: no-store`, `X-Robots-Tag: noindex`. |
| `POST /ingest` | `Authorization: Bearer $DASHBOARD_INGEST_TOKEN` | Venture snapshot write path. |
| `POST /ingest/live` | `Authorization: Bearer $DASHBOARD_INGEST_TOKEN` | Live running-set write path. Replace-whole-set semantics. |
| `GET /hq` | session | The founder's private control room (login screen without a session). `no-store`, strict CSP. |
| `GET /hq/login`, `POST /hq/login` | one-time link | Exchanges a magic-link token (URL fragment) for a 30-day `HttpOnly` session cookie. |
| `POST /hq/logout` | session | Ends the session. |
| `GET /hq/api/state` | session | Full HQ state (public ventures, live runs, private HQ document, commands, bridge). |
| `POST /hq/api/commands` | session, same-origin | Queues a founder action (approve, changes, reject, note, answer, resume, suggest, tick). |
| `GET /hq/ws` | session, same-origin | WebSocket: full state on connect, then every change. |
| `POST /ingest/hq` | bearer | The laptop's private HQ document (≤ 2 MB). |
| `POST /ingest/login-links` | bearer | Records the SHA-256 of a magic-link token (≤ 15 min). |
| `GET /ingest/commands?wait=25` | bearer | The laptop bridge's long poll for queued commands. |
| `POST /ingest/commands/{id}/result` | bearer | The laptop's result for one command. |

### Ingest request

```json
{
  "snapshot_id": "<sha256 of canonicalised ventures, tokens and status>",
  "generated_at": "2026-08-24T22:31:07+02:00",
  "cycle": 41,
  "source": "laptop",
  "last_tick": "2026-08-24T21:00:00+02:00",
  "next_tick": "2026-08-24T22:40:00+02:00",
  "queued_notes": {"example-venture": ["Focus on the export flow"]},
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
      "design_url": "https://design.example/canvas/…",
      "created": "2026-08-01T10:00:00+02:00"
    }
  ]
}
```

### Ingest response

```json
{ "status": "applied", "applied": 12, "skipped_stale": 0, "warnings": [] }
```

### Live ingest request

`POST /ingest/live` takes the same bearer token. Its `runs` array replaces the
entire running set: an empty array clears it.

```json
{
  "generated_at": "2026-09-24T12:00:00+00:00",
  "cycle": 151,
  "tick_running": true,
  "runs": [
    {
      "run_id": "example-venture|2026-09-24T12:00:00+00:00|developer",
      "slug": "example-venture",
      "role": "developer",
      "leg": "primary",
      "engine": "opencode",
      "model": "openai/gpt-6-sol",
      "status": "running",
      "started_at": "2026-09-24T12:00:00+00:00",
      "log_bytes": 56688,
      "log_tail": "",
      "log_events": [
        {"id": "prt_01", "kind": "message", "text": "Checking the deploy.", "ts": 1790571833464},
        {"id": "call_01", "kind": "tool", "tool": "shell", "name": "bash", "status": "done",
         "title": "go test ./...", "output": "ok  ./internal/web", "ms": 1500},
        {"id": "call_02", "kind": "tool", "tool": "edit", "name": "apply_patch", "status": "done",
         "title": "internal/web/app.go", "adds": 2, "dels": 1, "diff": "-old\n+new\n+more"}
      ]
    }
  ]
}
```

`log_tail` is plain text (already redacted, <= 64 KB) and is sent only when the
run's log is not an event stream. `log_events` is the typed transcript, oldest
first. Every step has `id` (stable across pushes) and `kind`: `message`,
`thinking`, `tool`, `error`, `notice`, `result` or `raw`. A `tool` step adds
`tool` (the category the page colours by), `name` (the engine's tool name),
`status` (`running`, `done`, `error`), `title`, and when known `detail`,
`command`, `output`, `diff`, `adds`/`dels`, `todos` (`[{text, status}]`) and `ms`.
Optional on any step: `text`, `tokens`, `ts` (epoch ms) and `sub` (the step came
from a sub-agent). The server keeps the newest 250 steps, caps each field, maps
an unknown `kind` to `raw` and an unknown `tool` to `other`, and makes ids unique.

Response: `{ "status": "ok", "runs": 1, "warnings": [] }`. Envelope limits: 1 MB
body, at most 64 runs, `generated_at` required and no more than an hour ahead. A
run missing `run_id`, `slug` or `role` is skipped with a warning rather than
rejecting the push. `GET /live.json` returns the stored set plus a server-computed
`age_seconds` so the page can dim a feed that stopped arriving.

## Configuration

All configuration is environment variables. **No secret value belongs in this
repo, in any manifest, or in any commit.**

| Variable | Required | Notes |
|---|---|---|
| `DATABASE_URL` | yes | Postgres connection string. From a Kubernetes Secret in production. |
| `DASHBOARD_INGEST_TOKEN` | yes | Shared bearer token for every `/ingest*` route. Compared in constant time. From a Kubernetes Secret in production. It also mints HQ login links, so whoever holds it can act as the founder in the HQ: rotate it if it leaks. |
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

# 4. Push a live running set (clears the panel again with "runs": [])
curl -sS -X POST http://localhost:8080/ingest/live \
  -H "Authorization: Bearer $DASHBOARD_INGEST_TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"generated_at":"2026-09-24T12:00:00Z","cycle":151,"tick_running":true,
           "runs":[{"run_id":"demo|2026-09-24T12:00:00Z|developer","slug":"demo",
           "role":"developer","engine":"opencode","model":"openai/gpt-6-sol",
           "status":"running","started_at":"2026-09-24T12:00:00Z","log_tail":"building…"}]}' | jq

# 5. Look at it
open http://localhost:8080
```

Stopping and starting `go run ./cmd/dashboard` against the same container is safe:
the embedded migration ledger records every applied SQL file and skips it on the
next startup. Tests launch an isolated Postgres 16 container automatically:

```sh
go build ./...
go vet ./...
go test ./...
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
internal/web/       html/template handlers, the HQ (hq.go, hub.go), embedded templates + CSS + JS
docs/hq.md          HQ control room protocol (laptop ⇄ server ⇄ browser)
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
