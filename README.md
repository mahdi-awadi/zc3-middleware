# zc3-middleware

[![CI](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/ci.yml/badge.svg)](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/ci.yml)
[![Release](https://github.com/mahdi-awadi/zc3-middleware/actions/workflows/release.yml/badge.svg)](https://github.com/mahdi-awadi/zc3-middleware/releases)

A complete **gym membership + gate access system** built on **ZKTeco
C3-series** access-control panels. It fronts a C3 panel (TCP `4370`, the PULL
protocol) and runs the whole membership layer on top: members with time-boxed
subscriptions bound to cards, a per-swipe gate decision engine, attendance
history, payments, and an embedded Arabic console + gate kiosk screen — all in
a single static Go binary, `.env`-driven, zero hardcoded config, serving
**`http://<ip>:8000/`**.

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

## How it works

```
                         ┌────────────────────────── zc3-middleware ──────────────────────────┐
                         │                                                                     │
  ┌──────────┐  TCP 4370 │  ┌───────────┐   ┌────────────┐   ┌──────────┐   ┌───────────────┐  │  HTTP :8000
  │ C3 panel │◀─────────▶│  │ supervisor│──▶│ c3 protocol│──▶│gym engine│   │  gym console  │◀─┼───────▶ reception /
  │  (doors, │  binary   │  │ (1 conn,  │   │ (frame/CRC │   │ (member? │   │  + /gym API   │  │         gate kiosk
  │  readers)│  PULL     │  │  reconnect)│  │  cards,    │   │  active? │   │  + /metrics   │  │         curl
  └──────────┘           │  └───────────┘   │  events)   │   │  opens)  │   └───────────────┘  │
                         │        │         └────────────┘   └──────────┘          ▲           │
                         │        │  swipe events     │           │  check-ins     │ SSE       │
                         │        └───────────────────┴───────────┴────────────────┘           │
                         │            (decision → door relay • gym.json • gate kiosk)          │
                         └─────────────────────────────────────────────────────────────────────┘
```

**1 — One owned connection.** The panel is single-session for control, so a
**supervisor** goroutine holds exactly one TCP connection to port 4370 and
reconnects with exponential backoff + jitter if it drops. Every command is
serialized over that connection.

**2 — The protocol layer** (`internal/c3`) speaks the reverse-engineered C3
"PULL" wire format: frames are `AA 01 <cmd> <len> <data> <crc16> 55` with a
CRC-16/ARC checksum; the panel here uses session-less mode. This layer turns
raw frames into typed operations — device info, parameters, clock, door/relay
control, data-table read/write (users, cards, timezones, holidays), and the
realtime event log. Card enroll/revoke use the binary `SETDATA`/`DELETE` table
format, validated against live hardware.

**3 — Events flow one way out.** The middleware polls the panel's key-value
realtime log (`0x79`), de-duplicates by the panel's monotonic record index, and
normalizes each swipe into an `Event` (card, reader, direction, time, type).
Every swipe feeds the **gym engine**.

**4 — Memberships decide access.** Cards are linked to members whose
subscriptions live in `gym.json`. Member cards are enrolled on the panel
*without* panel authorization (`NoPanelAuth`), so the panel denies by default
and the middleware is the sole gate. On each swipe the engine resolves
card → member → subscription and decides:
- **active** → open the door, count the visit;
- **expiring** (≤ `warn_days` left) → open + renewal warning on the kiosk;
- **expired / frozen / suspended card** → deny, with the reason on the kiosk;
- **unknown card** → deny.

Every outcome is recorded as a check-in (attendance, live feed, stats) and
broadcast over SSE to the gate kiosk screen.

**5 — Everything is one binary.** The HTTP layer exposes the `/gym` REST + SSE
API, Prometheus `/metrics`, health/readiness probes, and the **gym console**,
which is embedded into the executable (`go:embed`) and served at `/`. No
database, no runtime, no external assets — a single static file you run with a
`PANEL_ADDR`.

**Failure behavior.** The middleware is deliberately in the unlock path —
that's the point: the panel can't evaluate subscriptions. It runs on the LAN
next to the panel and reconnects automatically; membership state is plain JSON
on disk, so restarts are instant and lossless.

## Quick start

```bash
cp .env.example .env         # set PANEL_ADDR at minimum
make run                     # or: go run ./cmd/middleware
# console: http://<machine-ip>:8000/
curl localhost:8000/healthz
```

## Gym console (نادي المنارة)

The product is a complete gym membership system riding on the gate: members
with time-boxed subscriptions bound to cards, a per-swipe decision engine
(granted / renewal warning / denied / unknown), attendance heatmaps, a card
registry, payments with receipts. The Arabic RTL console is **embedded in
the binary** and served at the site root — **http://\<ip\>:8000/** — with a
fullscreen gate kiosk screen for the entrance display. See
[docs/GYM.md](docs/GYM.md).

Staff login maps onto `API_KEY`: the password field is the key. With no key
configured, access is open (development mode).

## Configuration

Everything is an environment variable (optionally seeded from `.env`; real env
always wins). See [`.env.example`](.env.example) for the annotated list.

| Variable | Default | Purpose |
|---|---|---|
| `PANEL_ADDR` | *(required)* | `host:port` of the C3 panel |
| `PANEL_ID` | `panel-1` | Logical id for logs/events |
| `IO_TIMEOUT` | `5s` | Per-command panel I/O timeout |
| `RECONNECT_MIN` / `RECONNECT_MAX` | `1s` / `30s` | Reconnect backoff bounds |
| `HTTP_ADDR` | `:8000` | API listen address (all interfaces → `http://<ip>:8000`) |
| `GYM_DATA_PATH` | `gym.json` | Members, subscriptions, cards, check-ins |
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
| `GET` | `/` | Gym console (embedded web UI) |
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness (panel link state) |
| `GET` | `/config` | Non-sensitive runtime flags for the UI |
| `GET` | `/metrics` | Prometheus metrics |
| `POST` | `/doors/{n}/open` | Manual gate open (reception) |
| `GET/POST` | `/gym/members` … | Members, subscriptions, renew, freeze, card link |
| `GET/POST` | `/gym/cards` … | Card registry (blank / suspended / lost) |
| `GET` | `/gym/summary` | Dashboard stats |
| `GET` | `/gym/checkins` | Recent gate feed |
| `GET` | `/gym/gate` | Gate decisions stream (SSE, kiosk screen) |
| `POST` | `/gym/simulate` | Inject a swipe (demo / no hardware) |

The full gym API is documented in [docs/GYM.md](docs/GYM.md).

Example — open the gate for 5s:

```bash
curl -X POST localhost:8000/doors/1/open \
  -H "Authorization: Bearer $API_KEY" -d '{"seconds":5}'
```

Example — an SSE gate decision:

```
event: gate
data: {"time":"2026-08-27T18:22:00+03:00","member_id":"MGR-1001","name":"أحمد الجبوري",
       "card_no":"497420652","result":"granted","visit":313,"days_left":16}
```

## Layout

```
cmd/middleware      entrypoint, wiring, graceful shutdown
internal/config     .env + env loading and validation
internal/c3         C3 wire protocol (frame, CRC, connect, events, control)
internal/api        HTTP layer: /gym REST + SSE API + embedded console (/)
internal/gym        membership store + gate decision engine
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
- [x] Realtime event stream ingestion (0x79 RTLog polling, de-dup)
- [x] Prometheus `/metrics`, structured logging, graceful shutdown
- [x] **Card enroll/revoke** (SETDATA `0x07` / DELETE `0x09`, binary) — validated on
      live hardware via a dummy round-trip; gated behind `ALLOW_WRITES`
- [x] **Gym membership system** — members, subscriptions, renew/freeze,
      payments + receipts, card registry, attendance heatmaps, per-swipe gate
      decisions, embedded Arabic console + gate kiosk (SSE)
- [ ] Member photo capture/storage, multi-panel supervisor, OIDC for staff

