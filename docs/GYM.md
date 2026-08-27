# Gym subsystem (نادي المنارة)

A gym membership layer on top of the C3 gate middleware: members with
time-boxed subscriptions bound to access cards, a per-swipe decision engine,
attendance history, a card registry, and a full Arabic RTL console.

## The idea

The middleware already owns the gate (one C3 connection, realtime swipe
events, door relays). The gym layer adds *who is allowed and until when*:

```
swipe at reader ──▶ gym engine ──▶ decision
                                    ├─ granted  → open door, count visit
                                    ├─ warn     → open door + "renew soon" (≤ warn_days left)
                                    ├─ denied   → keep closed (expired / frozen / card blocked)
                                    └─ unknown  → keep closed (card not registered)
                        │
                        └──▶ SSE /gym/gate ──▶ live dashboard feed
```

Cards linked to gym members are enrolled on the panel with `NoPanelAuth`,
so the panel itself never opens the door for them — the middleware is the
sole gate.

## Gates

A **gate** is one physical entrance: a ZKAccess C3 panel address plus a door
(relay) number — one C3 drives up to 4 doors, so several gates can share an
address. Gates are managed from the console's **البوابات** screen:

- **فحص الشبكة** sweeps the machine's local subnets for devices listening on
  TCP 4370 and confirms each with a real C3 handshake (serial, model, door
  count). Found devices are added as gates in one click.
- **اتصال** makes a gate's panel the live connection (the supervisor redials
  it immediately); the choice persists across restarts and overrides
  `PANEL_ADDR`.
- **صلاحيات الخطط** maps each access level to the gates it may open. On a
  swipe the door resolves to its gate; if the member's plan doesn't include
  it the entry is denied with «الخطة لا تشمل هذه البوابة». A plan with no
  selection is allowed through every gate.

## Console

Open **`http://<ip>:8000/`** — the console is the site root. Screens: login,
dashboard (stats, weekly chart, live
gate feed), members list with status filters, member profile (attendance
heatmap, event log, subscription, electronic card), add member (card scan
flow + plan builder), cards registry, and renew & pay.

Staff login maps onto the middleware's `API_KEY`: the password field *is*
the API key (stored in the browser). With no `API_KEY` configured, login is
open.

## Data

Everything persists in one JSON file (`GYM_DATA_PATH`, default `gym.json`):
members (with subscription + payment + log history), the card registry, and
the last 20k check-ins. Member statuses are derived, never stored:

| status | meaning |
|---|---|
| `active` | current subscription end is in the future |
| `expiring` | ends within `warn_days` (default 7) — gate still opens, with a warning |
| `expired` | ended — gate denies |
| `frozen` | freeze active — gate denies; unfreezing extends the end date by the frozen days |

## API

All under `/gym/*`, protected by the same `API_KEY` bearer auth.

```
GET  /gym/summary                      dashboard stats (today, active, expiring, revenue, week)
GET/PUT /gym/config                    enabled, warn_days, open_seconds, default prices

GET  /gym/members?status=&q=           list (status: active|expiring|expired|frozen|nocard)
POST /gym/members                      {..member fields.., card_no?, plan:{months,access,price?,discount?,pay_method?,start?}}
GET/PUT/DELETE /gym/members/{id}
POST /gym/members/{id}/renew           {months, access, price?, discount?, pay_method?, start?}
POST /gym/members/{id}/freeze          {days?, reason?}
POST /gym/members/{id}/unfreeze
POST /gym/members/{id}/card            {card_no} — link (+ panel enroll, NoPanelAuth)
DELETE /gym/members/{id}/card          unlink (+ panel revoke)
GET  /gym/members/{id}/attendance?weeks=16
GET  /gym/members/{id}/checkins?limit=50

GET  /gym/cards                        registry (linked | blank | suspended | lost)
POST /gym/cards                        {serial} — register a blank card
PUT  /gym/cards/{serial}               {status} — suspend / mark lost / restore

GET  /gym/gates                        gates + active gate + plan→gate map
POST /gym/gates/scan                   sweep the LAN for ZKAccess C3 panels (TCP 4370 + C3 probe)
POST /gym/gates                        {name, addr, door} — add a gate (scan result or manual)
PUT  /gym/gates/{id}                   patch name / addr / door
DELETE /gym/gates/{id}                 remove (scrubs plan mappings + active state)
POST /gym/gates/{id}/connect           make this gate's panel the live connection
POST /gym/gates/{id}/open              pulse the gate's door relay (reception)
PUT  /gym/gates/plans                  {"gym":["gate-1"],...} access level → allowed gates

GET  /gym/checkins?limit=20            recent gate feed
GET  /gym/gate                         SSE stream of gate decisions (live feed)
GET  /gym/scan                         last swipe seen (card-link flow); DELETE clears it
POST /gym/simulate                     {card_no, dry_run?} — inject a swipe (demo / no hardware)
```

`access` levels: `gym` (صالة فقط), `gym_classes` (صالة + حصص), `full`
(كامل الوصول). Defaults per month come from `/gym/config`; every price is
editable per member.

Panel writes are best-effort: responses carry `"panel_synced": false` when
the C3 was unreachable — the store remains the source of truth.
