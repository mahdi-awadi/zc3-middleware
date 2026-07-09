#!/usr/bin/env python3
"""Empirical validator for the ZKTeco C3 protocol against a live panel.

Confirms: frame layout (AA 01 cmd len.. data crc.. 55), CRC-16/ARC (poly 0xA001),
CONNECT_SESSION handshake + session-id parsing, and RTLog polling/decoding.
Reachable in dev via the WSL->Windows portproxy at 172.17.224.1:4370.
"""
import socket
import struct
import sys
import time

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370

START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT_SESSION = 0x76
CMD_CONNECT_SESSIONLESS = 0x01
CMD_DISCONNECT = 0x02
CMD_RTLOG = 0x0B


def crc16(data: bytes) -> int:
    """CRC-16/ARC (reflected, poly 0xA001, init 0x0000)."""
    crc = 0x0000
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if (crc & 1) else (crc >> 1)
    return crc & 0xFFFF


def build(cmd: int, session_id: int, request_nr: int, payload: bytes = b"") -> bytes:
    body = bytearray()
    include_session = session_id != 0
    seslen = 4 if include_session else 0
    length = seslen + len(payload)
    body += bytes([VER, cmd, length & 0xFF, (length >> 8) & 0xFF])
    if include_session:
        body += struct.pack("<HH", session_id & 0xFFFF, request_nr & 0xFFFF)
    body += payload
    c = crc16(body)
    return bytes([START]) + bytes(body) + bytes([c & 0xFF, (c >> 8) & 0xFF]) + bytes([END])


def recv_frame(sock) -> tuple[int, bytes]:
    hdr = recvn(sock, 5)  # START VER CMD LEN_LO LEN_HI
    if hdr[0] != START:
        raise ValueError(f"bad start byte {hdr[0]:#x}")
    cmd = hdr[2]
    length = hdr[3] | (hdr[4] << 8)
    rest = recvn(sock, length + 3)  # data + crc(2) + end(1)
    data = rest[:length]
    crc_lo, crc_hi, end = rest[length], rest[length + 1], rest[length + 2]
    got = crc_lo | (crc_hi << 8)
    want = crc16(hdr[1:] + data)  # CRC over VER..last data byte
    ok = (got == want) and (end == END)
    print(f"  <- cmd={cmd:#x} len={length} crc={'OK' if got==want else f'BAD got={got:#x} want={want:#x}'} end={'OK' if end==END else hex(end)}")
    if not ok:
        raise ValueError("frame validation failed")
    return cmd, data


def recvn(sock, n: int) -> bytes:
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("connection closed")
        buf += chunk
    return buf


def decode_zk_time(v: int) -> str:
    sec = v % 60; v //= 60
    minute = v % 60; v //= 60
    hour = v % 24; v //= 24
    day = v % 31 + 1; v //= 31
    month = v % 12 + 1; v //= 12
    year = v + 2000
    return f"{year:04d}-{month:02d}-{day:02d} {hour:02d}:{minute:02d}:{sec:02d}"


def decode_records(data: bytes):
    for i in range(0, len(data) - len(data) % 16, 16):
        r = data[i:i + 16]
        card, pin = struct.unpack("<II", r[0:8])
        verified, port, etype, inout = r[8], r[9], r[10], r[11]
        t = struct.unpack("<I", r[12:16])[0]
        if etype == 0xFF:
            print(f"    [status] raw={r.hex()}")
        else:
            print(f"    [EVENT] card={card} pin={pin} door/reader={port} "
                  f"event_type={etype} verify={verified} inout={inout} "
                  f"time={decode_zk_time(t)} (zk={t}) raw={r.hex()}")


def main():
    print(f"connecting to {HOST}:{PORT} ...")
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)

    # --- CONNECT (session-less: this firmware rejects 0x76 session mode) ---
    sid, req = 0, 0  # session_id 0 => build() emits no session header, len=0
    frame = build(CMD_CONNECT_SESSIONLESS, sid, req)
    print(f"CONNECT(0x01, session-less) -> {frame.hex()}")
    s.send(frame)
    cmd, data = recv_frame(s)
    if cmd != 0xC8:
        print(f"  unexpected connect reply cmd={cmd:#x}")
        s.close(); return
    print("  connected OK (session-less)")

    # --- Poll RTLog; SWIPE A CARD during the window ---
    print("\nPolling RTLog for ~25s — SWIPE A CARD NOW ...")
    deadline = time.time() + 25
    while time.time() < deadline:
        s.send(build(CMD_RTLOG, sid, req))
        try:
            cmd, data = recv_frame(s)
            if data:
                decode_records(data)
        except socket.timeout:
            print("  (rtlog timeout)")
        time.sleep(1)

    s.send(build(CMD_DISCONNECT, sid, req))
    s.close()
    print("done.")


if __name__ == "__main__":
    main()
