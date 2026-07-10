#!/usr/bin/env python3
"""SAFELY discover the C3 SETDATA/DELETE format.

Adds a DUMMY user (Pin=88888, card 99999999, NO door authorization -> cannot
open anything), reads back to confirm which command/format worked, then deletes
it to restore the panel to its original state. Never touches existing users.
"""
import socket, sys

HOST = sys.argv[1] if len(sys.argv) > 1 else "172.17.224.1"
PORT = 4370
START, END, VER = 0xAA, 0x55, 0x01
CONNECT, DISCONNECT, DATACFG, GETDATA = 0x01, 0x02, 0x06, 0x08
DUMMY_PIN = "88888"

# command bytes to try for write/delete (unknown; 0x07/0x09 are the gaps)
SET_CMDS = [0x07, 0x09, 0x0A]
DEL_CMDS = [0x09, 0x0A, 0x07]


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


def sr(s, cmd, payload=b""):
    s.send(build(cmd, payload))
    return recv_frame(s)


def connect():
    s = socket.create_connection((HOST, PORT), timeout=5)
    s.settimeout(5)
    sr(s, CONNECT)
    return s


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


def read_user_pins(s, cfg):
    tidx, fields = cfg["user"]
    findex = sorted(f[2] for f in fields)
    _, msg = sr(s, GETDATA, [tidx, len(findex)] + findex + [0, 0])
    if not msg or msg[0] != tidx:
        return []
    fcnt = msg[1]
    ridx = list(msg[2:2 + fcnt])
    data = msg[2 + fcnt:]
    typ = {f[2]: f[1] for f in fields}
    nm = {f[2]: f[0] for f in fields}
    pins = []
    while data:
        rec = {}
        for fi in ridx:
            if not data:
                break
            size = data[0]
            val = data[1:1 + size]
            rec[nm[fi]] = int.from_bytes(val, "little") if typ[fi] in "iL" else val.decode("ascii", "ignore")
            data = data[1 + size:]
        if "Pin" in rec:
            pins.append(rec["Pin"])
        else:
            break
    return pins


# candidate SETDATA payloads to add the dummy user
def set_payloads():
    fields = f"Pin={DUMMY_PIN}\tCardNo=99999999\tPassword=\tGroup=0\tStartTime=0\tEndTime=0\tName=CLAUDETEST\tSuperAuthorize=0\tDisable=0"
    return [
        ("user NUL tab-fields", b"user\x00" + fields.encode()),
        ("user tab tab-fields", b"user\t" + fields.encode()),
        ("user, comma-fields", b"user," + fields.replace("\t", ",").encode()),
        ("tab-fields only", fields.encode()),
        ("user CRLF fields", b"user\r\n" + fields.encode()),
    ]


def del_payloads():
    return [
        ("user NUL Pin", b"user\x00Pin=" + DUMMY_PIN.encode()),
        ("user tab Pin", b"user\tPin=" + DUMMY_PIN.encode()),
        ("Pin only", b"Pin=" + DUMMY_PIN.encode()),
        ("user NUL Pin= tab", b"user\x00Pin=" + DUMMY_PIN.encode() + b"\t"),
    ]


def main():
    s = connect()
    _, cfgmsg = sr(s, DATACFG)
    cfg = parse_cfg(cfgmsg)
    before = read_user_pins(s, cfg)
    print(f"users before: {before}")

    found_set = None
    for cmd in SET_CMDS:
        if found_set:
            break
        for lbl, p in set_payloads():
            rcmd, data = sr(s, cmd, p)
            code = "OK" if rcmd == 0xC8 else (f"ERR{data.hex()}" if rcmd == 0xC9 else hex(rcmd))
            now = read_user_pins(s, cfg)
            added = DUMMY_PIN in now and DUMMY_PIN not in before
            print(f"  SET cmd=0x{cmd:02x} [{lbl}] -> {code} users={len(now)} added={added}")
            if added:
                found_set = (cmd, lbl)
                break
    print(f"\n==> SETDATA format: {found_set}")

    # cleanup: delete the dummy if it was added
    if found_set and DUMMY_PIN in read_user_pins(s, cfg):
        found_del = None
        for cmd in DEL_CMDS:
            if found_del:
                break
            for lbl, p in del_payloads():
                rcmd, data = sr(s, cmd, p)
                now = read_user_pins(s, cfg)
                removed = DUMMY_PIN not in now
                print(f"  DEL cmd=0x{cmd:02x} [{lbl}] -> {'OK' if rcmd==0xC8 else hex(rcmd)} removed={removed}")
                if removed:
                    found_del = (cmd, lbl)
                    break
        print(f"==> DELETE format: {found_del}")

    after = read_user_pins(s, cfg)
    print(f"\nusers after (should match before): {after}")
    print("RESTORED OK" if sorted(after) == sorted(before) else "!!! STATE CHANGED — CHECK PANEL")
    sr(s, DISCONNECT)
    s.close()


if __name__ == "__main__":
    main()
