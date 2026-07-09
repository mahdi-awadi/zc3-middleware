# ZKTeco C3 Access Control Panel — Wire Protocol Reference

Target hardware: ZKTeco **C3-100 / C3-200 / C3-400** (and OEM clones / inBio family) running the
**"PULL SDK" / "C3 communication protocol"** over **TCP port 4370**.

> This is **NOT** the standalone attendance-terminal protocol (the `0x5050`-framed / `CMD_*`
> protocol implemented by `pyzk`, `zklib`, `adrobinoga/zk-protocol`). That is a different device
> family. The C3 access panels use the binary datagram described here, framed by `0xAA … 0x55`.

Live reference panel for empirical validation: `192.168.0.102:4370`, MAC OUI `00:17:61` (ZKTeco),
thttpd web server, firmware ~2015. Factory default IP is `192.168.1.201`.

## Confidence legend

Every non-trivial field is tagged:

- **[CONFIRMED]** — corroborated by at least one authoritative implementation
  (`vwout/zkaccess-c3-py` readme + source, `bdragon300/pyzkaccess` docs) and consistent with
  ZKTeco's PULL SDK docs.
- **[LIKELY]** — strong knowledge / single-source; matches the model but not cross-checked byte-for-byte.
- **[UNVERIFIED — most likely: X]** — must be validated against the live panel or a packet capture
  before shipping. Do not treat as ground truth.

A consolidated **Confidence Map** is at the end (§13).

## Primary sources used

1. `vwout/zkaccess-c3-py` — pure-Python C3 library (readme documents the datagram + command IDs).
   Also `zkaccess-c3-lua`.
2. `bdragon300/pyzkaccess` — Python/CLI wrapper over the Windows `plcommpro.dll` PULL SDK
   (documents table models, enums, parameter names).
3. ZKTeco "Access PULL SDK" (`plcommpro.dll`) documentation and `Pull-SDK-Demo` /
   `ZKTecoStandAlonePullSDK` community mirrors (function-level: `Connect`, `ControlDevice`,
   `GetRTLog`, `GetDeviceParam`, `SetDeviceParam`, `GetDeviceData`, `SetDeviceData`).

Where sources disagree it is called out inline.

---

## 1. TCP session model

### 1.1 Transport

- TCP, single connection, **port 4370**. Panel is the server, middleware is the client.
- One request → one response, strictly synchronous on the socket. There is **no unsolicited push**
  from the panel; realtime events are obtained by **polling** (see §10.4). **[CONFIRMED]**
- All multi-byte integers are **little-endian** unless stated. **[CONFIRMED]**

### 1.2 Datagram framing

Every request and every response uses the same frame: **[CONFIRMED]**

```
 Offset  Size  Field         Value / meaning
 ------  ----  ------------  --------------------------------------------------
   0      1    Start         0xAA   (fixed)
   1      1    Version       0x01   (fixed)
   2      1    Command       command id on request; 0xC8 = "reply/OK" on response
   3      2    Length        LE uint16 = number of bytes in Data field
   5      L    Data          payload (L = Length); may begin with a session header (§1.4)
  5+L     2    Checksum      LE uint16, CRC-16 over bytes [1 .. 4+L] (Version..last Data byte)
  7+L     1    End           0x55   (fixed)
```

Total frame size = `Length + 8`.

- `Start` (0xAA) and `End` (0x55) are fixed sentinels. **[CONFIRMED]**
- `Version` is `0x01`. **[CONFIRMED]**
- `Command` on a **response** is `0xC8` (200) meaning "command accepted / here is your reply".
  An error reply uses a different code (see §1.6). **[CONFIRMED]** that `0xC8` = OK reply.
- `Length` counts **only the Data field**. **[CONFIRMED]**

### 1.3 Checksum algorithm

`vwout` documents the checksum as a **CRC-16** computed **over the whole frame except the Start byte,
the two checksum bytes, and the End byte** — i.e. over `Version, Command, Length(2), Data(L)`,
bytes at offsets `1 .. 4+L`. **[CONFIRMED that it is CRC-16 over that span]**

The **exact CRC variant** is **[UNVERIFIED — most likely: CRC-16/CCITT "Kermit"]**: reflected input/output,
polynomial `0x1021` (reflected form `0x8408`), init `0x0000`, no final XOR. Go implementation:

```go
// crc16 — CRC-16/CCITT (Kermit): poly 0x8408 (reflected 0x1021), init 0x0000, refin/refout, xorout 0.
// VALIDATE against a captured panel frame before trusting; if it fails, try the additive fallback below.
func crc16(data []byte) uint16 {
    var crc uint16 = 0x0000
    for _, b := range data {
        crc ^= uint16(b)
        for i := 0; i < 8; i++ {
            if crc&0x0001 != 0 {
                crc = (crc >> 1) ^ 0x8408
            } else {
                crc >>= 1
            }
        }
    }
    return crc // emit little-endian: low byte first
}
```

Pseudocode:

```
crc = 0
for each byte b in (Version, Command, LengthLo, LengthHi, Data...):
    crc = crc XOR b
    repeat 8 times:
        if (crc AND 1) != 0: crc = (crc >> 1) XOR 0x8408
        else:               crc = crc >> 1
checksum_bytes = little_endian(crc & 0xFFFF)   // low byte at offset 5+L, high byte at 6+L
```

**Fallback to test if the CRC above does not verify** — the older ZK additive checksum (16-bit words
summed, one's-complement). The C3 family is believed to use the true CRC-16, but confirm empirically:

```
sum = 0
for each 16-bit LE word w in payload (pad last odd byte with 0x00):
    sum += w
checksum = ~sum & 0xFFFF   // one's complement
```

**Validation recipe (do this first, before anything else):** open the socket, send a CONNECT frame,
capture the panel's reply with `tcpdump`/Wireshark, and confirm the reply's stored checksum equals
`crc16(replyBytes[1:len-3])`. Once the algorithm reproduces a real panel frame, everything else follows.

### 1.4 Session header (session mode)

There are two connect styles. The **session style** (command `0x76`) prefixes the Data field of
**every subsequent packet** with a 4-byte session header: **[CONFIRMED]**

```
 Data offset  Size  Field
    0          2    SessionId    LE uint16 — assigned by panel at connect
    2          2    MessageNr    LE uint16 — per-command counter (a.k.a. reply counter)
    4          …    Payload      the actual command payload
```

- `SessionId`: obtained from the CONNECT reply; echoed by client on all later frames. **[CONFIRMED]**
- `MessageNr` (reply counter): initialised to **`0xFEFE` (= -258 as int16)** and **incremented by 1
  for each new command**; the response echoes the same number, which is how request/response are
  paired. **[LIKELY]** — the `-258` initial value is documented by `vwout`; confirm the exact
  increment/echo behaviour against the panel.

The **simple style** (command `0x01`) does **not** use the session header — the Data field is the raw
payload. **[CONFIRMED that two styles exist]**. Recommended: use session style (`0x76`) as pyzkaccess/
plcommpro does.

### 1.5 Connect / disconnect handshake

**Connect (session), command `0x76`:** **[LIKELY]**

```
Request:  AA 01 76 <len> <data> <crc> 55
          - data begins with SessionId=0x0000, MessageNr=0xFEFE, then (optionally) a password payload
Response: AA 01 C8 <len> <data> <crc> 55
          - the panel returns the SessionId to use for the rest of the session
```

After connect, use the returned `SessionId` and increment `MessageNr` on every command.
Password: the PULL SDK connection string carries `passwd=`; when empty (our panel), no auth payload.
Exact password-hashing scheme is **[UNVERIFIED]** (our panel has no comm password, so this is moot
for the live device — leave empty).

**Disconnect, command `0x02`:** send `AA 01 02 <len> <session-header> <crc> 55`; panel replies `0xC8`.
Always disconnect cleanly; a lingering session can block reconnects until timeout. **[CONFIRMED cmd id]**

### 1.6 Response / error handling

- Success reply: `Command` byte = `0xC8` (200). **[CONFIRMED]**
- On error the reply command byte is **not** `0xC8`; the PULL SDK maps negative error codes
  (`PULL_SDK_ERRORS`) — e.g. unsupported table, bad param. Exact on-wire error byte value is
  **[UNVERIFIED — most likely: `0xC9`/`0xAA`-style negative code in the data]**. Treat "reply command
  != 0xC8" as failure and read any ASCII error text from the Data field.

---

## 2. Command set (CmdID values)

From `vwout/zkaccess-c3-py` (command byte at frame offset 2). **[CONFIRMED]**

| CmdID  | Name                         | Meaning |
|--------|------------------------------|---------|
| `0x01` | CONNECT (no session)         | Simple connect, no session header on later frames |
| `0x02` | DISCONNECT                   | Close session |
| `0x03` | SET DATETIME                 | Write the panel RTC (packed time, §7) |
| `0x04` | GET PARAM                    | `GetDeviceParam` — read `key=value` device parameters (§3) |
| `0x05` | CONTROL DEVICE               | Door/relay/aux output, cancel alarm, restart, normal-open enable (§5) |
| `0x06` | GET DATATABLE CONFIG         | Read a data table's field layout / options (precedes `0x08`) |
| `0x08` | READ DATATABLE (query)       | `GetDeviceData` — read records from a table (users, cards, txn…) (§4) |
| `0x0B` | GET RTLOG                    | Read realtime event buffer (RTLog) (§6) |
| `0x14` | DEVICE DISCOVERY             | UDP/broadcast discovery of panels |
| `0x76` | CONNECT (session)            | Session connect; assigns SessionId |
| `0x79` | RTLOG (key-value/text form)  | Realtime log as text key=value records (alt to `0x0B`) |
| `0xC8` | RESPONSE / ACK               | Reply command byte meaning "OK" |

**Commands NOT in the vwout list but required for a full middleware:**

- **SET PARAM** (`SetDeviceParam`): **[UNVERIFIED — most likely: reuses `0x04`]** with a
  `key=value,key=value` Data payload (get vs set distinguished by presence of `=value`). Some builds
  use a distinct id. Validate: send `0x04` with `DeviceName=Test` and read back with a `0x04` get.
- **WRITE DATATABLE** (`SetDeviceData`) and **DELETE DATATABLE** (`DeleteDeviceData`): needed to enroll/
  revoke cards. **[UNVERIFIED — most likely: a dedicated id near the read id, e.g. `0x09` write /
  `0x0A` delete]**. `vwout` is largely read-only for tables, so the write id is not documented there;
  confirm from a `plcommpro` capture or by probing. See §4.4 for the payload format, which IS known.

---

## 3. Device parameters — `GetDeviceParam` / `SetDeviceParam` (cmd `0x04`)

Text key/value interface. **[CONFIRMED]**

**GET request** Data payload = comma-separated list of parameter names, e.g.:

```
~SerialNumber,DeviceName,IPAddress,NetMask,LockCount,ReaderCount
```

**GET response** Data payload = comma-separated `key=value` pairs, e.g.:

```
~SerialNumber=0123456789,DeviceName=C3-400,IPAddress=192.168.0.102,NetMask=255.255.255.0,LockCount=4,ReaderCount=4
```

**SET request** Data payload = comma-separated `key=value` pairs to write (see command caveat in §2).

> Separator is **[LIKELY comma `,`]** for the C3 PULL SDK. Some ZK text interfaces use `\t` or `\n`;
> if comma fails, try tab. Validate with a single-key round-trip.

### 3.1 Known parameter keys

**System / read-mostly (often `~`-prefixed):** **[CONFIRMED names]**

| Key | Meaning |
|-----|---------|
| `~SerialNumber`        | Device serial number (read-only) |
| `~DeviceName`          | Model/product name string |
| `~ZKFPVersion`         | Fingerprint algorithm version |
| `~IsOnlyRFMachine`     | 1 = card-only (no biometrics) |
| `~MaxUserCount`        | Max users supported |
| `~MaxUserFingerCount`  | Max fingerprint templates |
| `~MaxAttLogCount`      | Max stored transaction records |
| `~CardFormatFunOn`     | Card-format function enabled (affects Wiegand/facility handling, §4) |
| `~Ext485ReaderFunOn`   | External RS485 reader function enabled |
| `MachineType`          | Device type code |
| `FirmVer`              | Firmware version string |
| `DeviceID`             | Device ID (1..255) for RS485 bus |

**Network / general:** **[CONFIRMED names]**

| Key | Meaning |
|-----|---------|
| `IPAddress`     | Panel IPv4 |
| `NetMask`       | Subnet mask |
| `GATEIPAddress` | Default gateway  (**[LIKELY]** exact spelling) |
| `RS232BaudRate` | Serial baud |
| `WatchDog`      | Watchdog enable (0/1) |
| `DateTime`      | RTC as packed seconds (§7) — also settable via cmd `0x03` |
| `LockCount`     | Number of lock relays (1/2/4 → C3-100/200/400) |
| `ReaderCount`   | Number of readers |
| `AuxInCount`    | Number of auxiliary inputs |
| `AuxOutCount`   | Number of auxiliary outputs |
| `AntiPassback`  | Global anti-passback rule bitmask |
| `InterLock`     | Interlock configuration |
| `Door4ToDoor2`  | C3-400→2-door mode flag |
| `BackupTime`    | Daily backup hour |
| `DaylightSavingTimeOn`, `DLSTMode`, `~DSTF` | DST enable / mode / display **[LIKELY names]** |
| `InBIOMode` / `InBIOTowWay` | inBio / two-way reader mode **[UNVERIFIED — most likely one of these spellings]** |

**Per-door parameters** (replace `N` with door number 1..4): **[CONFIRMED pattern; spellings LIKELY]**

| Key | Meaning |
|-----|---------|
| `DoorNDrivertime`        | **Lock relay drive / open duration, seconds** (0..254; 255=stay) |
| `DoorNDetectortime`      | Door-sensor / magnet alarm delay before "door open too long" (seconds) |
| `DoorNSensorType`        | 0 = none, 1 = normally-open, 2 = normally-closed (see `SensorType`, §6.6) |
| `DoorNCloseAndLock`      | Lock immediately when door closes (0/1) |
| `DoorNVerifyType`        | Verify mode for the door (see `VerifyMode`, §6.5) |
| `DoorNValidTZ`           | Active/valid timezone id |
| `DoorNKeepOpenTimeZone`  | Normal-open timezone id |
| `DoorNCancelKeepOpenDay` | Cancel intraday normal-open |
| `DoorNForcePassWord`     | Duress password |
| `DoorNSupperPassWord`    | Emergency/super password |
| `DoorNFirstCardOpenDoor` | First-card-open enable |
| `DoorNMultiCardOpenDoor` | Multi-card-open enable |
| `DoorNIntervalTime`      | Min punch interval (seconds) **[LIKELY name]** |

> The pyzkaccess high-level names map onto the above device keys, e.g. `lock_driver_time`→`DoorNDrivertime`,
> `sensor_type`→`DoorNSensorType`, `verify_mode`→`DoorNVerifyType`, `active_time_tz`→`DoorNValidTZ`,
> `magnet_alarm_duration`→`DoorNDetectortime`. Exact spelling of each key should be confirmed by dumping
> all params from the live panel (send a `0x04` GET with the names you expect and see which echo back).

---

## 4. Data tables (users, cards, authorizations) — read (`0x08`) / write (`0x06` config + write id)

### 4.1 Record wire format

Data-table records are **ASCII text**, not packed binary. **[CONFIRMED]** In the PULL SDK:

- **Fields within a record** are separated by **TAB (`0x09`)**, each field is `FieldName=Value`.
- **Records** are separated by **CR/LF (`\r\n`)** (some builds use `\n`). **[LIKELY]**

Example one user record (as it appears in the buffer):

```
Pin=1001\tCardNo=13375873\tPassword=\tGroup=1\tStartTime=0\tEndTime=0\tSuperAuthorize=0\r\n
```

**READ flow:** optionally send `0x06` (GET DATATABLE CONFIG) to learn field order for a `tablename`,
then `0x08` (READ) with a request specifying the table and filter; the response Data field is the text
block of records above. The `0x08` request payload format (table name + field list + filter) is
**[UNVERIFIED — most likely: `tablename\r\n<field,field,...>\r\n<filter>`]**; confirm from a capture.

### 4.2 `user` table (table name `"user"`) **[CONFIRMED fields]**

| Field key        | Type      | Meaning |
|------------------|-----------|---------|
| `Pin`            | int/str   | User id / enrollment number (primary key), 1..N |
| `CardNo`         | int (dec) | Card number as a **decimal string** (see §4.5 for Wiegand normalization) |
| `Password`       | str       | Optional door password |
| `Group`          | int       | Access group |
| `StartTime`      | date/int  | Validity start (0 = no limit) |
| `EndTime`        | date/int  | Validity end (0 = no limit) |
| `SuperAuthorize` | 0/1       | Super-user (bypasses timezones/anti-passback) |

### 4.3 `userauthorize` table (table name `"userauthorize"`) **[CONFIRMED fields]**

| Field key             | Type | Meaning |
|-----------------------|------|---------|
| `Pin`                 | int  | User id (matches `user.Pin`) |
| `AuthorizeTimezoneId` | int  | Timezone granting access (1 = always/24h by convention) |
| `AuthorizeDoorId`     | int  | **Door bitmask**: door1=1, door2=2, door3=4, door4=8 (all four = 15) |

### 4.4 Other tables (field → wire key) **[CONFIRMED from pyzkaccess]**

- **`timezone`** (`"timezone"`): `TimezoneId`, plus 3 time segments per day for Sun..Sat and Hol1..Hol3.
  Each segment is `HHMM`-style start/end. A "24h open" timezone is typically id 1 with every segment
  `0000`–`2359`.
- **`holiday`** (`"holiday"`): `Holiday` (date), `HolidayType` (1..3), `Loop` (0 unknown / 1 annual /
  2 not-annual — `HolidayLoop`).
- **`firstcard`** (`"firstcard"`): `DoorID`, `Pin`, `TimezoneID`.
- **`multimcard`** (`"multimcard"`): `Index`, `DoorId`, `Group1..Group5`.
- **`inoutfun`** (`"inoutfun"`, input/output linkage): `Index`, `EventType`, `InAddr`, `OutType`
  (`InOutFunRelayGroup`: 0 lock / 1 aux), `OutAddr`, `OutTime`, `Reserved`.
- **`transaction`** (`"transaction"`, stored access history): `Cardno`, `Pin`, `Verified`
  (`VerifyMode`), `DoorID`, `EventType`, `InOutState` (`PassageDirection`), `Time_second`
  (packed time §7). **This is the on-disk twin of the RTLog record (§6).**
- **`templatev10`** (`"templatev10"`, fingerprint templates): `Size`, `UID`, `Pin`, `FingerID`,
  `Valid`, `Template`, `Resverd`, `EndTag`.

### 4.5 Wiegand card-number normalization

The `CardNo`/`Cardno` field is the **decimal integer** the panel stores. How a physical Wiegand card
maps to that integer depends on the card format and `~CardFormatFunOn`. **[LIKELY]**

- **Wiegand 26-bit**: 26 bits = `[P] [8-bit Facility] [16-bit Card ID] [P]` (1 leading + 1 trailing
  parity). The 24 data bits = `Facility(8) | CardID(16)`.
  - If facility is **combined**: `CardNo = Facility * 65536 + CardID` (the full 24-bit value, 0..16777215).
  - If facility is **ignored** (`~CardFormatFunOn` off / "card number only"): `CardNo = CardID` (0..65535).
  - Example: facility `204`, card `1234` → combined `204*65536 + 1234 = 13,369,618`.
- **Wiegand 34-bit**: `[P] [32-bit Card ID] [P]`; `CardNo = 32-bit value` (0..4294967295). Facility
  handling varies by site format.
- **Normalization rule for the middleware**: store and compare cards as the **exact decimal that the
  panel emits in an RTLog swipe** (§6). The safest approach is: enroll a physical card, read the RTLog
  swipe, and use that decimal as the canonical `CardNo`. Do not assume a formula — the panel's Wiegand
  format setting decides it. The RTLog `CardNo` field is a **32-bit little-endian unsigned int** (§6.3),
  so the full 34-bit-derived numbers fit.

### 4.6 Authorizing a card to doors/timezones

1. Write a `user` record: `Pin`, `CardNo` (+ optional validity window / password).
2. Write a `userauthorize` record: same `Pin`, `AuthorizeTimezoneId` (1 for always), `AuthorizeDoorId`
   bitmask of the permitted doors.

Both writes use the SetDeviceData write command (§2 caveat) with the text record format of §4.1.

---

## 5. Door & relay control — `ControlDevice` (cmd `0x05`)

The Data payload after the (optional) session header is a **5-byte control block**: **[CONFIRMED shape]**

```
 Offset  Size  Field       Meaning
   0      1    Operation   1=OUTPUT, 2=CANCEL_ALARM, 3=RESTART, 4=NORMAL_OPEN_STATE_ENABLE
   1      1    Param1      operation-specific
   2      1    Param2      operation-specific
   3      1    Param3      operation-specific
   4      1    Param4      reserved (0)
```

`ControlOperation` enum: `output=1`, `cancel_alarm=2`, `restart=3` **[CONFIRMED]**;
`normal_open_state_enable=4` **[LIKELY]**.

### 5.1 Open a door / pulse a lock relay (OUTPUT)

`Operation=1 (OUTPUT)`, then: **[CONFIRMED semantics]**

| Param  | Meaning |
|--------|---------|
| Param1 | **Output number** = the door/relay index, 1..4 (LOCK1..LOCK4 → door 1..4; also AUX index when addressing aux group) |
| Param2 | **Address / relay group**: `1` = door lock relay group, `2` = auxiliary relay group |
| Param3 | **Duration** in seconds: `0` = close/off, `1..254` = pulse N seconds, `255` = open indefinitely |
| Param4 | `0` (reserved) |

`RelayGroup` enum: **lock = 1, aux = 2** (this is the *address* value). **[CONFIRMED]**
(Note: the separate `InOutFunRelayGroup` used in the `inoutfun` table is 0=lock/1=aux — different enum.)

**Worked example — open door 1 (LOCK1) for 5 seconds**, session mode:

```
control block = 01 01 01 05 00
                 |  |  |  |  +-- Param4 reserved
                 |  |  |  +----- Param3 duration = 5 s
                 |  |  +-------- Param2 address = 1 (lock relay group)
                 |  +----------- Param1 output number = door 1
                 +-------------- Operation = 1 (OUTPUT)

Data = <SessionId LE> <MessageNr LE> 01 01 01 05 00      (Length = 9)
Frame = AA 01 05 09 00  SS SS NN NN 01 01 01 05 00  CC CC  55
                                                    ^^^^^  CRC-16 (§1.3), little-endian
```

Simple (non-session) form: `Data = 01 01 01 05 00`, `Length=5`:
`AA 01 05 05 00 01 01 01 05 00 CC CC 55`.

- **Pulse AUX output 1 for 10 s**: `01 01 02 0A 00` (address=2 aux, duration=10).
- **Drive LOCK2 (door 2) for 5 s**: `01 02 01 05 00`.
- **Latch a door open until told otherwise**: duration `255` → `01 0N 01 FF 00`; close with
  duration `0` → `01 0N 01 00 00`. Alternatively use NORMAL_OPEN_STATE_ENABLE (§5.4).

### 5.2 Cancel alarm

`Operation=2`, params `0`. Payload `02 00 00 00 00`. Clears a triggered alarm (tamper, door-forced,
door-open-too-long). **[CONFIRMED]**

### 5.3 Restart panel

`Operation=3`, params `0`. Payload `03 00 00 00 00`. Reboots the controller. **[CONFIRMED]**

### 5.4 Normal-open state enable/disable

`Operation=4`; `Param1 = door number (1..4)`, `Param2 = enable (1) / disable (0)`. Puts a door into
(or out of) held-open state. **[LIKELY]**

### 5.5 Reading door / input / relay state

There is no dedicated "read IO" command; door-sensor state, alarm state and relay state are reported
**inside the RTLog** as door/alarm **status records** (§6.4) and via door-status event types (§6.5).
Poll RTLog and track the latest status record per door. Static config (sensor type, lock time) is read
via `GetDeviceParam` (§3). **[LIKELY]**

---

## 6. Realtime events — RTLog  (MOST IMPORTANT)

RTLog is a ring buffer of the most recent events, **max ~30 records held in RAM**. **[CONFIRMED]**
Read it with **command `0x0B`** (binary records) — the panel returns 0..N back-to-back **16-byte**
records in the response Data field (after the session header). Empty/`0` records = nothing new.
Command `0x79` returns the same information as **text `key=value` records** (alt encoding). **[CONFIRMED]**

### 6.1 Request

Send `AA 01 0B <len> <session-header> <crc> 55`. **[CONFIRMED cmd]** The panel replies `0xC8` with the
Data field = session header (if session mode) followed by `k × 16` bytes of records. Parse the payload
in 16-byte strides.

### 6.2 Two record kinds, both 16 bytes

The RTLog interleaves two 16-byte record types: **[CONFIRMED there are two kinds; discriminator LIKELY]**

1. **Event record** (a card swipe / access decision / door event) — §6.3.
2. **Door & alarm status record** (periodic door-sensor / alarm / relay snapshot) — §6.4.

**Discriminator: [UNVERIFIED — most likely: `EventType == 0xFF (255)` marks a status record]**; an
event record has `EventType` in the documented event range (§6.5). Cross-check by the fact that a
status record has `CardNo == 0` and carries per-door bitfields rather than a card. Validate on the
live panel by swiping a card (event) vs. opening a door by hand (status).

### 6.3 Event record layout (16 bytes) **[CONFIRMED field set & order; widths LIKELY]**

Mirrors the `transaction` table (`Cardno, Pin, Verified, DoorID, EventType, InOutState, Time_second`):

```
 Offset  Size  Field         Type/Encoding
 ------  ----  -----------   ---------------------------------------------
   0      4    CardNo        uint32 little-endian  (decimal card number, §4.5)
   4      4    Pin           uint32 little-endian  (user id; 0 if unknown card)
   8      1    Verified      uint8   (VerifyMode, §6.5)
   9      1    DoorID        uint8   (door / reader number, 1..4)
  10      1    EventType     uint8   (event code, §6.5)
  11      1    InOutState    uint8   (PassageDirection: 0 in / 1 out / 2 none, §6.5)
  12      4    Time_second   uint32 little-endian  (packed time, §7)
 ------  ----
        16 bytes total
```

> Field **order** (CardNo, Pin, Verified, DoorID, EventType, InOutState, Time) is **[CONFIRMED]** from
> the `transaction` table model. The exact **widths** (CardNo=4, Pin=4, three 1-byte enum fields,
> Time=4 = 16 total) are the standard PULL SDK RTLog layout **[LIKELY]** — validate the first swipe.

**Worked decode of a card-swipe (hypothetical, to show the method):**

```
raw = C1 05 CC 00 | E9 03 00 00 | 04 | 01 | 00 | 00 | 20 5A 8C 0F
       CardNo(4)     Pin(4)        Vf  Dr  Et  IO   Time_second(4)

CardNo      = 0x00CC05C1 LE = 13,371,841
Pin         = 0x000003E9 LE = 1001
Verified    = 0x04       = only_card
DoorID      = 0x01       = door 1 / reader 1
EventType   = 0x00       = Normal Punch Open (access granted)
InOutState  = 0x00       = entry (in)
Time_second = 0x0F8C5A20 LE = 260,712,992  -> decode via §7
```

(The byte values above are illustrative; capture a real swipe to lock the layout.)

### 6.4 Door & alarm status record (16 bytes) **[UNVERIFIED — most likely]**

When `EventType == 0xFF`, the 16 bytes encode per-door **sensor status**, **door status**, **alarm
status**, **relay status** as bitfields (one nibble/bit group per door for up to 4 doors), plus a
`Time_second`. The precise byte assignment is **[UNVERIFIED]**; most-likely layout:

```
 Offset  Size  Field           Note
   0      4    (status flags)   packed door-sensor / door-open bits (per door)
   4      4    (alarm flags)    packed alarm bits (per door / aux)
   8      1    Verified         0xFF sentinel
   9      1    DoorID/Index     0 or door index
  10      1    EventType        0xFF (status marker)
  11      1    InOutState       0
  12      4    Time_second      packed time (§7)
```

Do not rely on this until confirmed by capturing status records from the live panel.

### 6.5 Enum values

**`VerifyMode`** (the `Verified` byte): **[CONFIRMED]**

| Value | Meaning |
|-------|---------|
| 0   | not available / none |
| 1   | fingerprint only |
| 3   | password only |
| 4   | card only |
| 6   | card **or** fingerprint |
| 10  | card **and** fingerprint |
| 11  | card **and** password |
| 200 | others |

**`InOutState` / `PassageDirection`** (the `InOutState` byte): **[CONFIRMED]**

| Value | Meaning |
|-------|---------|
| 0 | entry (in) |
| 1 | exit (out) |
| 2 | none / not applicable |

**`EventType`** (the `EventType` byte). Standard ZKAccess event codes — **[LIKELY]** for the full list
(common codes high-confidence, rarer codes should be confirmed):

Access / decision events (card & fingerprint):

| Code | Meaning |
|------|---------|
| 0  | Normal Punch Open (access granted) |
| 1  | Punch during Normal-Open time zone |
| 2  | First-Card Normal Open (punch) |
| 3  | Multi-Card Open (punch) |
| 4  | Emergency Password Open |
| 5  | Open during Normal-Open time zone |
| 6  | Linkage event triggered |
| 7  | Cancel alarm |
| 8  | Remote Opening |
| 9  | Remote Closing |
| 10 | Disable intraday Normal-Open time zone |
| 11 | Enable intraday Normal-Open time zone |
| 12 | Open Auxiliary output |
| 13 | Close Auxiliary output |
| 14 | Press Fingerprint Open |
| 15 | Multi-Card Open (fingerprint) |
| 16 | Press Fingerprint during Normal-Open time zone |
| 17 | Card plus Fingerprint Open |
| 18 | First-Card Normal Open (fingerprint) |
| 19 | First-Card Normal Open (card + fingerprint) |
| 20 | Too-short punch interval |
| 21 | Door inactive time zone (punch) |
| 22 | Illegal time zone |
| 23 | Access Denied (invalid access) |
| 24 | Anti-Passback |
| 25 | Interlock |
| 26 | Multi-Card authentication (punch, waiting) |
| 27 | Unregistered card |
| 28 | Opening timeout |
| 29 | Card expired |
| 30 | Password error |
| 31 | Too-short fingerprint press interval |
| 32 | Multi-Card authentication (fingerprint, waiting) |
| 33 | Fingerprint expired |
| 34 | Unregistered fingerprint |
| 35 | Door inactive time zone (fingerprint) |
| 36 | Door inactive time zone (exit button) |
| 37 | Failed to close during Normal-Open time zone |

Door / sensor / alarm status events (higher range) — **[LIKELY]**:

| Code | Meaning |
|------|---------|
| 101 | Duress Password Open |
| 102 | Opened Accidentally (door forced) |
| 103 | Duress Fingerprint Open |
| 200 | Door opened correctly |
| 201 | Door closed correctly |
| 202 | Exit button open |
| 203 | Multi-Card Open (card + fingerprint) |
| 204 | Normal-Open time zone over |
| 205 | Remote Normal Opening |
| 206 | Device started up |
| 207 | Password open |
| 208 | Auxiliary input disconnected |
| 209 | Auxiliary input shorted |
| 220 | Auxiliary input point event |
| 255 | **Status-record marker / current door & alarm status** (see §6.4) |

> The 0–37 access codes and 200-range door codes are consistent across ZKTeco access docs (high
> confidence). Codes 101/102/103, 204–209, 220, and the exact 255 semantics should be confirmed
> against the live panel — trigger each condition and read the RTLog.

### 6.6 `SensorType` (door sensor config, §3 door params, and status interpretation) **[CONFIRMED]**

| Value | Meaning |
|-------|---------|
| 0 | not available (no sensor) |
| 1 | normally open |
| 2 | normally closed |

---

## 7. Time encoding (packed seconds-since-2000)

The panel stores/transmits date-times as a **single 32-bit "encoded seconds" value** (used by RTLog
`Time_second`, the `transaction` table, the `DateTime` param, and SET DATETIME cmd `0x03`).
**[LIKELY — this is the documented ZKTeco access-control time format]**

**Encode:**

```
value = ((year - 2000) * 12 * 31 + (month - 1) * 31 + (day - 1)) * 86400
        + hour * 3600 + minute * 60 + second
```

**Decode:**

```
second  =  value        % 60
minute  = (value / 60)  % 60
hour    = (value / 3600) % 24
day     = (value / 86400)                % 31 + 1
month   = (value / (86400 * 31))         % 12 + 1
year    = (value / (86400 * 31 * 12))         + 2000
```

Notes:
- Every month is treated as 31 "day slots" and every year as 12 months — this is a **calendar-index
  packing, not real elapsed seconds**. Do not use Unix epoch math.
- On the wire it is **little-endian uint32** (as in the RTLog `Time_second` field).
- **SET DATETIME (cmd `0x03`)** payload = this 4-byte encoded value (after the session header).
  **[LIKELY]** Alternatively set the `DateTime` parameter via `0x04`.

**Worked example — 2021-06-15 08:30:00:**

```
(21*372 + 5*31 + 14) = 7812 + 155 + 14 = 7981 day-slots
7981 * 86400 = 689,558,400
+ 8*3600 + 30*60 + 0 = 28,800 + 1,800 = 30,600
value = 689,589,000  = 0x291B_C688
LE bytes on wire = 88 C6 1B 29
```

> Some very old ZK access firmwares used a BCD scheme instead; the C3 PULL SDK uses the packed-seconds
> form above. Confirm by reading the panel's `DateTime` param and comparing to the wall clock.

---

## 8. Timezones, holidays, access levels (summary)

- **Timezone** (`timezone` table): id + 3 daily intervals for each weekday and 3 holiday types. A
  "24-hour always" zone is conventionally **id 1**. To grant unrestricted access, authorize the user
  to timezone 1.
- **Holiday** (`holiday` table): a date + `HolidayType` (1..3) + `Loop` (annual or not). During a
  holiday, the matching `HolidayN` timezone segment applies instead of the weekday segment.
- **Access level / authorization** is per-user via `userauthorize` (Pin + timezone + door bitmask,
  §4.3). There is no separate "AccessLevel" table on the C3 PULL SDK — the (user, timezone, doors)
  triple IS the access level. `FirstCard` and `MultiCard` tables add first-card-open and
  multi-card-open rules.

---

## 9. Realtime monitoring — mechanics

- **Poll model (only supported model):** after connecting, loop: send `0x0B`, parse 16-byte records,
  sleep a short interval (e.g. 200–1000 ms), repeat. The RAM buffer holds ~30 records; poll faster
  than the busiest expected swipe rate to avoid overrun. **[CONFIRMED]**
- **No async subscribe/push.** The PULL SDK "realtime" is client polling; the "PULL" name refers to
  the panel being polled/pulled from. **[CONFIRMED]**
- **Deduplication / cursor:** the RTLog returns *new* records since last read; once read they are
  consumed from the buffer. Keep the session alive; if you reconnect you may re-read whatever is still
  buffered. Use `(Time_second, CardNo, DoorID, EventType)` as a dedupe key if needed.
- **Text variant (`0x79`):** same events as `key=value` text records; useful if binary parsing of the
  status-record kind proves ambiguous.

---

## 10. Worked frame walkthroughs

**A. Session connect:**
```
Req:  AA 01 76 <len> 00 00 FE FE [pwd?] CC CC 55     (SessionId=0, MsgNr=0xFEFE)
Resp: AA 01 C8 <len> <SessionId LE> <MsgNr LE> ...  CC CC 55
      -> store SessionId; set MsgNr = returned+1 (validate increment behaviour)
```

**B. Open door 1 for 5 s** (§5.1): `AA 01 05 09 00  SS SS NN NN 01 01 01 05 00  CC CC 55`.

**C. Poll RTLog:** `AA 01 0B <len> SS SS NN NN CC CC 55` → response Data = `SS SS NN NN` + k×16 bytes.

**D. Set clock to 2021-06-15 08:30:00** (§7): payload `88 C6 1B 29` →
`AA 01 03 <len> SS SS NN NN 88 C6 1B 29 CC CC 55`.

(`CC CC` = CRC-16 low/high per §1.3; `SS SS`/`NN NN` = session id / message nr.)

---

## 11. Implementation checklist (capability → commands)

| Middleware capability      | Commands / sequence |
|----------------------------|---------------------|
| **Connect**                | `0x76` (session) → store SessionId, init MsgNr=0xFEFE |
| **Disconnect**             | `0x02` |
| **Sync / set time**        | `0x03` with packed-seconds payload (§7), or `0x04` SET `DateTime=` |
| **Read time / serial / params** | `0x04` GET with `~SerialNumber,DateTime,IPAddress,LockCount,…` |
| **Set params (door times, sensor, mode)** | `0x04` SET `key=value,…` (§2 caveat) — e.g. `Door1Drivertime=5` |
| **Enroll card (add user + card)** | write `user` record (`Pin`,`CardNo`) via SetDeviceData; then write `userauthorize` (`Pin`,`AuthorizeTimezoneId=1`,`AuthorizeDoorId=<mask>`) (§4, §2 write-id caveat) |
| **Revoke card**            | DeleteDeviceData on `user`/`userauthorize` by `Pin` (delete id §2), or overwrite validity window to expired |
| **Open door / pulse lock** | `0x05` OUTPUT `01 <door> 01 <sec> 00` (§5.1) |
| **Drive AUX output**       | `0x05` OUTPUT `01 <aux> 02 <sec> 00` |
| **Hold door open / release** | `0x05` OUTPUT dur `255`/`0`, or NORMAL_OPEN_ENABLE `04 <door> <0/1> 00 00` |
| **Cancel alarm**           | `0x05` `02 00 00 00 00` |
| **Restart panel**          | `0x05` `03 00 00 00 00` |
| **List / stream events**   | poll `0x0B`, parse 16-byte RTLog records (§6); or `0x79` text |
| **Read stored history**    | `0x06`+`0x08` on `transaction` table |
| **Read door/input state**  | derive from RTLog status records (§6.4) + door params (§3) |

---

## 12. Go type sketches (drop-in starting point)

```go
const (
    Start = 0xAA
    Ver   = 0x01
    End   = 0x55

    CmdConnect        = 0x01
    CmdDisconnect     = 0x02
    CmdSetDateTime    = 0x03
    CmdGetParam       = 0x04
    CmdControl        = 0x05
    CmdGetTableCfg    = 0x06
    CmdReadTable      = 0x08
    CmdGetRTLog       = 0x0B
    CmdConnectSession = 0x76
    CmdRTLogText      = 0x79
    CmdReplyOK        = 0xC8
)

// RTLog event record (16 bytes, little-endian).
type RTEvent struct {
    CardNo     uint32
    Pin        uint32
    Verified   uint8
    DoorID     uint8
    EventType  uint8
    InOutState uint8
    TimeSecond uint32
}

func parseRTEvent(b []byte) RTEvent { // len(b)==16
    return RTEvent{
        CardNo:     uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24,
        Pin:        uint32(b[4]) | uint32(b[5])<<8 | uint32(b[6])<<16 | uint32(b[7])<<24,
        Verified:   b[8], DoorID: b[9], EventType: b[10], InOutState: b[11],
        TimeSecond: uint32(b[12]) | uint32(b[13])<<8 | uint32(b[14])<<16 | uint32(b[15])<<24,
    }
}

func decodeTime(v uint32) (y, mo, d, h, mi, s int) {
    s = int(v % 60); v /= 60
    mi = int(v % 60); v /= 60
    h = int(v % 24);  v /= 24
    d = int(v%31) + 1; v /= 31
    mo = int(v%12) + 1; v /= 12
    y = int(v) + 2000
    return
}

func buildFrame(cmd byte, data []byte) []byte {
    n := len(data)
    f := make([]byte, 0, n+8)
    f = append(f, Start, Ver, cmd, byte(n), byte(n>>8))
    f = append(f, data...)
    c := crc16(f[1:])          // Version..last data byte
    f = append(f, byte(c), byte(c>>8), End)
    return f
}
```

---

## 13. Confidence map (what is solid vs. must be validated on the live panel)

**Solid / high confidence — implement directly:**
- Frame layout: `0xAA 0x01 Cmd Len(LE2) Data CRC(LE2) 0x55`; `Length` = data length. **[CONFIRMED]**
- Command IDs `0x01,0x02,0x03,0x04,0x05,0x06,0x08,0x0B,0x14,0x76,0x79,0xC8`. **[CONFIRMED]**
- Checksum is a CRC-16 over `Version..last-data-byte`. **[CONFIRMED it is CRC-16 over that span]**
- Session header = SessionId(LE2)+MessageNr(LE2) at start of Data in session mode. **[CONFIRMED]**
- ControlDevice 5-byte block; OUTPUT = `01 <door> <1=lock|2=aux> <sec> 00`; cancel-alarm/restart.
  **[CONFIRMED]**
- RTLog event record fields & order (CardNo,Pin,Verified,DoorID,EventType,InOutState,Time). **[CONFIRMED]**
- `VerifyMode` and `InOutState` enum values. **[CONFIRMED]**
- Data-table field names for `user`, `userauthorize`, `transaction`, `timezone`, `holiday`,
  `firstcard`, `multimcard`, `inoutfun`, `templatev10`; TAB-delimited `Name=Value` records. **[CONFIRMED]**
- Device-parameter key names (system/network/door). **[CONFIRMED names]**

**Must validate empirically before shipping:**
1. **Exact CRC-16 variant** (poly/init). Use §1.3 Kermit first; fall back to additive. — *highest priority*
2. **RTLog event field widths** (CardNo=4, Pin=4, Time=4 → 16 total) — confirm with a real swipe.
3. **Status-record layout & the `EventType==255` discriminator** (§6.4).
4. **MessageNr initial value `0xFEFE` / increment / echo** semantics (§1.4).
5. **SET-PARAM command id** (probably reuse of `0x04`) and **param-list separator** (comma vs tab) (§2/§3).
6. **SetDeviceData / DeleteDeviceData command ids** for table writes (card enroll/revoke) (§2, §4).
7. **`0x08` read-request payload format** (table + fields + filter) (§4.1).
8. **SET DATETIME (`0x03`) payload = packed-seconds** vs. a struct (§7).
9. **Time packing formula** (calendar-index seconds-since-2000) — read `DateTime` and compare to clock.
10. **Wiegand→CardNo normalization** — enroll a card, read its RTLog decimal, adopt as canonical (§4.5).
11. **Full EventType table beyond the common codes** — trigger conditions and read RTLog (§6.5).
12. **Door-param exact key spellings** (`DoorNDrivertime`, `GATEIPAddress`, `InBIOMode`, etc.) — dump params.

**Sources:** vwout/zkaccess-c3-py (readme + `c3/` sources), bdragon300/pyzkaccess docs
(enums/tables/params), ZKTeco Access PULL SDK (`plcommpro.dll`) documentation and community mirrors.
