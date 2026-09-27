# HQ control room — protocol

The HQ is a private, authenticated page at `/hq` on the existing dashboard
service (`https://agents-startup.antoinelaborderie.com/hq`). It shows the
company in real time over a WebSocket and lets the founder do what the
Telegram bot does: approve / request changes / reject gates, add notes,
answer blocked ventures, resume them, suggest new venture subjects, and ask
for a tick now.

The server still can never reach the laptop ("jarvis"). Everything the HQ
shows is **pushed** by the laptop; everything the founder does is **queued**
in Postgres and **pulled** by a laptop daemon (`bin/hq-bridge.py`) that
long-polls the server, executes the command against the on-disk state, and
reports the result back.

```
laptop                                   server (k3s)                       browser
bin/sync.sh ── POST /ingest ───────────▶  ventures ───────┐
bin/live-daemon ─ POST /ingest/live ───▶  live_runs ──────┤
bin/hq-bridge ─ POST /ingest/hq ───────▶  hq_state ───────┼─ WS /hq/ws ───▶  hq.js
bin/hq-bridge ◀─ GET /ingest/commands ──  hq_commands ◀───┴─ POST /hq/api/commands
bin/hq-bridge ─ POST /ingest/commands/{id}/result ─▶ (broadcast "command")
bin/hq-login.py / Telegram /web ─ POST /ingest/login-links ─▶ hq_login_links
```

All `/ingest/*` routes use the existing `Authorization: Bearer $DASHBOARD_INGEST_TOKEN`.
All `/hq/*` routes (except the login page itself and static assets) require a
session cookie.

---

## 1. Login (magic link, no password)

1. Laptop mints a random token `T` (32 random bytes, base64url, no padding).
2. Laptop `POST /ingest/login-links` with
   `{"token_sha256": "<64 lowercase hex of sha256(T)>", "expires_at": "<RFC3339>"}`.
   Server clamps `expires_at` to at most now+15 min. Response `{"status":"ok"}`.
3. Laptop gives the founder `https://agents-startup.antoinelaborderie.com/hq/login#t=<T>`.
   The token is in the **fragment**, so it is never sent in a GET, never logged,
   and a link-preview bot cannot consume it.
4. `GET /hq/login` serves a small page whose script reads `location.hash`,
   then `POST /hq/login` with JSON `{"token": "<T>"}`.
5. Server hashes `T`, atomically marks the unexpired, unused link as used, and
   creates a session: random 32-byte id `S`, stores `sha256(S)` with a 30-day
   expiry, sets cookie `__Host-hq_session=<S>; Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=2592000`.
   The `__Host-` prefix (which requires `Path=/` and no `Domain`) stops sibling
   `*.antoinelaborderie.com` apps from planting a cookie that shadows the session.
   Response `204`. Invalid/expired/used → `401 {"error":"link expired or already used"}`.
6. `POST /hq/logout` deletes the session and clears the cookie → `204`.

Unauthenticated `GET /hq` renders the login screen (instructions: "send /web to
the Telegram bot"), not a redirect loop. Unauthenticated `/hq/api/*` and
`/hq/ws` → `401`.

Cross-site protection: every non-GET `/hq/*` route goes through Go's
`http.CrossOriginProtection`; the WebSocket accept checks Origin == Host.

## 2. Commands

### Browser → server: `POST /hq/api/commands`

Body (JSON):

```json
{"kind": "approve", "slug": "f-0923-jev-model-router", "gate": "design", "text": "", "client_id": "b9c0…"}
```

| kind      | slug     | gate                          | text                 | laptop effect (same code as Telegram)                  |
|-----------|----------|-------------------------------|----------------------|--------------------------------------------------------|
| `approve` | required | required: build/design/deploy | ignored              | `set_gate(slug, gate, "approved", idempotent=True)`    |
| `changes` | required | required                      | required, ≤3000      | `note(slug, text)` + `set_gate(slug, gate, "revision")`|
| `reject`  | required | required                      | required, ≤3000      | `note(slug, "Founder rejection: "+text)` + `set_gate(…,"rejected")` |
| `note`    | required | —                             | required, ≤3000      | `note(slug, text)` then snapshot sync                  |
| `answer`  | required | —                             | required, ≤3000      | `note(slug, text)` + `resume(slug)` (blocked ventures) |
| `resume`  | required | —                             | —                    | `resume(slug)`                                         |
| `suggest` | —        | —                             | required, ≤2000      | append to the founder's subject queue (see §5)         |
| `tick`    | —        | —                             | —                    | `systemctl --user start --no-block agent-startup-tick.service` |

Validation: slug `^[a-zA-Z0-9_-]{1,60}$`; text trimmed, non-empty where required,
max length by kind; `client_id` optional (≤64 chars, `[A-Za-z0-9_-]`) and unique —
a repeated `client_id` returns the existing command (idempotent double-click).
Response `202 {"command": Command}`; `400 {"error": "..."}` on validation failure.

`Command` object (used everywhere):

```json
{
  "id": "c_4f6e0b9a2d1c7e83",
  "kind": "approve",
  "slug": "f-0923-jev-model-router",
  "gate": "design",
  "text": "",
  "status": "queued",            // queued | delivered | done | failed
  "message": "",                 // laptop's human-readable result, e.g. "Approved x (design). The next tick will advance it."
  "created_at": "2026-09-27T21:08:19Z",
  "delivered_at": null,
  "finished_at": null
}
```

### Laptop ← server: `GET /ingest/commands?wait=25`

Long poll. Returns immediately if any command is `queued`, or `delivered` more
than 120 s ago without a result (redelivery; the bridge dedupes by id).
Otherwise waits up to `wait` seconds (max 30) for a new one, then returns an
empty list. Every call marks the bridge as online (see `bridge` below).
Returned commands become `delivered` (attempts+1, delivered_at=now).

```json
{"commands": [Command, ...]}
```

### Laptop → server: `POST /ingest/commands/{id}/result`

```json
{"status": "done", "message": "Approved f-0923-… (design). The next tick will advance it."}
```

`status` ∈ `done|failed`; message ≤ 2000 chars. Unknown id → 404. Returns `{"command": Command}`.
A finished command is final: a second result for it is ignored (200, unchanged).

## 3. Private HQ state: `POST /ingest/hq`

The laptop pushes a single JSON document whenever it changes (the bridge
rebuilds it every few seconds and pushes on hash change, plus a heartbeat
push at least every 60 s). Body ≤ 2 MB. Server requires `generated_at`
(not > 1 h in the future), skips a document older than the stored one
(`{"status":"stale"}`), stores it verbatim (any valid JSON string, including `\u0000`), and broadcasts it.
Response `{"status":"ok"}`.

```json
{
  "generated_at": "2026-09-27T21:08:19+00:00",
  "cycle": 229,
  "ideation_mode": "autonomous",          // or "founder"
  "wip_limit_total": 3,
  "tick": {
    "running": true,
    "started_at": "2026-09-27T21:00:58+00:00",   // null when idle
    "last": "2026-09-27T19:27:58+00:00",          // systemd timer last trigger, may be null
    "next": "2026-09-27T21:00:58+00:00"           // systemd timer next trigger, may be null
  },
  "roles": [
    {"role": "inventor",  "tier": "research", "engine": "opencode", "model": "opencode-go/deepseek-v4.1-flash", "effort": "medium"},
    {"role": "designer",  "tier": "claude",   "engine": "claude",   "model": "claude-opus-5-5", "effort": ""}
  ],
  "ventures": [
    {
      "slug": "f-0923-jev-model-router",
      "title": "Self-hosted prompt-aware model router",
      "stage": "BUILD",
      "kind": "founder",                      // founder | internal | product | autonomous-derived: "product"
      "field": "development",
      "priority": 8,
      "created": "2026-09-23T12:42:02+02:00",
      "ticks": 14,
      "max_ticks": 40,
      "gate": null,                           // "build" | "design" | "deploy" when a gate file is pending
      "gate_decision": null,                  // "pending" | "approved" | "revision" | "rejected" | null
      "blocked": true,
      "block_question": "developer opened no PR …",   // null when not blocked
      "block_url": null,                      // GitHub issue URL or null
      "repo_url": "https://github.com/alaborderie/f-0923-jev-model-router",
      "url": null,                            // live product URL
      "design_url": null,                     // published design canvas URL
      "current_pr": {"number": 17, "url": "https://github.com/…/pull/17"},   // or null
      "artifacts": [
        {"name": "00-idea.md", "stage": "IDEATION", "verdict": "pass", "author": "inventor"}
      ],
      "focus": {                              // what the founder must read now, or null:
        "name": "04-design.md",               // the gate artifact while a gate is pending,
        "text": "---\nstage: DESIGN …"        // else the current stage's artifact if blocked.
      },                                      // text ≤ 12000 chars, already redacted
      "notes":  [{"ts": "2026-09-27T20:01:00+00:00", "text": "Focus on the export flow"}],   // last 5, newest first
      "events": [{"ts": "2026-09-27T03:13:07+02:00", "event": "role_done", "detail": "security via opencode/openai/gpt-6-sol rc=0"}],  // last 15, newest first, redacted
      "suggestion": {"id": "s_…", "text": "…"}   // present when this venture was opened from a founder suggestion, else null
    }
  ],
  "activity": [                              // last 40 events across all ventures, newest first
    {"ts": "…", "slug": "a-0927-210133", "title": "a-0927-210133", "event": "role_start", "detail": "inventor via opencode/… (primary)"}
  ],
  "suggestions": [                           // founder subject queue, newest first, last 30
    {"id": "s_9d2c…", "ts": "…", "text": "Tools for tabletop game masters", "status": "queued", "slug": null},
    {"id": "s_11aa…", "ts": "…", "text": "…", "status": "picked", "slug": "a-0927-210133"}
  ]
}
```

`hq.ventures` holds every venture whose stage is not `KILLED`/`ARCHIVED`, plus
terminal ones with any event in the last 72 h. The public `ventures` list (below)
remains the complete archive, including abandoned reasons.

## 4. Browser read side

### `GET /hq/api/state` and the WebSocket `state` message

```json
{
  "type": "state",
  "server_time": "2026-09-27T21:08:20Z",
  "ventures": [PublicVenture, ...],
  "live": LiveState,
  "status": SnapshotStatus,
  "hq": {"received_at": "…", "age_seconds": 3, "doc": HQDoc} ,   // null until the laptop first pushes
  "commands": [Command, ...],                                      // newest first, last 50
  "bridge": {"online": true, "last_seen": "2026-09-27T21:08:10Z"}
}
```

`PublicVenture` (existing public data, JSON form):

```json
{"slug": "…", "title": "…", "field": "development", "status": "in_progress",
 "stage": "BUILD", "kind": "founder", "description": "…", "monetization": "…",
 "abandoned_reason": "…", "abandoned_at_stage": "IDEATION", "url": "…",
 "repo_url": "…", "design_url": "…", "created": "…",
 "tokens": {"runs": 12, "fresh": 123456, "total": 999999, "cost_usd": 0.42} }   // tokens may be null
```

`LiveState` is exactly today's `/live.json` body:
`{"generated_at","cycle","tick_running","age_seconds","runs":[{"run_id","slug","role","leg","engine","model","status","started_at","log_bytes","log_tail"}]}`.

`SnapshotStatus` is today's `/snapshot-status.json` body:
`{"generated_at","last_tick","next_tick","queued_notes":{slug:[text]}}`.

### WebSocket `GET /hq/ws`

Server → client JSON text messages; the client never needs to send anything.
Server sends WS pings every 25 s. On connect: one `state`. Then deltas:

| type       | payload                                   | when                                   |
|------------|-------------------------------------------|----------------------------------------|
| `state`    | full object above                         | on connect                             |
| `live`     | `{"type":"live","live": LiveState}`       | every live push (~3 s during a tick)   |
| `ventures` | `{"type":"ventures","ventures":[…],"status": SnapshotStatus}` | an applied snapshot ingest |
| `hq`       | `{"type":"hq","hq":{"received_at","age_seconds","doc"}}` | every accepted `/ingest/hq` |
| `command`  | `{"type":"command","command": Command}`   | a command is created or changes status |
| `bridge`   | `{"type":"bridge","bridge":{"online","last_seen"}}` | bridge goes online/offline (offline = no poll for 60 s) |

`command` deltas for one id arrive in status order, with one exception: a
redelivery can race a late result and send `delivered` after `done`. Because
`done` and `failed` are final, a client keeps a finished command even if an
older status for the same id arrives later.

The client reconnects with backoff (1 s → 30 s) and, while disconnected, may
poll `GET /hq/api/state` every 10 s.

## 5. Founder subject suggestions (laptop side)

`suggest` commands append `{"id","ts","text","status":"queued","slug":null}` to
`company/suggestions.json` (a JSON array, written atomically under a lock).
When the router is about to open a new IDEATION venture it takes the oldest
queued suggestion, marks it `picked` with the new slug, records it on the
venture (`venture.json .suggestion`) and as a founder note, so the Inventor
researches that subject first. A queued suggestion also lets the router open
an idea when `ideation.mode` is `founder` (the founder asked for it).
