package rosslare

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(string(bytes.ReplaceAll([]byte(s), []byte(" "), nil)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames recorded on the wire between AxTraxNG and the real AC-215F.
func TestRecordedFrames(t *testing.T) {
	// DeletePanelData as AxTraxNG sent it: CRC-16/XMODEM 5D A7.
	if got := encode(1, 0x16, 0x64, []byte{1}, true); !bytes.Equal(got, unhex(t, "ffffff55 0201010116 02 6401 5da7")) {
		t.Fatalf("encode DeletePanelData = % x", got)
	}
	// The real board's AnswerFirmVer (sum check E1, then its two 00 bytes of line noise).
	real := unhex(t, "ffffff55 01 01 02 01 05 27 eb 6163323135763034 5f30355f31300000 62746c5f61633232 35765f30315f3032 11f3021000 00 e1 0000")
	f, err := readFrame(bufio.NewReader(bytes.NewReader(real)), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.typ != typeAnswer || f.seq != 5 || f.cmd != ansFirmVer || cstr(f.data[:16]) != "ac215v04_05_10" || binary.BigEndian.Uint16(f.data[32:]) != 0x11F3 {
		t.Fatalf("readFrame = %+v", f)
	}
}

// Records captured from AxTraxNG for user "Test Card" (group Master, W26 123/4660).
func TestRecordsMatchAxTraxNG(t *testing.T) {
	day := time.Date(2026, 10, 29, 21, 0, 0, 0, time.UTC)
	k := c3.Card{CardNo: "8065588", Timezone: 1, Doors: []int{1, 2}}
	want := unhex(t, "0001 ffffffff 3daa ffff 0000 0101 000000000000 80 000000 00000000 ffffffff 00000000")
	if got := userRecord(1, k, day, time.UTC); !bytes.Equal(got, want) {
		t.Fatalf("userRecord\n got % x\nwant % x", got, want)
	}
	if got := codeRecord(1, 8065588); !bytes.Equal(got, unhex(t, "0001 0001 0000000000 7b1234")) {
		t.Fatalf("codeRecord = % x", got)
	}
	// Valid until 2026-12-31 (3F CA, 23:59), Reader 1 only on zone 2 ("Entry R1" = 02 00).
	k.End, k.Timezone, k.Doors = time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), 2, []int{1}
	got := userRecord(1, k, day, time.UTC)
	if !bytes.Equal(got[8:10], []byte{0x3F, 0xCA}) || !bytes.Equal(got[12:14], []byte{2, 0}) || !bytes.Equal(got[24:26], []byte{23, 59}) {
		t.Fatalf("userRecord with end date = % x", got)
	}
	if d := packDate(time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)); d != [2]byte{0x39, 0xCA} {
		t.Fatalf("packDate holiday = % x", d)
	}
	if y, m, d := unpackDate(0x3D, 0xAA); y != 2026 || m != 10 || d != 29 {
		t.Fatalf("unpackDate = %d-%d-%d", y, m, d)
	}
}

// fakePanel answers like the real AC-215 / fakepanel.py and records every request.
type fakePanel struct {
	ln     net.Listener
	mu     sync.Mutex
	got    []frame
	events [][]byte // queued AnswerEvents payloads; then "C1 00"
}

func startFake(t *testing.T) *fakePanel {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePanel{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			hdr, err := r.Peek(11) // preamble, 6 header bytes, cmd
			if err != nil {
				return
			}
			crc := hdr[10] != cmdRequestFirmVer
			f, err := readFrame(r, crc)
			if err != nil {
				return
			}
			p.mu.Lock()
			p.got = append(p.got, f)
			payload := []byte{ansAck}
			switch f.cmd {
			case cmdRequestFirmVer:
				payload = append([]byte{ansFirmVer}, "ac215v04_08_23\x00\x00btl_ac225v_01_02\x11\xf3\x02\x10\x00\x00"...)
			case cmdRequestEvents:
				payload = []byte{ansEvents, 0}
				if len(p.events) > 0 {
					payload, p.events = p.events[0], p.events[1:]
				}
			}
			p.mu.Unlock()
			body := append([]byte{typeAnswer, f.addr, 0x02, 0x01, f.seq, byte(len(payload))}, payload...)
			out := append(append([]byte{}, preamble...), body...)
			if crc {
				out = binary.BigEndian.AppendUint16(out, crc16(body))
			} else {
				out = append(out, sum8(body))
			}
			conn.Write(append(out, 0, 0))
		}
	}()
	return p
}

func (p *fakePanel) requests(cmd byte) (out [][]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.got {
		if f.cmd == cmd {
			out = append(out, f.data)
		}
	}
	return out
}

func TestDriverAgainstFakePanel(t *testing.T) {
	p := startFake(t)
	// Two records in one answer: a granted pass on Reader 2 (card slot 1) and a
	// refused W26 card 123/4660 on Reader 1, event numbers 0x11F4 and 0x11F5.
	hdr := make([]byte, 16)
	hdr[2] = 2
	p.events = [][]byte{append(append([]byte{ansEvents}, hdr...), unhex(t,
		"0001 3daa 060c16 11 00 22 0000 0000 0001"+
			"0000 3daa 070c16 21 01 21 007b 0000 1234"+
			"0000 11f4 11f5")...),
		// The same pass again with an already-used number must be dropped.
		append(append([]byte{ansEvents}, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0), unhex(t,
			"0001 3daa 060c16 11 00 22 0000 0000 0001 0000 11f4")...)}

	state := filepath.Join(t.TempDir(), "state.json")
	cl, err := New(p.ln.Addr().String(), WithAllowWrites(true), WithPollInterval(10*time.Millisecond),
		WithTimeout(time.Second), WithLocation(time.UTC), WithStatePath(state))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := cl.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if info, _ := cl.DeviceInfo(ctx); info.Firmware != "ac215v04_08_23 / btl_ac225v_01_02" {
		t.Fatalf("firmware = %q", info.Firmware)
	}

	if err := cl.SetCard(ctx, c3.Card{CardNo: "8065588", Name: "Test Card", Role: "teacher"}); err != nil {
		t.Fatal(err)
	}
	if u := p.requests(cmdUserData); len(u) != 1 || !bytes.Equal(u[0][:2], []byte{0, 1}) || !bytes.Equal(u[0][12:14], []byte{1, 1}) {
		t.Fatalf("UserData sent = % x", u)
	}
	if k := p.requests(cmdCodeData); len(k) != 1 || !bytes.Equal(k[0], unhex(t, "0001 0001 0000000000 7b1234")) {
		t.Fatalf("CodeData sent = % x", k)
	}

	evctx, cancel := context.WithCancel(ctx)
	ch, _ := cl.Events(evctx)
	var evs []c3.Event
	for len(evs) < 2 {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
		case <-time.After(2 * time.Second):
			t.Fatalf("got %d events", len(evs))
		}
	}
	for i := 0; len(p.requests(cmdRequestEvents)) < 3 && i < 200; i++ {
		time.Sleep(10 * time.Millisecond) // the next poll follows the drained answer
	}
	cancel()
	for ev := range ch {
		t.Fatalf("re-sent event number was emitted again: %+v", ev)
	}
	if e := evs[0]; e.CardNo != "8065588" || e.InOut != "out" || e.Reader != 2 || e.EventType != 0x11 ||
		!e.Time.Equal(time.Date(2026, 10, 29, 22, 12, 6, 0, time.UTC)) {
		t.Fatalf("granted event = %+v", e)
	}
	if e := evs[1]; e.CardNo != "8065588" || e.InOut != "in" || e.EventName != "Access denied (card + facility): unknown card" {
		t.Fatalf("denied event = %+v", e)
	}
	if asks := p.requests(cmdRequestEvents); !bytes.Equal(asks[0], []byte{0x11, 0xF4}) || !bytes.Equal(asks[1], []byte{0x11, 0xF6}) {
		t.Fatalf("RequestEvents asked % x", asks)
	}

	if err := cl.OpenDoor(ctx, 2, 5); err != nil {
		t.Fatal(err)
	}
	if o := p.requests(cmdOutputsManual); len(o) != 1 || len(o[0]) != 64 || !bytes.Equal(o[0][8:12], []byte{2, 0, 0, 5}) {
		t.Fatalf("OutputsManual sent = % x", o)
	}

	// The state file survives a restart, then the card is revoked.
	again, err := New("unused:1", WithStatePath(state), WithAllowWrites(true))
	if err != nil {
		t.Fatal(err)
	}
	if users, _ := again.ListUsers(ctx); len(users) != 1 || users[0].Name != "Test Card" || users[0].Role != "teacher" || users[0].UID != 1 {
		t.Fatalf("reloaded users = %+v", users)
	}
	if err := cl.DeleteCard(ctx, "8065588"); err != nil {
		t.Fatal(err)
	}
	if k := p.requests(cmdCodeData); len(k) != 2 || !bytes.Equal(k[1], unhex(t, "0001 ffffffffffffffffffff")) {
		t.Fatalf("CodeData delete = % x", k)
	}
	if cards, _ := cl.ListCards(ctx); len(cards) != 0 {
		t.Fatalf("cards after delete = %+v", cards)
	}
}
