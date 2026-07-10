#!/usr/bin/env python3
"""Read C3 data tables using the binary GETDATA (0x08) protocol.

Request payload: [table_index, num_fields, *field_indexes, 0, 0]
Response: [table_index, field_count, *field_indexes, <records>]
where each record field is [size, value_bytes] (int LE for type 'i'/'L', ascii for 's').
"""
import socket, sys

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
TABLES = sys.argv[2:] or ["transaction", "user", "timezone", "userauthorize"]
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CONNECT, DISCONNECT, DATACFG, GETDATA = 0x01, 0x02, 0x06, 0x08


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
    length = hdr[3] | (hdr[4] << 8)
    rest = recvn(s, length + 3)
    return hdr[2], rest[:length]


def send_recv(s, cmd, payload=b""):
    s.send(build(cmd, payload))
    return recv_frame(s)


def parse_cfg(msg):
    """Return {name: (index, [(fname, type, findex), ...])}."""
    cfg = {}
    for line in msg.split(b"\x0a"):
        line = line.decode("ascii", "ignore").strip()
        if not line:
            continue
        parts = line.split(",")
        name, tidx = parts[0].split("=")
        fields = []
        for p in parts[1:]:
            fn, spec = p.split("=")
            fields.append((fn, spec[0], int(spec[1:])))
        cfg[name] = (int(tidx), fields)
    return cfg


def read_table(s, cfg, name):
    tidx, fields = cfg[name]
    findex = sorted(f[2] for f in fields)
    payload = [tidx, len(findex)] + findex + [0, 0]
    _, msg = send_recv(s, GETDATA, payload)
    if not msg or msg[0] != tidx:
        return []
    fcnt = msg[1]
    resp_idx = list(msg[2:2 + fcnt])
    data = msg[2 + fcnt:]
    typ = {f[2]: f[1] for f in fields}
    nm = {f[2]: f[0] for f in fields}
    records = []
    while data:
        rec = {}
        ok = True
        for fi in resp_idx:
            if not data:
                ok = False
                break
            size = data[0]
            val = data[1:1 + size]
            if typ.get(fi) in ("i", "L"):
                rec[nm.get(fi, fi)] = int.from_bytes(val, "little")
            else:
                rec[nm.get(fi, fi)] = val.decode("ascii", "ignore")
            data = data[1 + size:]
        if not ok or not rec:
            break
        records.append(rec)
    return records


def main():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    cmd, _ = send_recv(s, CONNECT)
    print("connect:", "OK" if cmd == 0xC8 else "ERR")
    _, cfgmsg = send_recv(s, DATACFG)
    cfg = parse_cfg(cfgmsg)
    print("tables:", ", ".join(f"{n}({i[0]})" for n, i in cfg.items()))
    for t in TABLES:
        if t not in cfg:
            print(f"\n[{t}] not a table"); continue
        recs = read_table(s, cfg, t)
        print(f"\n[{t}] {len(recs)} record(s):")
        for r in recs[:15]:
            print("  ", r)
        if len(recs) > 15:
            print(f"   ... +{len(recs)-15} more")
    send_recv(s, DISCONNECT)
    s.close()


if __name__ == "__main__":
    main()
