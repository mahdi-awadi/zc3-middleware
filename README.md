# zc3-middleware

[![CI](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/ci.yml/badge.svg)](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/ci.yml)
[![Release](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/release.yml/badge.svg)](https://github.com/mahdi-awadi/zc3-middleware/releases)

Production middleware for **ZKTeco C3-series** access-control panels. It fronts a
C3 panel (TCP `4370`, the PULL protocol) with a clean **REST + Server-Sent-Events**
HTTP API: read device info, open doors, drive aux outputs, sync time, and stream
realtime access events. Single static Go binary, `.env`-driven, zero hardcoded
config.

Validated against real hardware: **C3-200**, firmware `AC Ver 5.4.3.2001`.

---

## Why a middleware

The panel makes access decisions **locally** and speaks an unauthenticated binary
protocol on port 4370. This service is the safe, structured boundary in front of
it: it never sits in the unlock decision path, it exposes an authenticated API to
your app, and it keeps the raw protocol off the public network.

```
[C3 panel :4370] ──raw C3, LAN only──> [zc3-middleware] ──HTTPS REST+SSE, API-key──> [your app / VPS]
```

## Quick start

```bash
cp .env.example .env         # set PANEL_ADDR at minimum
make run                     # or: go run ./cmd/middleware
curl localhost:8080/device
curl -N localhost:8080/events   # live event stream (SSE)
```

## Web console

A full admin UI is **embedded in the binary** and served same-origin at
**<http://localhost:8080/ui>** (the root `/` redirects there). No build step, no
separate deploy. It manages every part of the API:

- **Overview** — device identity, live door status, service metrics
- **Live events** — realtime swipe stream (SSE)
- **Cards & roles** — enrol / revoke, assign role (student / teacher / administrator / custom)
- **Access rules** — create roles with time windows, holidays, and days; live decisions
- **Doors** — pulse relays, aux outputs, cancel alarm, live status
- **Data tables** — read any panel table
- **Device** — read/set parameters, sync clock, restart
- **Settings** — API base URL + key (stored in the browser)

Enroll/revoke require `ALLOW_WRITES=true`; the console shows a banner when writes
are locked. Set an `API_KEY` and enter it under Settings to authenticate.

## Configuration

Everything is an environment variable (optionally seeded from `.env`; real env
always wins). See [`.env.example`](.env.example) for the annotated list.

| Variable | Default | Purpose |
|---|---|---|
| `PANEL_ADDR` | *(required)* | `host:port` of the C3 panel |
| `PANEL_ID` | `panel-1` | Logical id for logs/events |
| `IO_TIMEOUT` | `5s` | Per-command panel I/O timeout |
| `RECONNECT_MIN` / `RECONNECT_MAX` | `1s` / `30s` | Reconnect backoff bounds |
| `HTTP_ADDR` | `:8080` | API listen address |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Slowloris guard |
| `HTTP_SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown budget |
| `DOOR_DEFAULT_SECONDS` | `5` | Default lock-relay pulse |
| `EVENT_BUFFER_SIZE` | `64` | Per-SSE-subscriber buffer |
| `API_KEY` | *(empty)* | Bearer token; empty disables auth (dev only) |
| `CORS_ORIGINS` | *(empty)* | Comma-separated allowed origins; `*` = all |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Structured logging |

## API

Full contract: [`docs/openapi.yaml`](docs/openapi.yaml). A browsable reference is
in [`docs/api.html`](docs/api.html) (open locally, or import the spec into
[editor.swagger.io](https://editor.swagger.io) for a try-it console).

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness (panel link state) |
| `GET` | `/metrics` | Prometheus metrics |
| `GET` | `/device` | Panel identity / firmware |
| `GET` | `/status` | Live door sensor / relay / alarm state |
| `GET` | `/params?keys=a,b` | Read device parameters |
| `PUT` | `/params` | Set device parameters |
| `POST` | `/time/sync` | Sync panel clock to server time |
| `POST` | `/restart` | Reboot the panel |
| `GET` | `/users` | List enrolled users + access rules |
| `GET` | `/cards` | List enrolled cards |
| `POST` | `/cards` | Enroll/update card (validated; gated by `ALLOW_WRITES`) |
| `DELETE` | `/cards/{cardNo}` | Revoke card (validated; gated) |
| `GET` | `/tables` | List panel data tables |
| `GET` | `/tables/{name}` | Read any table (user, timezone, holiday, transaction, …) |
| `POST` | `/doors/{n}/open` | Pulse a lock relay |
| `POST` | `/aux/{n}` | Drive an auxiliary output |
| `POST` | `/alarm/cancel` | Cancel alarm state |
| `GET` | `/events` | Realtime access events (SSE) |
| `GET` | `/events/history?card=&door=&limit=&offset=` | Query the audit log |

Example — open door 1 for 5s:

```bash
curl -X POST localhost:8080/doors/1/open \
  -H "Authorization: Bearer $API_KEY" -d '{"seconds":5}'
```

Example — an SSE access event:

```
event: access
data: {"time":"2026-07-09T16:11:20+03:00","card_no":"212532046","door":2,
       "event_type":27,"event_name":"Unregistered Card","verify":4,"in_out":"out"}
```

## Layout

```
cmd/middleware      entrypoint, wiring, graceful shutdown
internal/config     .env + env loading and validation
internal/c3         C3 wire protocol (frame, CRC, connect, events, control)
internal/api        REST + SSE HTTP server
internal/supervisor panel connection lifecycle (reconnect + event fan-out)
docs/               openapi.yaml, C3-PROTOCOL.md, ARCHITECTURE.md
deploy/             systemd unit, Tailscale guide
scripts/            Python protocol probes used to reverse-validate the panel
```

## Build & deploy

```bash
make build                 # host binary -> bin/middleware
make build-linux           # static linux/amd64 -> dist/middleware
make docker                # scratch image
```

Deploy on a Linux box **on the panel's LAN**; reach it from your cloud/VPS over a
tunnel — see [`deploy/TAILSCALE.md`](deploy/TAILSCALE.md). **Never** port-forward
`4370` to the internet: the C3 protocol has no authentication.

## Protocol

Reverse-validated against the live panel and documented in
[`docs/C3-PROTOCOL.md`](docs/C3-PROTOCOL.md). Frame:
`AA 01 <cmd> <len:LE16> <data> <crc16:LE> 55`, CRC-16/ARC (poly `0xA001`).
This firmware uses **session-less** mode; realtime events come from the ASCII
key-value RTLog (`0x79`), de-duplicated by the panel's record index.

## Roadmap

- [x] Connect, device info, params, time sync, restart
- [x] Door open, aux output, cancel alarm, live door/relay/alarm status
- [x] Realtime event stream (SSE) + durable JSONL audit log + history query
- [x] Data-table reads: users, cards, access levels, timezones, holidays, transactions
- [x] Prometheus `/metrics`, structured logging, graceful shutdown
- [x] Unit + integration tests, full live API test pass (`scripts/api_test.sh`)
- [x] **Card enroll/revoke** (SETDATA `0x07` / DELETE `0x09`, binary) — validated on
      live hardware via a dummy round-trip; gated behind `ALLOW_WRITES`
- [x] Embedded admin web console (`/ui`) managing the whole API
- [x] **Role enforcement engine** — user-defined roles with time-window / day /
      holiday / direction rules applied to live events; enforced roles are
      panel-denied and gated by the middleware, admins stay panel-native
- [ ] Multi-panel supervisor, API-key store with scopes, OIDC for the frontend

## Roles &amp; enforcement

Roles are stored on the panel in each user's `Group` field; their rules live in
`access.json` (created with school defaults, edited live from the **Access
rules** page or the `/roles` + `/access/config` API).

- An **enforced** role (student, teacher, …) is enrolled *without* panel
  authorization, so the panel denies it and the middleware opens the door only
  when the swipe's direction + time + day satisfy the role — enabling rules the
  panel can't express natively (e.g. *enter 07:00–07:30, exit 13:30–14:00*).
- A **panel-native** role (administrator) is authorized on the panel with a
  timezone and doors, so it works locally even if the middleware is down.
- Enforcement is off until enabled (master switch on the Access rules page).

| Method | Path | Description |
|---|---|---|
| `GET` | `/roles` | List roles |
| `POST` | `/roles` | Create / update a role |
| `DELETE` | `/roles/{group}` | Delete a role |
| `GET` / `PUT` | `/access/config` | Enforcement switch, holidays, reader→door map |
| `GET` | `/access/decisions` | Recent enforcement decisions |
