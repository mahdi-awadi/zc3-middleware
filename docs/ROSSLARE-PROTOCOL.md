# AC-215 wire protocol (decoded from our own captures)

Link: RS-485, 9600 8N1, PC is master, panel answers only when asked.
Sources: tap.py captures of AxTraxNG 27.7.1.20 <-> the real AC-215F (2026-10-29), then
fakepanel.py captures, plus the command names AxTraxNG prints in its own debug log
(`C:\Program Files (x86)\Rosslare\AxTraxNG Server\Logs\27.7.1.20\AxtraxService.*.axtraxlog`,
lines `send message <Name> , values ...` and `CMD <Answer> - ...`).

## Frame

```
FF FF FF 55 | type | addr | b2 | b3 | seq | len | cmd, data... (len bytes) | check
```

- `FF FF FF` preamble, `55` start byte.
- `type`: PC sends 02 (03 seen on the second identify probe of an address); panel answers 01.
- `addr`: panel address 1-32 (DIP 4-8).
- `b2 b3`: 01 01 from the PC, 02 01 from the panel.
- `seq`: PC counter, echoed in the reply. A retry reuses the same seq.
- `len`: number of bytes in cmd + data.
- `check`, two styles (the panel answers in the style it was asked):
  - 1 byte: low 8 bits of the sum of every byte from `type` to the last data byte
    (RequestFirmVer, EnterBoot, DownloadFirmware and their answers).
  - 2 bytes, high byte first: CRC-16/XMODEM (poly 0x1021, init 0) over the same bytes
    (DeletePanelData; checked on 2 frames).
- The real panel's reply is followed by two `00` bytes (line turnaround noise, ignore).
- AxTraxNG timeouts: 9 s between identify probes, 40 s between retries of a normal command, 3 tries.

## Commands seen

| code | name (AxTraxNG log) | dir | data | notes |
|------|------|-----|------|-------|
| 6A | RequestFirmVer | PC -> panel | none | sum check |
| EB | AnswerFirmVer | panel -> PC | `"ac215v04_05_10" 00 00 "btl_ac225v_01_02" 11 F3 02 10 00 00` | firmware + loader names, last 6 bytes unknown |
| 64 | DeletePanelData | PC -> panel | `01` | CRC check; first command after adding a panel (wipes it) |
| 42 | EnterBoot | PC -> panel | none | starts the firmware loader |
| 80 | Answer80 | panel -> PC | none | plain ack; real reply to EnterBoot |
| 44 | DownloadFirmware | PC -> panel | `80 00 <addr hi> <addr lo> ...` 137 bytes | 128 bytes of the .hex per block |
| C5 | AnswerFirmwareAck | panel -> PC | `00` | |
| 62 | UpdateClock | PC -> panel | `YY MM DOW DD hh mm ss` binary | `1A 0A 04 1D 15 30 23` = 2026-10-29 Thu 21:48:35; DOW 04 = Thursday |
| 40 | RequestEvents | PC -> panel | `11 F4` (meaning unknown) | polled about twice a second while connected |
| C1 | AnswerEvents | panel -> PC | status + events, see "AnswerEvents" below | a plain 80 here counts as a timeout; 6 timeouts = NotResponding |
| 54 | GlobalAntipass | PC -> panel | 16 bytes, all 00 | setup download |
| 37 | PanelData | PC -> panel | 28 bytes | setup download |
| 56 | InputData | PC -> panel | 96 bytes | setup download, 4 inputs x 8 bytes, rest 00 |
| 5A | ReaderData | PC -> panel | 36 bytes, first byte = reader (01 / 00) | setup download, once per reader |
| 5B | CustomReaderData | PC -> panel | 64 bytes, first byte = reader + page (01 / 11 / 00 / 10) | card-format (Wiegand) rules |
| 38 | DoorData | PC -> panel | 16 bytes | door times |
| 36 | EventFilter | PC -> panel | 160 bytes, FF = report, FE = filtered | |
| 3E | Holidays | PC -> panel | 128 bytes, FF = unused | |
| 34 | UserData | PC -> panel | 36 bytes: `slot(2)` then the user record | delete = same slot, rest all FF |
| 32 | CodeData | PC -> panel | 12 bytes: `slot(2) user-slot(2) 00 00 00 00 00 FC cardHi cardLo` | delete = same slot, rest all FF |
| 22 | OutputsManual | PC -> panel | 16 outputs x `mode 00 min sec` | mode 00 = leave, 01 = back to normal, 02 = open for min:sec, 03 = open permanently; slots = OUT1 (Door), OUT1A, OUT2, OUT2A, then 12 unused |

## Schedules, users and settings (decoded 2026-10-29 with the fake panel)

### 3A TimezoneConfig: allowed times

Sent in 4 frames per time zone: `part zone` + 2 days x 64 bytes.
part 1 = Sunday + Monday, 2 = Tuesday + Wednesday, 3 = Thursday + Friday, 4 = Saturday + Holiday.
A day = 16 intervals x `startHour startMin endHour endMin`, FF FF FF FF = unused.
Zone numbers: 0 = never (no access), 1 = always, 2.. = zones created in AxTraxNG.

```
3A  01 02  08 00 0d 1e  ff..(60)  09 00 0b 1e  ff..(60)   zone 2: Sun 08:00-13:30, Mon 09:00-11:30
```

### 34 UserData: one user (36 bytes)

| bytes | meaning |
|-------|---------|
| 0-1 | user slot |
| 2-5 | PIN, digits as hex nibbles, padded with 0 (`12 34 00 00` = 1234); `FF FF FF FF` = no PIN |
| 6-7 | valid from: date, same packing as events (`3D AA` = 2026-10-29) |
| 8-9 | valid until: date (`3F CA` = 2026-12-31); `FF FF` = never expires |
| 10-11 | 00 00 (probably valid-from time hh mm) |
| 12-13 | time zone per reader: [Reader 1, Reader 2]. Master group = `01 01` (always / always); group "Entry R1" = `02 00` (zone 2 on Reader 1, no access on Reader 2) |
| 20 | 80 (flags, not mapped) |
| 24-25 | valid-until time hh mm (`17 3B` = 23:59) |
| 28-31 | FF FF FF FF (probably the duress PIN) |
| others | 00 |

Access groups are not sent to the panel as such: AxTraxNG turns the user's group into the two
per-reader time-zone bytes. So "entry allowed 08:00-13:30, exit any time" = zone 2 on Reader 1, zone 1 on Reader 2.

### 32 CodeData: one card (12 bytes)

`card slot(2) user slot(2) code(8, big endian)`: Wiegand 26 facility 123 card 4661 = `00 00 00 00 00 7B 12 35`.
There is no bulk command: AxTraxNG sends one 34 + one 32 per person (about 10 cards a second at 9600 baud).

### 38 DoorData (16 bytes)

`flags(4) openMin openSec extMin extSec heldMin heldSec forcedMin forcedSec 00 00 00 00`
Default `00 00 00 01 00 04 00 08 00 1e 00 00`: open 4 s, extended 8 s, held-open alarm 30 s, forced 0.
Changing open time to 7 s gave `00 00 00 01 00 07 ...`. Flag bits not mapped.

### 3E Holidays (128 bytes)

64 dates x 2 bytes, same packing as event dates, `FF FF` = empty. `39 CA` = 2026-12-25.

### 5A ReaderData (36 bytes)

| byte | meaning |
|------|---------|
| 0 | reader: 00 = Reader 1, 01 = Reader 2 |
| 3 | keypad type: 00 inactive, 01 Wiegand 6 Rosslare |
| 5 | operation mode: 00 inactive, 01 card only, 02 PIN only, 03 card or PIN, 04 desktop, 05 no access |
| 11 | 05 on Reader 1 (In), 06 on Reader 2 (Out): probably direction |

5B CustomReaderData (card-format rules) is sent right after each 5A and was not changed in any test.

AxTraxNG's init order for a new panel: RequestFirmVer, DeletePanelData, UpdateClock, RequestEvents,
then the setup download: GlobalAntipass, PanelData, InputData, ReaderData + CustomReaderData x2
(per reader), DoorData, EventFilter, Holidays. Users and cards follow only when the database has them.

## AnswerEvents (C1), decoded with the fake panel

RequestEvents `40 HH LL` asks for event number `HHLL` (the next one AxTraxNG wants; 0x11F4 = 4596 at first).
The answer data after `C1`:

```
16 header bytes | count x 16-byte records | 2 bytes (00 00 works) | count x 2-byte event numbers (big endian)
```

- Header byte 2 = number of records. A single `00` (or all-zero header) = no events, AxTraxNG polls again
  about twice a second. Other header bytes carry door / input / output status (AxTraxNG logs
  DoorStatusChanged / InputStatusChanged / OutputStatusChanged when they change): not mapped yet.
- Up to 2 records per answer work. With more, AxTraxNG takes one record and reads the event number
  right after it. A record with type 0 also ends the list.
- Event numbers must be the ones asked for, in sequence, or AxTraxNG logs
  "Received wrong event number : X instead of Y", drops that record and skips ahead.

Record (offsets inside the 16 bytes):

| byte | meaning |
|------|---------|
| +0, +1 | card slot (big endian) of the card used, for access-granted events (AxTraxNG IdCardSlot) |
| +2 | bits 0-4 day, bits 5-7 year bits 4-6 |
| +3 | bits 4-7 month, bits 0-3 year bits 0-3 (year = 2000 + 7-bit value) |
| +4 | seconds |
| +5 | minutes |
| +6 | bits 0-4 hour (top bits unknown) |
| +7 | event type (table below) |
| +8 | event subtype: for access denied = the reason (01 = unknown code) |
| +9 | event source: high nibble = kind, low nibble = number (table below) |
| +10, +11 | facility / site code, big endian (denied events; `00 7B` = 123) |
| +12 .. +17 | card code, 48-bit big endian. NOTE: AxTraxNG reads 6 bytes here, so the last 2 come from the 2 bytes after the record (the `00 00` before the event numbers, or the next record's +0..+1). How a real panel fills them is to be checked on a healthy board |
| +14, +15 | user slot (big endian) for access-granted events (AxTraxNG IdEmpSlot) |

Event types (names as AxTraxNG prints them; "Unknown Event" types skipped):

| type | event |
|------|-------|
| 01-10 | processed but never stored: not event types (maybe status records) |
| 11 | Access Granted, AccessGrantedCodeWithFacility |
| 12 | Access Granted, AccessGrantedCode |
| 13 | Access Granted, AccessGrantedPINCode |
| 19 | Access Recorded |
| 21 | Access Denied, AccessDeniedCodeWithFacility (subtype = reason) |
| 22 | Access Denied, AccessDeniedCode |
| 23 | Access Denied, AccessDeniedPINCode |
| 31 / 32 | Output opened / closed |
| 33 / 34 | Input opened / closed |
| 35 / 36 | Siren started / stopped |
| 37 / 38 | Input armed / disarmed |
| 3A | Reader mode changed |
| 3C | Reader bell key pushed |
| 3D | Door interlock by REX |
| 43 / 44 | Reader mode changed by time zone / returned to default at time zone end |
| 47 / 48 / 49 | Timed / door / global antipassback forgive by time zone |
| 51 / 52 | Output unlocked / re-locked by PC |
| 53 / 54 | Reader mode changed by PC / returned to default by PC |
| 55 / 56 | Input disarmed by PC / returned to default by PC |
| 63 | Alarm started |
| 71 / 72 / 73 | User dual authentication initiated (card + facility / card / PIN) |
| 81 | Panel reset (power on) |
| 82 | Panel loads factory settings |
| 83 | Panel firmware download succeeded |
| 84 | Manual panel reset, factory settings loaded |
| 85 / 86 | Remote modem answered / disconnected |
| 87 / 88 | Communication failed for over 1 minute / restored |
| 89 / 8A / 8B | Event memory full / low / normal |
| 8C / 8D | AC power fail / restored |
| 8E / 8F | Low battery / battery OK |
| 90 | Configuration mode |
| 91 / 92 / 93 | Access logged (card + facility / card / PIN) |

Access-denied reasons (subtype of 21): 00 none, 01 unknown code (primary reader type),
02 wrong facility code, 03 unknown code (secondary reader type); 04-0F show no reason text.

Sources:

| source | location |
|--------|----------|
| 01 | the panel |
| 11 | Door 1 |
| 21 / 22 | Reader 1 / Reader 2 |
| 31-37 | voltages: aux, input, expansion, readers, system, aux power board, battery |
| 41-44 | inputs: Door REX (IN1), Door Monitor (IN1A), Spare Input 2 (IN2), Spare Input 2A (IN2A) |
| 51-54 | outputs: Door (OUT1), Output 1A, Output 2, Output 2A |
| 61-66 | camera outputs (AxTraxNG describes them as "Camera A / B momentary output") |

Header status bytes (partial; AxTraxNG only logs changes): byte 2 = event count, byte 3 changes
both readers' status, byte 11 = the four inputs (00 open, FF closed). Bytes 0, 1, 4-10, 12-15 showed
no visible change when set to FF.

A turnstile pass therefore arrives as type 11 (granted) from source 21 or 22 with the card slot in +0..+1
and the user slot in +14..+15; a refused card as type 21 with the facility in +10..+11 and the code from +12.

Example, 2026-10-29 22:12:06: `+2 = 3D` (year 26 = 001 1010 -> 001 << 5 | day 29), `+3 = AA` (month 10 << 4 | 1010).

Event types seen so far: 1-16 are processed but not stored (probably real event types that need valid
reader / card fields); 17-80 are stored as "Unknown"; AxTraxNG flags 0x29, 0x2A, 0x39, 0x3F, 0x40, 0x45, 0x46
as "Invalid Event Type". Still to find: the type for access granted / denied, and where the reader and
card sit in +10..+15. Tools: eventsweep.ps1, rawevents.ps1, oneevent.ps1, statusmap.py.

### Test user (fake panel, 2026-10-29)

User "Test Card", user number 77, access group Master, card Wiegand 26 facility 123 / card 4660:

```
34 UserData  00 01 ff ff ff ff 3d aa ff ff 00 00 01 01 00 00 00 00 00 00 80 00 00 00 00 00 00 00 ff ff ff ff 00 00 00 00
32 CodeData  00 01 00 01 00 00 00 00 00 7b 12 34          <- 7B = 123, 1234 = 4660
```
Unknown in UserData so far: `3d aa`, `01 01`, `80`. Next samples to take: a PIN, another access group, a valid-until date.
Find Panels probes each address twice about 9 s apart; no reply = no panel there.
A reply AxTraxNG doesn't recognize is logged as `Received: Network 1 - ...` with no `CMD` name.
