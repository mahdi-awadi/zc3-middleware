package c3

import (
	"testing"
	"time"
)

func TestCRC16KnownVector(t *testing.T) {
	// The validated connect frame body: VER,CMD,len_lo,len_hi = 01 01 00 00
	// produced CRC 0x3c50 (frame ...503c55) against live hardware.
	got := crc16([]byte{0x01, 0x01, 0x00, 0x00})
	if got != 0x3c50 {
		t.Fatalf("crc16 connect body = 0x%04x, want 0x3c50", got)
	}
}

func TestEncodeFrameRoundTrip(t *testing.T) {
	f := encodeFrame(cmdRTLogKV, []byte("hello"))
	if f[0] != frameStart || f[len(f)-1] != frameEnd {
		t.Fatalf("frame markers wrong: %x", f)
	}
	// length field must equal payload length
	if int(f[3])|int(f[4])<<8 != 5 {
		t.Fatalf("length field wrong: %x", f)
	}
	// CRC must verify over VER..last-data
	body := f[1 : len(f)-3]
	crc := uint16(f[len(f)-3]) | uint16(f[len(f)-2])<<8
	if crc16(body) != crc {
		t.Fatalf("crc mismatch")
	}
}

func TestParseRTLogKV(t *testing.T) {
	loc := time.UTC
	s := "time=2026-07-09 16:11:20\tpin=0\tcardno=212532046\teventaddr=2\tevent=27\tinoutstatus=1\tverifytype=4\tindex=428\r\n" +
		"time=2026-07-09 16:11:26\tpin=0\tcardno=212532046\teventaddr=2\tevent=27\tinoutstatus=1\tverifytype=4\tindex=432"
	evs, maxIdx := parseRTLogKV(s, loc)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2", len(evs))
	}
	if maxIdx != 432 {
		t.Fatalf("maxIdx=%d want 432", maxIdx)
	}
	e := evs[0]
	if e.CardNo != "212532046" || e.Door != 2 || e.EventType != 27 || e.Verify != 4 {
		t.Fatalf("bad parse: %+v", e)
	}
	if e.EventName != "Unregistered Card" {
		t.Fatalf("event name = %q", e.EventName)
	}
	if e.index != 428 {
		t.Fatalf("index=%d", e.index)
	}
}

func TestDoorMaskRoundTrip(t *testing.T) {
	cases := []struct {
		mask  int
		doors []int
	}{
		{3, []int{1, 2}},
		{1, []int{1}},
		{2, []int{2}},
		{5, []int{1, 3}},
	}
	for _, c := range cases {
		if got := doorsFromMask(c.mask); !eqInts(got, c.doors) {
			t.Errorf("doorsFromMask(%d)=%v want %v", c.mask, got, c.doors)
		}
		if got := maskFromDoors(c.doors); got != c.mask {
			t.Errorf("maskFromDoors(%v)=%d want %d", c.doors, got, c.mask)
		}
	}
}

func TestDecodeTable(t *testing.T) {
	// table index 1, one field index 2 (CardNo, type i), one record value 1000 (LE)
	tc := tableCfg{Name: "user", Index: 1, Fields: []tableField{{Name: "CardNo", Type: 'i', Index: 2}}}
	// reply: [tidx=1, fcnt=1, fidx=2, size=2, 0xE8,0x03(=1000)]
	data := []byte{1, 1, 2, 2, 0xE8, 0x03}
	rows, err := decodeTable(tc, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["CardNo"] != "1000" {
		t.Fatalf("decodeTable = %v", rows)
	}
}

func TestEncodeC3Time(t *testing.T) {
	// 2000-01-01 00:00:00 encodes to 0
	if v := encodeC3Time(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)); v != 0 {
		t.Fatalf("encodeC3Time epoch = %d want 0", v)
	}
	// one second later
	if v := encodeC3Time(time.Date(2000, 1, 1, 0, 0, 1, 0, time.UTC)); v != 1 {
		t.Fatalf("encodeC3Time +1s = %d want 1", v)
	}
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
