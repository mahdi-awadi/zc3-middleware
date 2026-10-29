// Package rosslare implements the Rosslare AC-215 access panel protocol (RS-485,
// usually reached through a transparent serial-to-TCP adapter) behind the same
// c3.Panel interface the rest of the middleware uses.
//
// The protocol was decoded from our own captures of AxTraxNG 27.7.1.20 talking to
// an AC-215 and to a simulated panel; byte layouts are in docs/ROSSLARE-PROTOCOL.md.
package rosslare

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	typeRequest = 0x02 // PC -> panel
	typeAnswer  = 0x01 // panel -> PC

	cmdRequestFirmVer = 0x6A // answer 0xEB AnswerFirmVer
	cmdUpdateClock    = 0x62
	cmdRequestEvents  = 0x40 // answer 0xC1 AnswerEvents
	cmdOutputsManual  = 0x22
	cmdUserData       = 0x34
	cmdCodeData       = 0x32

	ansAck       = 0x80 // Answer80: plain ack to every setting command
	ansFirmVer   = 0xEB
	ansEvents    = 0xC1
	eventNumbers = 5000 // the AC-215 event log wraps at 5000
)

var preamble = []byte{0xFF, 0xFF, 0xFF, 0x55}

// crc16 is CRC-16/XMODEM (poly 0x1021, init 0), sent high byte first.
func crc16(data []byte) uint16 {
	var c uint16
	for _, b := range data {
		c ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x1021
			} else {
				c <<= 1
			}
		}
	}
	return c
}

func sum8(data []byte) byte {
	var s byte
	for _, b := range data {
		s += b
	}
	return s
}

// encode builds FF FF FF 55 | type addr 01 01 seq len | cmd data | check.
// crc selects the 2-byte CRC-16 check; otherwise the 1-byte sum is used
// (identify and the firmware loader use the sum, everything else the CRC).
func encode(addr, seq, cmd byte, data []byte, crc bool) []byte {
	body := append([]byte{typeRequest, addr, 0x01, 0x01, seq, byte(1 + len(data)), cmd}, data...)
	out := append(append([]byte{}, preamble...), body...)
	if crc {
		return binary.BigEndian.AppendUint16(out, crc16(body))
	}
	return append(out, sum8(body))
}

// frame is one decoded frame: header fields, the command byte and its data.
type frame struct {
	typ, addr, seq, cmd byte
	data                []byte
}

// readFrame reads until one complete frame with a valid check arrives, skipping
// line noise (the real panel follows each answer with two 00 bytes). The panel
// answers in the check style it was asked in, so the caller says which to expect.
func readFrame(r *bufio.Reader, crc bool) (frame, error) {
	n := 1
	if crc {
		n = 2
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return frame{}, err
		}
		if b != 0x55 {
			continue
		}
		hdr, err := r.Peek(6)
		if err != nil {
			return frame{}, err
		}
		size := 6 + int(hdr[5]) + n
		raw, err := r.Peek(size)
		if err != nil {
			return frame{}, err
		}
		body := raw[:size-n]
		ok := raw[size-1] == sum8(body)
		if crc {
			ok = binary.BigEndian.Uint16(raw[size-2:]) == crc16(body)
		}
		if !ok || hdr[5] == 0 {
			continue // a 55 inside data or noise: resync on the next byte
		}
		f := frame{typ: body[0], addr: body[1], seq: body[4], cmd: body[6], data: append([]byte(nil), body[7:]...)}
		_, _ = r.Discard(size)
		return f, nil
	}
}

// packDate encodes a date the way the panel stores it in events, users and holidays:
// byte 0 = day | (year bits 4-6) << 5, byte 1 = month << 4 | (year bits 0-3); year = 2000 + 7 bits.
func packDate(t time.Time) [2]byte {
	y := t.Year() - 2000
	return [2]byte{byte(y>>4&0x07)<<5 | byte(t.Day()), byte(t.Month())<<4 | byte(y&0x0F)}
}

func unpackDate(b0, b1 byte) (year int, month time.Month, day int) {
	return 2000 + (int(b0>>5)<<4 | int(b1&0x0F)), time.Month(b1 >> 4), int(b0 & 0x1F)
}

// eventNames are AxTraxNG's own names for the AC-215 event types (from its log).
var eventNames = map[byte]string{
	0x11: "Access granted (card + facility)", 0x12: "Access granted (card)", 0x13: "Access granted (PIN)",
	0x19: "Access recorded",
	0x21: "Access denied (card + facility)", 0x22: "Access denied (card)", 0x23: "Access denied (PIN)",
	0x31: "Output opened", 0x32: "Output closed", 0x33: "Input opened", 0x34: "Input closed",
	0x35: "Siren started", 0x36: "Siren stopped", 0x37: "Input armed", 0x38: "Input disarmed",
	0x3A: "Reader mode changed", 0x3C: "Reader bell key pushed", 0x3D: "Door interlock by REX",
	0x43: "Reader mode changed by time zone", 0x44: "Reader mode returned to default by time zone end",
	0x47: "Timed antipassback forgive by time zone", 0x48: "Door antipassback forgive by time zone",
	0x49: "Global antipassback forgive by time zone",
	0x51: "Output unlocked by PC", 0x52: "Output re-locked by PC",
	0x53: "Reader mode changed by PC", 0x54: "Reader mode returned to default by PC",
	0x55: "Input disarmed by PC", 0x56: "Input returned to default by PC",
	0x63: "Alarm started",
	0x71: "Dual authentication initiated (card + facility)", 0x72: "Dual authentication initiated (card)",
	0x73: "Dual authentication initiated (PIN)",
	0x81: "Panel reset (power on)", 0x82: "Panel loads factory settings", 0x83: "Panel firmware download succeeded",
	0x84: "Manual panel reset, factory settings loaded", 0x85: "Remote modem answered", 0x86: "Remote modem disconnected",
	0x87: "Communication failed for over 1 minute", 0x88: "Communication restored",
	0x89: "Event memory full", 0x8A: "Event memory low", 0x8B: "Event memory normal",
	0x8C: "AC power fail", 0x8D: "AC power restored", 0x8E: "Low battery", 0x8F: "Battery OK",
	0x90: "Configuration mode",
	0x91: "Access logged (card + facility)", 0x92: "Access logged (card)", 0x93: "Access logged (PIN)",
}

// denyReasons are the subtype names for access-denied events (0x21-0x23).
var denyReasons = map[byte]string{1: "unknown card", 2: "wrong facility code", 3: "unknown card (secondary reader type)"}

func eventName(typ, sub byte) string {
	n, ok := eventNames[typ]
	if !ok {
		return fmt.Sprintf("Event 0x%02X", typ)
	}
	if typ >= 0x21 && typ <= 0x23 {
		if r, ok := denyReasons[sub]; ok {
			n += ": " + r
		}
	}
	return n
}
