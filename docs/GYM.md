# Gym subsystem (نادي المنارة)

A gym membership layer on top of the C3 gate middleware: members with
time-boxed subscriptions bound to access cards, a per-swipe decision engine,
attendance history, a card registry, and a full Arabic RTL console + gate
kiosk screen.

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
                        └──▶ SSE /gym/gate ──▶ gate kiosk screen + live dashboard feed
```

Cards linked to gym members are enrolled on the panel with `NoPanelAuth`,
so the panel itself never opens the door for them — the middleware is the
sole gate. The existing roles engine (school mode) ignores group-0 cards, so
both subsystems coexist without double door-opens.

## Console

Open **`/ui/gym/`**. Screens: login, dashboard (stats, weekly chart, live
gate feed), members list with status filters, member profile (attendance
heatmap, event log, subscription, electronic card), add member (card scan
flow + plan builder), cards registry, renew & pay, and the fullscreen gate
kiosk (`تشغيل شاشة البوابة` from the login screen).

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

GET  /gym/checkins?limit=20            recent gate feed
GET  /gym/gate                         SSE stream of gate decisions (kiosk screen)
GET  /gym/scan                         last swipe seen (card-link flow); DELETE clears it
POST /gym/simulate                     {card_no, dry_run?} — inject a swipe (demo / no hardware)
```

`access` levels: `gym` (صالة فقط), `gym_classes` (صالة + حصص), `full`
(كامل الوصول). Defaults per month come from `/gym/config`; every price is
editable per member.

Panel writes are best-effort: responses carry `"panel_synced": false` when
the C3 was unreachable — the store remains the source of truth.
