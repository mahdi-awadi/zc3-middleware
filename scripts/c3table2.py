#!/usr/bin/env python3
"""READ-ONLY: dump full table schema (0x06) and read transaction+user (0x08)."""
import socket, sys, time

HOST = sys.argv[1] if len(sys.argv) > 1 else "192.168.0.102"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT, CMD_DISCONNECT, CMD_CFG, CMD_READ = 0x01, 0x02, 0x06, 0x08


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


def connect():
    s = socket.create_connection((HOST, PORT), timeout=6)
    s.settimeout(6)
    s.send(build(CMD_CONNECT))
    cmd, _ = recv_frame(s)
    if cmd != 0xC8:
        raise SystemExit("connect failed")
    return s


def rd(s, cmd, payload, label):
    s.send(build(cmd, payload))
    rcmd, data = recv_frame(s)
    tag = "OK" if rcmd == 0xC8 else (f"ERR code={data[0] if data else '?'}" if rcmd == 0xC9 else f"cmd={rcmd:#x}")
    print(f"[{label}] {payload!r} -> {tag} len={len(data)}")
    return rcmd, data


def main():
    s = connect()
    print("===== FULL 0x06 SCHEMA DUMP =====")
    _, data = rd(s, CMD_CFG, b"user", "cfg")
    print(data.decode("latin1"))

    print("\n===== READ transaction (history) =====")
    for v in [b"transaction", b"transaction\r\n", b"transaction\r\n*\r\n"]:
        try:
            _, d = rd(s, CMD_READ, v, "txn")
            if d:
                print("   ascii:", d[:600].decode("latin1").replace("\r", "\\r").replace("\n", "\\n").replace("\t", "\\t"))
                break
        except Exception as e:
            print("   exc", e); s = connect()

    print("\n===== READ user (should be empty if no cards) =====")
    _, d = rd(s, CMD_READ, b"user", "user")
    if d:
        print("   ascii:", d[:600].decode("latin1"))

    try:
        s.send(build(CMD_DISCONNECT)); s.close()
    except Exception:
        pass


if __name__ == "__main__":
    main()
