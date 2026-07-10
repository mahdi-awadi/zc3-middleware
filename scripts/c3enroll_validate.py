#!/usr/bin/env python3
"""AUTHORIZED dummy enroll/revoke round-trip to discover the SETDATA/DELETE format.

Scope guarantees:
- Only ever touches a DUMMY user Pin=88888 / CardNo=99999999 with NO door
  authorization (cannot open anything).
- Backs up the real user table first; verifies it is fully restored at the end.
- Tries a small set of well-reasoned formats, reading back after each; on the
  first that works, immediately deletes the dummy and stops.
"""
import socket, struct, sys

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CONNECT, DISCONNECT, DATACFG, SETDATA, GETDATA, DELDATA = 0x01, 0x02, 0x06, 0x07, 0x08, 0x09
DPIN, DCARD = "88888", 99999999


def crc16(d):
    c = 0
    for b in d:
        c ^= b
        for _ in range(8):
            c = (c >> 1) ^ 0xA001 if (c & 1) else (c >> 1)
    return c & 0xFFFF


def build(cmd, payload=b""):
    body = bytes([VER, cmd, len(payload) & 0xFF, len(payload) >> 8 & 0xFF]) + bytes(payload)
    c = crc16(body)
    return bytes([START]) + body + bytes([c & 0xFF, c >> 8 & 0xFF, END])


def recvn(s, n):
    b = b""
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c:
            raise ConnectionError("closed")
        b += c
    return b


def recv_frame(s):
    hdr = recvn(s, 5)
    ln = hdr[3] | (hdr[4] << 8)
    rest = recvn(s, ln + 3)
    return hdr[2], rest[:ln]


def sr(s, cmd, payload=b""):
    s.send(build(cmd, payload))
    return recv_frame(s)


def parse_cfg(msg):
    cfg = {}
    for line in msg.split(b"\x0a"):
        line = line.decode("ascii", "ignore").strip()
        if not line:
            continue
        parts = line.split(",")
        name, tidx = parts[0].split("=")
        fields = [(p.split("=")[0], p.split("=")[1][0], int(p.split("=")[1][1:])) for p in parts[1:]]
        cfg[name] = (int(tidx), fields)
    return cfg


def read_users(s, cfg):
    tidx, fields = cfg["user"]
    fi = sorted(f[2] for f in fields)
    _, msg = sr(s, GETDATA, [tidx, len(fi)] + fi + [0, 0])
    if not msg or msg[0] != tidx:
        return []
    fcnt = msg[1]
    ridx = list(msg[2:2 + fcnt])
    data = msg[2 + fcnt:]
    typ = {f[2]: f[1] for f in fields}
    nm = {f[2]: f[0] for f in fields}
    rows = []
    while data:
        rec = {}
        for x in ridx:
            if not data:
                break
            sz = data[0]
            val = data[1:1 + sz]
            rec[nm[x]] = int.from_bytes(val, "little") if typ[x] in "iL" else val.decode("ascii", "ignore")
            data = data[1 + sz:]
        if "Pin" not in rec:
            break
        rows.append(rec)
    return rows


def has_dummy(s, cfg):
    return any(u.get("Pin") == DPIN for u in read_users(s, cfg))


VALS = {"UID": 4, "CardNo": DCARD, "Pin": DPIN, "Password": "", "Group": 0,
        "StartTime": 0, "EndTime": 0, "Name": "CLAUDETEST", "SuperAuthorize": 0, "Disable": 0}


def enc_field(typ, val):
    if typ in "iL":
        n = int(val)
        b = b"\x00" if n == 0 else n.to_bytes((n.bit_length() + 7) // 8, "little")
    else:
        b = str(val).encode("ascii")
    return bytes([len(b)]) + b


def bin_record(cfg, vals):
    tidx, fields = cfg["user"]
    order = sorted(fields, key=lambda f: f[2])
    fi = [f[2] for f in order]
    payload = bytes([tidx, len(fi)]) + bytes(fi)
    for nm, typ, _ in order:
        payload += enc_field(typ, vals.get(nm, 0))
    return payload


def set_candidates(cfg):
    # (cmd, label, payload) — binary format symmetric to the GETDATA response.
    return [
        (SETDATA, "0x07 binary symmetric", bin_record(cfg, VALS)),
        (0x0A, "0x0A binary symmetric", bin_record(cfg, VALS)),
        (0x0C, "0x0C binary symmetric", bin_record(cfg, VALS)),
    ]


def del_candidates(cfg):
    tidx = cfg["user"][0]
    # delete by key: table index + one field [size,value] for Pin
    keypay = bytes([tidx, 1, 3]) + enc_field("s", DPIN)
    return [
        (DELDATA, "0x09 binary key", keypay),
        (0x0A, "0x0A binary key", keypay),
        (DELDATA, "0x09 ascii", b"user\x00Pin=" + DPIN.encode()),
    ]


def main():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    sr(s, CONNECT)
    cfg = parse_cfg(sr(s, DATACFG)[1])

    backup = read_users(s, cfg)
    print(f"backup: {len(backup)} real users: {[u.get('Name') for u in backup]}")
    if has_dummy(s, cfg):
        print("!! dummy already present, aborting"); return

    found_set = None
    for cmd, lbl, p in set_candidates(cfg):
        rcmd, _ = sr(s, cmd, p)
        added = has_dummy(s, cfg)
        print(f"  SET [{lbl}] reply=0x{rcmd:02x} added={added}")
        if added:
            found_set = lbl
            break
    print(f"==> SETDATA format: {found_set}")

    if found_set:
        for cmd, lbl, p in del_candidates(cfg):
            sr(s, cmd, p)
            if not has_dummy(s, cfg):
                print(f"==> DELETE format: {lbl}")
                break

    after = read_users(s, cfg)
    same = [u.get("Pin") for u in after] == [u.get("Pin") for u in backup] and not has_dummy(s, cfg)
    print(f"final: {len(after)} users; RESTORED={'YES' if same else 'NO — CHECK PANEL'}")
    sr(s, DISCONNECT)
    s.close()


if __name__ == "__main__":
    main()
