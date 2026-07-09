#!/usr/bin/env python3
"""Capture a live card swipe from a C3 panel (session-less mode).

Polls both binary RTLog (0x0B) and key-value RTLog (0x79); prints a record only
when the payload changes, so a real swipe stands out from idle status polls.
"""
import socket, struct, sys, time

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT = 0x01
CMD_DISCONNECT = 0x02
CMD_RTLOG_BIN = 0x0B
CMD_RTLOG_KV = 0x79


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


def decode_zk_time(v):
    sec = v % 60; v //= 60
    mi = v % 60; v //= 60
    hr = v % 24; v //= 24
    d = v % 31 + 1; v //= 31
    mo = v % 12 + 1; v //= 12
    return f"{v+2000:04d}-{mo:02d}-{d:02d} {hr:02d}:{mi:02d}:{sec:02d}"


def decode_bin(data):
    out = []
    for i in range(0, len(data) - 15, 16):
        r = data[i:i+16]
        card, pin = struct.unpack("<II", r[0:8])
        out.append(f"card={card} pin={pin} verify={r[8]} door={r[9]} "
                   f"event={r[10]} inout={r[11]} time={decode_zk_time(struct.unpack('<I', r[12:16])[0])} "
                   f"raw={r.hex()}")
    tail = len(data) % 16
    if tail:
        out.append(f"(+{tail} trailing bytes: {data[-tail:].hex()})")
    return out


def main():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(4)
    s.send(build(CMD_CONNECT))
    cmd, _ = recv_frame(s)
    print(f"connect reply cmd={cmd:#x}", "OK" if cmd == 0xC8 else "ERR")

    print("\n=== SWIPE YOUR CARD NOW — capturing for 30s ===\n")
    last_bin = last_kv = None
    end = time.time() + 30
    n = 0
    while time.time() < end:
        n += 1
        # binary rtlog
        s.send(build(CMD_RTLOG_BIN))
        try:
            _, data = recv_frame(s)
            if data != last_bin:
                last_bin = data
                print(f"[poll {n}] BIN changed ({len(data)}B):")
                for line in decode_bin(data):
                    print("   ", line)
        except socket.timeout:
            print(f"[poll {n}] bin timeout")
        # key-value rtlog (human readable)
        s.send(build(CMD_RTLOG_KV))
        try:
            _, data = recv_frame(s)
            if data and data != last_kv:
                last_kv = data
                print(f"[poll {n}] KV changed: {data!r}")
        except socket.timeout:
            pass
        time.sleep(0.4)

    s.send(build(CMD_DISCONNECT))
    s.close()
    print("\ncapture done.")


if __name__ == "__main__":
    main()
