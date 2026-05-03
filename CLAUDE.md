# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Lancache Monitor** is a sidecar container for an existing
[lancache-monolithic](https://github.com/lancachenet/monolithic) install. It does two things:

1. **Visualizes traffic** by tailing the lancache nginx access log
   (`/data/logs/access.log` inside the lancache container, mounted RO into this
   one) and aggregating per-minute counters into SQLite. The dashboard shows
   throughput, hit/miss ratio, top CDNs, and currently active client IPs.
2. **Edits and reloads `rate-limit.conf`** through a web UI. After saving, it
   runs `nginx -t && nginx -s reload` inside the lancache container by exec'ing
   through the Docker socket. If `nginx -t` rejects the new config, the
   previous file is restored automatically.

## Tech Stack

- **Backend:** Go 1.25 (`lancache-monitor` module), single static binary, no CGO.
- **Reactive UI:** [Datastar](https://data-star.dev/) via `github.com/starfederation/datastar-go` — backend-driven hypermedia over SSE.
- **UI components:** [Basecoat CSS](https://basecoat.dev/) — component classes only (no Tailwind utilities; see CSS section below).
- **Charts:** Chart.js loaded from jsDelivr CDN.
- **Storage:** `modernc.org/sqlite` — pure-Go SQLite driver.
- **Log rotation:** `github.com/fsnotify/fsnotify`.
- **Docker exec:** `github.com/docker/docker/client`.
- **Logging:** `github.com/s00500/env_logger`.

## Build & Run

```bash
go mod download         # first run only
go build ./...
go test ./...
go run .                # uses .env if present
```

Or in Docker:

```bash
docker compose -f docker-compose.example.yml up -d --build
```

## Environment Variables

| Variable                   | Default                              | Purpose                                              |
|----------------------------|--------------------------------------|------------------------------------------------------|
| `LCM_ADDR`                 | `:8080`                              | HTTP listen address                                   |
| `LCM_THEME`                | `teal`                               | `mono` \| `teal` \| `gold`                            |
| `LCM_PASSWORD`             | (unset → auth disabled)              | Single-password login                                 |
| `LCM_LOG_PATH`             | `/data/logs/access.log`              | Path to lancache access.log (mount RO)                |
| `LCM_RATELIMIT_PATH`       | `/etc/nginx/conf.d/rate-limit.conf`  | Path to rate-limit.conf (mount RW)                    |
| `LCM_DB_PATH`              | `/data/monitor.db`                   | SQLite file for per-minute aggregates                 |
| `LCM_RETENTION_DAYS`       | `30`                                 | How many days of history to keep                      |
| `LCM_LANCACHE_CONTAINER`   | `lancache`                           | Container name for `docker exec nginx -s reload`      |
| `LCM_DOCKER_HOST`          | `unix:///var/run/docker.sock`        | Docker daemon endpoint                                |
| `LCM_SESSION_SECRET`       | (auto-generated next to DB)          | HMAC key for session cookies                          |

## Architecture

Three goroutines in one Go process:

1. **Tailer** ([tailer.go](tailer.go)) — `tail -F`-style follow on the access log, parses lines, fans out to live tracker + aggregator. Handles log rotation via fsnotify.
2. **Live tracker** ([live.go](live.go)) — in-memory rolling window (5 min by default) of per-IP and per-host activity. Used for the "active downloads" view.
3. **Aggregator** ([aggregator.go](aggregator.go)) — flushes per-minute counters into SQLite every 60 s. Provides query helpers for trend charts and 24h totals.

HTTP serves Datastar SSE endpoints ([handlers.go](handlers.go)). The dashboard subscribes to `/api/dashboard` once on load and the server pushes signal updates every 2 seconds, which the Datastar client morphs into the table rows and triggers a Chart.js redraw.

The reload path ([reloader.go](reloader.go)) uses the Docker SDK to `ContainerExecCreate` + `ContainerExecAttach` for `sh -c "nginx -t && nginx -s reload"` inside the lancache container. Stdout/stderr are captured and shown in the UI verbatim.

## Lancache Access Log Format

```
[$time_local] $remote_addr $request_method "$request_uri" $http_range $status $body_bytes_sent $upstream_cache_status $host $upstream_status $upstream_response_time "$http_user_agent"
```

`$upstream_cache_status` is one of `HIT | MISS | BYPASS | EXPIRED | STALE | UPDATING | REVALIDATED | -`. `HIT` and `REVALIDATED` count as cache hits; everything else counts as a miss.

The parser is in [tailer.go](tailer.go); see `TestParseLogLine_*` for the contract.

## File Structure

```
.
├── main.go              # entrypoint, env config, embedded FS, route wiring
├── handlers.go          # Datastar SSE handlers + helpers (humanBytes, toastJS)
├── tailer.go            # access.log follower + parser
├── tailer_test.go       # log line parser tests
├── aggregator.go        # SQLite per-minute aggregates + queries
├── aggregator_test.go   # aggregator round-trip tests
├── ratelimit.go         # read/write rate-limit.conf, sanity check, atomic write
├── ratelimit_test.go    # write/restore round trip
├── reloader.go          # Docker exec wrapper around nginx -t && nginx -s reload
├── auth.go              # HMAC session cookie middleware
├── live.go              # rolling per-IP / per-host activity tracker
├── go.mod / go.sum
├── Dockerfile
├── docker-compose.example.yml
├── .env.example
├── .dockerignore
├── templates/
│   ├── layout.html      # base shell, nav, theme CSS, vendor scripts
│   ├── dashboard.html   # stats grid + chart + active IP/host tables
│   ├── ratelimit.html   # editor + reload button + nginx output panel
│   └── login.html       # standalone password form
├── static/
│   ├── app.css          # glass morphism design system
│   └── vendor/          # basecoat.min.css/js, datastar.js, fonts
├── themes/              # mono, teal, gold JSON
└── llmdocs/             # datastar.md + basecoat.md (reference for future agents)
```

## CSS & Styling — IMPORTANT

The Basecoat CSS bundle (`basecoat.min.css`) **contains ONLY component classes — no Tailwind utility classes.** Classes like `flex`, `gap-4`, `text-3xl`, `mb-4`, `max-w-6xl`, etc. do NOT exist in the bundle and silently have no effect.

Use [static/app.css](static/app.css) for layout, spacing, and colors. The theme tokens (`--theme-accent`, `--theme-accent-rgb`, `--theme-bg-gradient-from`, etc.) are emitted by the Go `Theme.CSS()` method in [main.go](main.go) and inlined into `<style>` in the layout.

Available glass-morphism classes:

| Class           | Purpose                                  |
|-----------------|------------------------------------------|
| `.glass`        | Base semi-transparent surface            |
| `.glass-card`   | Card with glass surface + rounded corners|
| `.page-wrap`    | Main content container                    |
| `.navbar`       | Sticky top nav with glass effect          |
| `.nav-tab`      | Tab/link in nav                          |
| `.stats-grid`   | 4-column responsive stat cards            |
| `.stat-*`       | Stat card content                         |
| `.lcm-table`    | Tabular data with theme-aware borders     |
| `.btn-primary`  | Accent-gradient button                    |
| `.btn-ghost`    | Subtle outlined button                    |

## Datastar Patterns

- All frontend reactivity uses `data-*` attributes; no separate JS framework.
- State is in **signals** prefixed with `$`. Initialize with `data-signals='{...}'`, bind with `data-bind`, display with `data-text`.
- Backend emits SSE events: `datastar-patch-elements` (HTML), `datastar-patch-signals` (JSON), or `ExecuteScript()` (JS).
- The dashboard uses one long-lived `data-on-load="@get('/api/dashboard')"` SSE that the server keeps writing to every 2 s.
- Toasts are dispatched via `ExecuteScript()` firing a `basecoat:toast` `CustomEvent` — see [handlers.go](handlers.go) `toastJS()`.

See [llmdocs/datastar.md](llmdocs/datastar.md) for the complete attribute and SDK reference, and [llmdocs/basecoatcss.md](llmdocs/basecoatcss.md) for the component catalog.

## Testing

```bash
go test ./...
```

- [tailer_test.go](tailer_test.go) — log line parser, including each `upstream_cache_status` value and IPv4-mapped IPv6 normalization.
- [aggregator_test.go](aggregator_test.go) — ingest + flush + query round trip against a temp SQLite file.
- [ratelimit_test.go](ratelimit_test.go) — atomic write, snapshot/restore, sanity-check rejection.

End-to-end smoke test against a real lancache deployment is described in [README.md](README.md).
