# Test report — zc3-middleware

Validated against **live hardware**: ZKTeco **C3-200**, serial `ACP7032460928`,
firmware `AC Ver 5.4.3.2001 Jun 15 2023`, MAC `00:17:61:FC:09:28`.

## Automated tests

```
go test ./...
ok  internal/access   (reactor policy: allowlist, reader→door map, non-card filtering)
ok  internal/api      (handlers, auth, write-gating, open-door defaults)
ok  internal/c3       (CRC-16/ARC vector, frame round-trip, RTLog parse, table decode, time encode, door mask)
ok  internal/config   (.env loading, defaults, validation, reader-door parsing)
ok  internal/store    (append/query/filter, ring eviction, file persistence + reload)
```

## Live API test pass (`scripts/api_test.sh`)

Run against the real panel via the middleware. **16 / 16 passed.**

| Endpoint | Result |
|---|---|
| `GET /healthz` `GET /readyz` | ✅ 200 |
| `GET /device` | ✅ serial, model C3-200, firmware, MAC, 2 doors |
| `GET /params` | ✅ live parameter read |
| `GET /users` | ✅ 3 real users + access rules (doors [1,2]) |
| `GET /cards` | ✅ normalized cards |
| `GET /tables` | ✅ all 19 panel tables |
| `GET /tables/{timezone,user,userauthorize,transaction}` | ✅ incl. empty paged transaction table |
| `GET /status` | ✅ live door sensor / relay / alarm |
| `GET /events/history` | ✅ audit query |
| `GET /metrics` | ✅ Prometheus format |
| `POST /cards` (writes off) | ✅ 502 gated |
| `POST /doors/x/open` (bad input) | ✅ 400 validated |

## Manually validated against hardware (protocol probes)

| Capability | Evidence |
|---|---|
| Session-less connect + CRC-16/ARC framing | connect reply `0xC8`, all frames CRC-verified |
| Realtime card events (RTLog `0x79`) | captured card `212532046`, reader 2, event 27 "Unregistered Card" |
| **Door open (both relays)** | `ControlDevice OUTPUT` → `0xC8`; **operator confirmed audible relay click on door 1 and door 2** |
| Swipe → auto-open reactor | `auto-open door=2 reader=2 card=212532046` fired from the card |
| Data-table schema + reads | `DATATABLE_CFG` full schema; `GETDATA` decoded 3 users, timezone, access levels |

## Not yet hardware-validated (deliberately)

- **Card enroll/revoke writes** (`SETDATA`/`DELETE`, `0x07`/`0x09`). Implemented
  from the documented format but **not** brute-forced against the production
  panel (it has 3 real users). Enable `ALLOW_WRITES=true` and validate once with
  a dummy credential before relying on it.
