# zc3-middleware

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

Full contract: [`docs/openapi.yaml`](docs/openapi.yaml).

| Method | Path | Description |
|---|---|---|
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Readiness (panel link state) |
| `GET` | `/device` | Panel identity / firmware |
| `GET` | `/params?keys=a,b` | Read device parameters |
| `PUT` | `/params` | Set device parameters |
| `POST` | `/time/sync` | Sync panel clock to server time |
| `GET` | `/cards` | List cards *(gated — see roadmap)* |
| `POST` | `/cards` | Enroll/update card *(gated)* |
| `DELETE` | `/cards/{cardNo}` | Revoke card *(gated)* |
| `POST` | `/doors/{n}/open` | Pulse a lock relay |
| `POST` | `/aux/{n}` | Drive an auxiliary output |
| `POST` | `/alarm/cancel` | Cancel alarm state |
| `GET` | `/events` | Realtime access events (SSE) |

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

- [x] Connect, device info, params, time sync
- [x] Door open, aux output, cancel alarm
- [x] Realtime event stream (SSE)
- [ ] Card enroll/revoke (write path — pending table-layout validation)
- [ ] SQLite audit log + offline event backfill
- [ ] Multi-panel supervisor
- [ ] Prometheus `/metrics`, API-key store with scopes
