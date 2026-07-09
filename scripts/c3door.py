#!/usr/bin/env python3
"""Validate the C3 ControlDevice OUTPUT (open door) command against live hardware.

Usage: c3door.py <host> <door> <seconds> [addr]
Tries the 5-byte and 4-byte OUTPUT payload variants and reports the reply.
"""
import socket, sys, time

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
DOOR = int(sys.argv[2]) if len(sys.argv) > 2 else 1
SECS = int(sys.argv[3]) if len(sys.argv) > 3 else 3
ADDR = int(sys.argv[4]) if len(sys.argv) > 4 else 1  # 1=door lock output, 2=aux
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CMD_CONNECT, CMD_DISCONNECT, CMD_CONTROL = 0x01, 0x02, 0x05


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


def control(s, payload, label):
    print(f"  {label}: send control payload {payload.hex()}")
    s.send(build(CMD_CONTROL, payload))
    cmd, data = recv_frame(s)
    ok = cmd == 0xC8
    print(f"    <- reply cmd={cmd:#x} {'OK ✓' if ok else 'ERR ✗'} data={data.hex()}")
    return ok


def main():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    s.send(build(CMD_CONNECT))
    cmd, _ = recv_frame(s)
    print(f"connect: cmd={cmd:#x} {'OK' if cmd==0xC8 else 'ERR'}")

    # OUTPUT operation=1, door, addr(1=lock/2=aux), duration, [trailing 0]
    print(f"\nOpening door {DOOR} for {SECS}s (addr={ADDR}) — LISTEN FOR THE RELAY CLICK")
    if not control(s, bytes([1, DOOR, ADDR, SECS, 0]), "5-byte variant"):
        time.sleep(0.3)
        control(s, bytes([1, DOOR, ADDR, SECS]), "4-byte variant")

    s.send(build(CMD_DISCONNECT))
    s.close()
    print("done.")


if __name__ == "__main__":
    main()
