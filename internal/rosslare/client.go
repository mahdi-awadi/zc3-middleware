package rosslare

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// ErrUnsupported is returned for c3.Panel operations the AC-215 has no known command for.
var ErrUnsupported = errors.New("rosslare: not supported by the AC-215 driver")

// Client is one AC-215 on an RS-485 bus reached through a transparent TCP adapter
// (USR-W610: TCP server, 9600 8N1). The bus is half duplex and the panel only
// answers when asked, so every round-trip holds the mutex.
type Client struct {
	addr          string
	panelAddr     byte
	timeout       time.Duration
	pollInterval  time.Duration
	loc           *time.Location
	replayBacklog bool
	allowWrites   bool
	crc           bool
	statePath     string

	mu         sync.Mutex
	conn       net.Conn
	r          *bufio.Reader
	seq        byte
	firmware   string // e.g. "ac215v04_08_23 / btl_ac225v_01_02"
	startEvent int    // first event number the next Events stream asks for
	header     []byte // status bytes of the latest AnswerEvents
	headerAt   time.Time

	smu sync.Mutex // guards st and the state file
	st  state
}

type Option func(*Client)

// WithAllowWrites enables card enroll/revoke. Off by default, like the C3 driver.
func WithAllowWrites(v bool) Option { return func(c *Client) { c.allowWrites = v } }

// WithReplayBacklog resumes events from the cursor saved in the state file, so
// swipes made while the middleware was down reach the audit log. Default false:
// stream only events newer than the panel's last one at connect.
func WithReplayBacklog(v bool) Option { return func(c *Client) { c.replayBacklog = v } }

// WithTimeout sets the per-try I/O timeout (each command is tried 3 times).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithPollInterval sets how often the panel is asked for new events.
func WithPollInterval(d time.Duration) Option { return func(c *Client) { c.pollInterval = d } }

// WithLocation sets the timezone of the panel clock.
func WithLocation(l *time.Location) Option { return func(c *Client) { c.loc = l } }

// WithAddress sets the panel's bus address (DIP switches 4-8), 1..32.
func WithAddress(n int) Option { return func(c *Client) { c.panelAddr = byte(n) } }

// WithCRC picks the check for normal commands: CRC-16 (true, what AxTraxNG
// 27.7 sends) or the 1-byte sum. Identify always uses the sum.
func WithCRC(v bool) Option { return func(c *Client) { c.crc = v } }

// WithStatePath sets the JSON file that records the cards written to the panel
// (the AC-215 cannot be read back) and the event cursor.
func WithStatePath(p string) Option { return func(c *Client) { c.statePath = p } }

// New returns a client for the panel behind addr (host:port of the adapter). It
// loads the state file, so a damaged file is reported here instead of overwritten.
func New(addr string, opts ...Option) (*Client, error) {
	c := &Client{addr: addr, panelAddr: 1, timeout: 5 * time.Second, pollInterval: time.Second,
		loc: time.Local, crc: true, statePath: "rosslare-state.json"}
	for _, o := range opts {
		o(c)
	}
	return c, c.load()
}

var _ c3.Panel = (*Client)(nil)

// Connect opens the TCP session and identifies the panel (RequestFirmVer).
func (c *Client) Connect(ctx context.Context) error {
	saved := c.savedCursor() // before c.mu: SetCard takes smu then mu
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("rosslare: dial %s: %w", c.addr, err)
	}
	c.conn, c.r = conn, bufio.NewReader(conn)
	// AnswerFirmVer: firmware name (16), loader name (16), last event number (2), 4 unknown bytes.
	data, err := c.exchangeLocked(cmdRequestFirmVer, nil, ansFirmVer)
	if err == nil && len(data) < 34 {
		err = fmt.Errorf("short answer % x", data)
	}
	if err != nil {
		conn.Close()
		c.conn = nil
		return fmt.Errorf("rosslare: identify panel %d: %w", c.panelAddr, err)
	}
	c.firmware = cstr(data[:16]) + " / " + cstr(data[16:32])
	c.startEvent = (int(binary.BigEndian.Uint16(data[32:34])) + 1) % eventNumbers
	if c.replayBacklog && saved >= 0 {
		c.startEvent = saved
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// exchange runs a request/answer round-trip under the mutex.
func (c *Client) exchange(cmd byte, data []byte, want byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exchangeLocked(cmd, data, want)
}

// exchangeLocked sends one command and returns the data of the answer whose
// command byte is want. A timeout or another answer (a plain 80 to RequestEvents
// means "busy") is retried with the same seq, 3 tries in all like AxTraxNG.
// Caller holds c.mu.
func (c *Client) exchangeLocked(cmd byte, data []byte, want byte) ([]byte, error) {
	if c.conn == nil {
		return nil, errors.New("rosslare: not connected")
	}
	c.seq++
	crc := c.crc && cmd != cmdRequestFirmVer
	req := encode(c.panelAddr, c.seq, cmd, data, crc)
	var last error
	for try := 0; try < 3; try++ {
		_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
		if _, err := c.conn.Write(req); err != nil {
			return nil, fmt.Errorf("rosslare: write: %w", err)
		}
		f, err := c.readAnswer(crc)
		switch {
		case err == nil && f.cmd == want:
			return f.data, nil
		case err == nil:
			last = fmt.Errorf("rosslare: command 0x%02X answered 0x%02X, want 0x%02X", cmd, f.cmd, want)
		case isTimeout(err):
			last = fmt.Errorf("rosslare: command 0x%02X: no answer from panel %d", cmd, c.panelAddr)
		default:
			return nil, fmt.Errorf("rosslare: read: %w", err)
		}
	}
	return nil, last
}

// readAnswer skips frames that are not this panel's answer to the current seq
// (late answers to an earlier try, other panels on the bus).
func (c *Client) readAnswer(crc bool) (frame, error) {
	for {
		f, err := readFrame(c.r, crc)
		if err != nil || (f.typ == typeAnswer && f.addr == c.panelAddr && f.seq == c.seq) {
			return f, err
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func cstr(b []byte) string { return strings.TrimRight(string(b), "\x00 ") }

// ---- capabilities ----

// DeviceInfo reports what AnswerFirmVer said at connect. The AC-215 has one door
// with two readers; its two lock outputs (OUT1, OUT2) are doors 1 and 2 here.
func (c *Client) DeviceInfo(ctx context.Context) (c3.DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return c3.DeviceInfo{}, errors.New("rosslare: not connected")
	}
	host, _, _ := net.SplitHostPort(c.addr)
	return c3.DeviceInfo{DeviceName: fmt.Sprintf("AC-215 #%d", c.panelAddr), Firmware: c.firmware, IPAddress: host, Doors: 2}, nil
}

func (c *Client) GetParams(ctx context.Context, keys ...string) (map[string]string, error) {
	return nil, ErrUnsupported
}
func (c *Client) SetParams(ctx context.Context, kv map[string]string) error { return ErrUnsupported }
func (c *Client) TableNames(ctx context.Context) ([]string, error)         { return nil, ErrUnsupported }
func (c *Client) GetTable(ctx context.Context, name string) ([]map[string]string, error) {
	return nil, ErrUnsupported
}
func (c *Client) CancelAlarm(ctx context.Context) error { return ErrUnsupported }
func (c *Client) Restart(ctx context.Context) error     { return ErrUnsupported }

// SyncTime sets the panel clock (UpdateClock: YY MM DOW DD hh mm ss).
// DOW 4 = Thursday was captured; Sunday = 0 is assumed.
func (c *Client) SyncTime(ctx context.Context, t time.Time) error {
	t = t.In(c.loc)
	data := []byte{byte(t.Year() - 2000), byte(t.Month()), byte(t.Weekday()), byte(t.Day()),
		byte(t.Hour()), byte(t.Minute()), byte(t.Second())}
	_, err := c.exchange(cmdUpdateClock, data, ansAck)
	return err
}

// OutputsManual slots: OUT1 (door), OUT1A, OUT2, OUT2A; modes below.
const (
	modeNormal = 0x01 // back to normal (schedule / locked)
	modeTimed  = 0x02 // open for min:sec
	modeLatch  = 0x03 // open until told otherwise
)

// output sends OutputsManual: 16 slots of `mode 00 min sec`, mode 00 = leave alone.
func (c *Client) output(slot int, mode byte, seconds int) error {
	data := make([]byte, 64)
	data[slot*4] = mode
	if mode == modeTimed {
		data[slot*4+2], data[slot*4+3] = byte(seconds/60), byte(seconds%60)
	}
	_, err := c.exchange(cmdOutputsManual, data, ansAck)
	return err
}

// OpenDoor pulses OUT1 (door 1) or OUT2 (door 2) for seconds (1..254).
func (c *Client) OpenDoor(ctx context.Context, door, seconds int) error {
	if door != 1 && door != 2 {
		return fmt.Errorf("rosslare: door must be 1 or 2")
	}
	seconds = min(max(seconds, 1), 254)
	return c.output((door-1)*2, modeTimed, seconds)
}

// SetAux drives OUT1A (index 1) or OUT2A (index 2): on for seconds, on until
// switched off when seconds is 0, or back to normal when off.
func (c *Client) SetAux(ctx context.Context, index int, on bool, seconds int) error {
	if index != 1 && index != 2 {
		return fmt.Errorf("rosslare: aux index must be 1 or 2")
	}
	slot := index*2 - 1
	switch {
	case !on:
		return c.output(slot, modeNormal, 0)
	case seconds > 0:
		return c.output(slot, modeTimed, min(seconds, 254))
	default:
		return c.output(slot, modeLatch, 0)
	}
}

// Status returns the raw status bytes of the latest event poll (the AC-215 sends
// door / input / reader state in the AnswerEvents header; only partly mapped).
func (c *Client) Status(ctx context.Context) (c3.PanelStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.header == nil {
		return c3.PanelStatus{}, errors.New("rosslare: no status yet (it arrives with the event poll)")
	}
	return c3.PanelStatus{Time: c.headerAt, Raw: hex.EncodeToString(c.header)}, nil
}

// ---- events ----

// Events polls RequestEvents with the number of the next event wanted and
// streams what the panel returns (at most 2 per answer, so it asks again at once
// while events keep coming).
func (c *Client) Events(ctx context.Context) (<-chan c3.Event, error) {
	c.mu.Lock()
	next := c.startEvent
	c.mu.Unlock()
	out := make(chan c3.Event, 128)
	go func() {
		defer close(out)
		t := time.NewTicker(c.pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			for ctx.Err() == nil {
				data, err := c.exchange(cmdRequestEvents, []byte{byte(next >> 8), byte(next)}, ansEvents)
				if err != nil {
					return // drop stream; supervisor reconnects
				}
				recs, nums := c.parseEvents(data)
				fresh := 0
				for i, r := range recs {
					if (nums[i]-next+eventNumbers)%eventNumbers >= eventNumbers/2 {
						continue // a number already passed: never re-emit (the engine opens doors on events)
					}
					next = (nums[i] + 1) % eventNumbers // the panel's numbering wins if it skipped ahead
					fresh++
					select {
					case out <- c.toEvent(r):
					case <-ctx.Done():
						return
					}
				}
				if fresh == 0 {
					break
				}
				c.saveCursor(next)
			}
		}
	}()
	return out, nil
}

// parseEvents splits AnswerEvents data: 16 status bytes (byte 2 = record count),
// count x 16-byte records, 2 bytes, then count x 2-byte event numbers.
// A record with type 0 ends the list. "C1 00" alone means no events.
func (c *Client) parseEvents(data []byte) (recs [][]byte, nums []int) {
	if len(data) < 16 {
		return nil, nil
	}
	c.mu.Lock()
	c.header, c.headerAt = append([]byte(nil), data[:16]...), time.Now()
	c.mu.Unlock()
	n := int(data[2])
	if len(data) < 16+n*18+2 {
		return nil, nil
	}
	for i := 0; i < n; i++ {
		r := data[16+i*16 : 32+i*16]
		if r[7] == 0 {
			break
		}
		o := 16 + n*16 + 2 + i*2
		recs = append(recs, r)
		nums = append(nums, int(binary.BigEndian.Uint16(data[o:])))
	}
	return recs, nums
}

func granted(typ byte) bool {
	return typ >= 0x11 && typ <= 0x13 || typ == 0x19 || typ >= 0x71 && typ <= 0x73 || typ >= 0x91 && typ <= 0x93
}

// toEvent decodes one 16-byte record. Source byte = kind << 4 | number:
// 1x door, 2x reader (Reader 1 = in, Reader 2 = out by AxTraxNG's default).
func (c *Client) toEvent(r []byte) c3.Event {
	y, mo, d := unpackDate(r[2], r[3])
	ev := c3.Event{
		Time:      time.Date(y, mo, d, int(r[6]&0x1F), int(r[5]), int(r[4]), 0, c.loc),
		EventType: int(r[7]),
		EventName: eventName(r[7], r[8]),
		Raw:       hex.EncodeToString(r),
	}
	switch kind, n := r[9]>>4, int(r[9]&0x0F); kind {
	case 1:
		ev.Door = n
	case 2:
		// Turnstile wiring: Reader 1 opens OUT1 (door 1), Reader 2 opens OUT2 (door 2).
		// access.json reader_door overrides this.
		ev.Reader, ev.Door = n, n
		ev.InOut = map[int]string{1: "in", 2: "out"}[n]
	}
	switch {
	case granted(r[7]):
		// +0..+1 card slot, +14..+15 user slot: names come from the state file.
		if k, ok := c.cardAt(int(binary.BigEndian.Uint16(r[0:2]))); ok {
			ev.CardNo, ev.PIN = k.CardNo, k.PIN
		}
	case r[7] == 0x21 || r[7] == 0x22:
		// ponytail: refused card = facility at +10..+11, code from +12; how a real panel
		// fills +12..+15 for a W26 card is unchecked, verify with one swipe on a healthy board.
		code := uint64(binary.BigEndian.Uint32(r[12:16]))
		if fac := uint64(binary.BigEndian.Uint16(r[10:12])); fac != 0 && code <= 0xFFFF {
			code = fac<<16 | code
		}
		ev.CardNo = fmt.Sprint(code)
	}
	return ev
}
