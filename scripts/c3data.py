#!/usr/bin/env python3
"""Probe the C3 GETDATA (0x08) request format for reading data tables."""
import socket, sys

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT, CMD_DISCONNECT, CMD_GETDATA, CMD_DATACFG = 0x01, 0x02, 0x08, 0x06


def crc16(d):
    c = 0
    for b in d:
        c ^= b
        for _ in range(8):
            c = (c >> 1) ^ 0xA001 if (c & 1) else (c >> 1)
    return c & 0xFFFF


def build(cmd, payload=b""):
    body = bytes([VER, cmd, len(payload) & 0xFF, len(payload) >> 8 & 0xFF]) + payload
    c = crc16(body)
    return bytes([START]) + body + bytes([c & 0xFF, c >> 8 & 0xFF, END])


def recvn(s, n):
    buf = b""
    while len(buf) < n:
        c = s.recv(n - len(buf))
        if not c:
            raise ConnectionError("closed")
        buf += c
    return buf


def recv_frame(s):
    hdr = recvn(s, 5)
    length = hdr[3] | (hdr[4] << 8)
    rest = recvn(s, length + 3)
    return hdr[2], rest[:length]


def connect():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    s.send(build(CMD_CONNECT))
    cmd, _ = recv_frame(s)
    return s if cmd == 0xC8 else None


def try_payload(cmd, label, payload):
    try:
        s = connect()
        if not s:
            print(f"  [{label}] connect failed"); return
        s.send(build(cmd, payload))
        rcmd, data = recv_frame(s)
        status = "OK" if rcmd == 0xC8 else (f"ERR 0x{data[0]:02x}" if (rcmd == 0xC9 and data) else f"cmd 0x{rcmd:x}")
        show = data[:400]
        try:
            txt = show.decode("ascii", "replace")
        except Exception:
            txt = show.hex()
        print(f"  [{label}] -> {status}, {len(data)}B: {txt!r}")
        s.send(build(CMD_DISCONNECT)); s.close()
    except Exception as e:
        print(f"  [{label}] error: {e}")


def full_cfg():
    s = connect()
    s.send(build(CMD_DATACFG, b""))
    _, data = recv_frame(s)
    s.send(build(CMD_DISCONNECT)); s.close()
    return data.decode("ascii", "replace")


print("== FULL DATATABLE_CFG ==")
print(full_cfg())

def try_multi(cmd, label, payload):
    """Send a command and read ALL frames that arrive within the timeout."""
    try:
        s = connect()
        s.settimeout(2.0)
        s.send(build(cmd, payload))
        total = b""
        first = None
        try:
            while True:
                rcmd, data = recv_frame(s)
                if first is None:
                    first = rcmd
                total += data
                if len(total) > 2000:
                    break
        except (socket.timeout, ConnectionError):
            pass
        txt = total[:300].decode("ascii", "replace")
        print(f"  [{label}] first=0x{(first or 0):x} total={len(total)}B: {txt!r}")
        try:
            s.send(build(CMD_DISCONNECT)); s.close()
        except Exception:
            pass
    except Exception as e:
        print(f"  [{label}] error: {e}")


print("\n== GETDATA (0x08) transaction probes (multi-frame) ==")
for lbl, p in [
    ("name only", b"transaction"),
    ("name\\t*", b"transaction\t*"),
    ("name\\t*\\t\\t", b"transaction\t*\t\t"),
    ("name,*,,", b"transaction,*,,"),
    ("name CRLF *", b"transaction\r\n*\r\n\r\n"),
    ("name NUL *NULNULNUL", b"transaction\x00*\x00\x00\x00"),
    ("id 5", b"5"),
    ("fields explicit", b"transaction\tIndex,Pin,Cardno,DoorID,EventType,Time_second\t\t"),
    ("querystr", b"tablename=transaction,fielddesc=*,filter=,options="),
]:
    try_multi(CMD_GETDATA, lbl, p)
