#!/usr/bin/env python3
"""Try several C3 CONNECT frame encodings and print any panel response."""
import socket, struct, sys, time

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01


def crc16(data: bytes) -> int:
    crc = 0
    for b in data:
        crc ^= b
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if (crc & 1) else (crc >> 1)
    return crc & 0xFFFF


def frame(cmd, length_counts_session, session, req, payload=b""):
    body = bytearray([VER, cmd])
    if session is not None:
        seslen = 4
        length = (seslen if length_counts_session else 0) + len(payload)
        body += bytes([length & 0xFF, length >> 8 & 0xFF])
        body += struct.pack("<HH", session & 0xFFFF, req & 0xFFFF)
    else:
        length = len(payload)
        body += bytes([length & 0xFF, length >> 8 & 0xFF])
    body += payload
    c = crc16(body)
    return bytes([START]) + bytes(body) + bytes([c & 0xFF, c >> 8 & 0xFF, END])


def try_frame(label, f):
    print(f"\n[{label}] send {f.hex()}")
    try:
        s = socket.create_connection((HOST, PORT), timeout=4)
        s.settimeout(3)
        s.send(f)
        time.sleep(0.4)
        buf = b""
        try:
            while True:
                chunk = s.recv(1024)
                if not chunk:
                    break
                buf += chunk
                if len(buf) > 4:
                    break
        except socket.timeout:
            pass
        s.close()
        if buf:
            print(f"  <== {len(buf)} bytes: {buf.hex()}")
        else:
            print("  <== (no response)")
    except Exception as e:
        print(f"  error: {e}")


variants = [
    ("0x76 sess len=4", frame(0x76, True, 0xFEFE, 0xFF02)),
    ("0x76 sess len=0", frame(0x76, False, 0xFEFE, 0xFF02)),
    ("0x01 sess len=4", frame(0x01, True, 0xFEFE, 0xFF02)),
    ("0x01 sess len=0", frame(0x01, False, 0xFEFE, 0xFF02)),
    ("0x76 no-sess", frame(0x76, False, None, 0)),
    ("0x01 no-sess", frame(0x01, False, None, 0)),
    ("0x76 sess=0000 len4", frame(0x76, True, 0x0000, 0x0000)),
]
for lbl, f in variants:
    try_frame(lbl, f)
print("\nsweep done.")
