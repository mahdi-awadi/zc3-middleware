#!/usr/bin/env python3
"""READ-ONLY probe for the C3 data-table protocol (cmd 0x06 config / 0x08 read).

Tries several candidate request payloads for reading the `user` table and prints
the panel's reply. Purely read-only: no writes/deletes are attempted here.

Usage: c3table.py [host]
"""
import socket, sys, time

HOST = sys.argv[1] if len(sys.argv) > 1 else "192.168.0.102"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT, CMD_DISCONNECT = 0x01, 0x02
CMD_CFG, CMD_READ = 0x06, 0x08


def crc16(data):
    crc = 0
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if (crc & 1) else (crc >> 1)
    return crc & 0xFFFF


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


def printable(b):
    return "".join(chr(x) if 32 <= x < 127 else ("\\t" if x == 9 else "\\r" if x == 13 else "\\n" if x == 10 else ".") for x in b)


def send_recv(s, cmd, payload, label):
    try:
        s.send(build(cmd, payload))
        rcmd, data = recv_frame(s)
        tag = "OK" if rcmd == 0xC8 else (f"ERR code={data[0] if data else '?'}" if rcmd == 0xC9 else f"cmd={rcmd:#x}")
        print(f"  [{label}] payload={payload!r}\n     -> {tag} len={len(data)}")
        if data:
            print(f"        ascii: {printable(data[:400])}")
            if len(data) > 400:
                print(f"        (+{len(data)-400} more bytes)")
        return rcmd, data
    except Exception as e:
        print(f"  [{label}] payload={payload!r} -> EXC {e}")
        # reconnect after an error/timeout so subsequent probes still run
        return None, None


def connect():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    s.send(build(CMD_CONNECT))
    cmd, _ = recv_frame(s)
    if cmd != 0xC8:
        raise SystemExit(f"connect failed cmd={cmd:#x}")
    return s


def main():
    print(f"== C3 data-table read probe against {HOST}:{PORT} (READ-ONLY) ==")

    cfg_variants = [b"user", b"user\x00", b"user\r\n", b"user\n"]
    read_variants = [
        b"user",
        b"user\r\n",
        b"user\r\n*\r\n",
        b"user\r\n*\r\n\r\n",
        b"user\r\nPin,CardNo,Password,Group,StartTime,EndTime,SuperAuthorize\r\n",
        b"user\tPin,CardNo",
        b"table=user",
        b"table=user\r\n",
        b"user\x00",
        b"user,*",
    ]

    print("\n--- cmd 0x06 DATATABLE_CFG (learn field layout) ---")
    for v in cfg_variants:
        s = connect()
        send_recv(s, CMD_CFG, v, "cfg")
        try:
            s.send(build(CMD_DISCONNECT)); s.close()
        except Exception:
            pass
        time.sleep(0.2)

    print("\n--- cmd 0x08 READ user table ---")
    for v in read_variants:
        s = connect()
        # some firmwares want a 0x06 cfg first; do it then read on same session
        send_recv(s, CMD_CFG, b"user", "cfg-pre")
        send_recv(s, CMD_READ, v, "read")
        try:
            s.send(build(CMD_DISCONNECT)); s.close()
        except Exception:
            pass
        time.sleep(0.2)

    print("\nprobe done.")


if __name__ == "__main__":
    main()
