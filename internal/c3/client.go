package c3

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a C3 panel connection. It keeps one long-lived TCP session and
// serializes commands with a mutex (the panel is single-connection for control).
// The realtime event stream and command calls share the connection; each
// round-trip holds the mutex only for its own request/response.
type Client struct {
	addr          string
	timeout       time.Duration
	pollInterval  time.Duration
	loc           *time.Location
	replayBacklog bool
	allowWrites   bool

	mu   sync.Mutex
	conn net.Conn
}

type Option func(*Client)

// WithAllowWrites enables data-table write operations (card enroll/revoke,
// timezone/holiday/access-level writes). Off by default: writes to a live
// access panel are gated behind explicit opt-in.
func WithAllowWrites(v bool) Option { return func(c *Client) { c.allowWrites = v } }

// WithReplayBacklog controls whether buffered events already on the panel are
// emitted on connect. Default false (prime the cursor, only stream new events);
// true replays the panel's recent buffer once per session (useful for backfill).
func WithReplayBacklog(v bool) Option { return func(c *Client) { c.replayBacklog = v } }

// WithTimeout sets the per-command I/O timeout.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithPollInterval sets how often the realtime log is polled for new events.
func WithPollInterval(d time.Duration) Option { return func(c *Client) { c.pollInterval = d } }

// WithLocation sets the timezone used to interpret panel-local event times.
func WithLocation(l *time.Location) Option { return func(c *Client) { c.loc = l } }

func New(addr string, opts ...Option) *Client {
	c := &Client{addr: addr, timeout: 5 * time.Second, pollInterval: time.Second, loc: time.Local}
	for _, o := range opts {
		o(c)
	}
	return c
}

var _ Panel = (*Client)(nil)

// Addr returns the panel address the client dials.
func (c *Client) Addr() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.addr
}

// SetAddr changes the address used for the NEXT dial. Callers switch panels at
// runtime with SetAddr followed by Close: the supervisor's reconnect loop then
// re-establishes the session against the new address.
func (c *Client) SetAddr(addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addr = addr
}

// Connect opens the TCP session and performs the session-less handshake.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	d := net.Dialer{Timeout: c.timeout}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("c3: dial %s: %w", c.addr, err)
	}
	c.conn = conn
	if _, err := c.exchangeLocked(cmdConnect, nil); err != nil {
		conn.Close()
		c.conn = nil
		return fmt.Errorf("c3: connect handshake: %w", err)
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	_, _ = c.exchangeLocked(cmdDisconnect, nil) // best-effort
	err := c.conn.Close()
	c.conn = nil
	return err
}

// exchange runs a request/response round-trip under the mutex.
func (c *Client) exchange(cmd byte, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exchangeLocked(cmd, payload)
}

// exchangeLocked sends a frame and reads the reply. Caller holds c.mu.
func (c *Client) exchangeLocked(cmd byte, payload []byte) ([]byte, error) {
	if c.conn == nil {
		return nil, fmt.Errorf("c3: not connected")
	}
	deadline := time.Now().Add(c.timeout)
	_ = c.conn.SetDeadline(deadline)

	if _, err := c.conn.Write(encodeFrame(cmd, payload)); err != nil {
		return nil, fmt.Errorf("c3: write: %w", err)
	}
	rcmd, data, err := readFrame(c.conn)
	if err != nil {
		return nil, err
	}
	switch rcmd {
	case replyOK:
		return data, nil
	case replyError:
		code := -1
		if len(data) > 0 {
			code = int(data[0])
		}
		return nil, fmt.Errorf("c3: panel error (code 0x%02x)", code)
	default:
		return nil, fmt.Errorf("c3: unexpected reply cmd 0x%02x", rcmd)
	}
}

// exchangeData runs a request that may return a paged response: zero or more
// replyData (0x0D) chunks followed by a terminating replyOK (0xC8). Used for
// large table reads (e.g. the transaction log).
func (c *Client) exchangeData(cmd byte, payload []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, fmt.Errorf("c3: not connected")
	}
	_ = c.conn.SetDeadline(time.Now().Add(c.timeout))
	if _, err := c.conn.Write(encodeFrame(cmd, payload)); err != nil {
		return nil, fmt.Errorf("c3: write: %w", err)
	}
	var out []byte
	gotData := false
	for {
		// After the first data chunk, some paged tables (e.g. an empty
		// transaction log) send no terminating 0xC8; use a short continuation
		// deadline and treat its timeout as end-of-stream.
		d := c.timeout
		if gotData {
			d = 800 * time.Millisecond
		}
		_ = c.conn.SetDeadline(time.Now().Add(d))
		rcmd, data, err := readFrame(c.conn)
		if err != nil {
			if gotData && isTimeout(err) {
				return out, nil // paged stream ended without explicit terminator
			}
			return nil, err
		}
		switch rcmd {
		case replyData:
			out = append(out, data...)
			gotData = true
		case replyOK:
			out = append(out, data...)
			return out, nil
		case replyError:
			code := -1
			if len(data) > 0 {
				code = int(data[0])
			}
			return nil, fmt.Errorf("c3: panel error (code 0x%02x)", code)
		default:
			return nil, fmt.Errorf("c3: unexpected reply cmd 0x%02x", rcmd)
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// readFrame reads one AA..55 frame and verifies its CRC and end marker.
func readFrame(r net.Conn) (cmd byte, data []byte, err error) {
	hdr := make([]byte, 5)
	if _, err = readFull(r, hdr); err != nil {
		return 0, nil, fmt.Errorf("c3: read header: %w", err)
	}
	if hdr[0] != frameStart {
		return 0, nil, fmt.Errorf("c3: bad start byte 0x%02x", hdr[0])
	}
	length := int(hdr[3]) | int(hdr[4])<<8
	rest := make([]byte, length+3) // data + crc(2) + end(1)
	if _, err = readFull(r, rest); err != nil {
		return 0, nil, fmt.Errorf("c3: read body: %w", err)
	}
	data = rest[:length]
	gotCRC := uint16(rest[length]) | uint16(rest[length+1])<<8
	if end := rest[length+2]; end != frameEnd {
		return 0, nil, fmt.Errorf("c3: bad end byte 0x%02x", end)
	}
	// CRC covers VER..last data byte (hdr[1:] + data).
	want := crc16(append(append([]byte{}, hdr[1:]...), data...))
	if gotCRC != want {
		return 0, nil, fmt.Errorf("c3: crc mismatch got=0x%04x want=0x%04x", gotCRC, want)
	}
	return hdr[2], data, nil
}

func readFull(r net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

// ---- capabilities ----

// DeviceInfo reads identity parameters from the panel.
func (c *Client) DeviceInfo(ctx context.Context) (DeviceInfo, error) {
	keys := []string{"~SerialNumber", "~DeviceName", "FirmVer", "IPAddress", "MAC", "LockCount"}
	m, err := c.GetParams(ctx, keys...)
	if err != nil {
		return DeviceInfo{}, err
	}
	doors, _ := strconv.Atoi(m["LockCount"])
	return DeviceInfo{
		SerialNumber: m["~SerialNumber"],
		DeviceName:   m["~DeviceName"],
		Firmware:     m["FirmVer"],
		IPAddress:    m["IPAddress"],
		MAC:          m["MAC"],
		Doors:        doors,
	}, nil
}

// GetParams reads device parameters by key (GETPARAM, k=v reply).
func (c *Client) GetParams(ctx context.Context, keys ...string) (map[string]string, error) {
	payload := []byte(strings.Join(keys, ","))
	data, err := c.exchange(cmdGetParam, payload)
	if err != nil {
		return nil, err
	}
	return parseKV(string(data), ","), nil
}

// SetParams writes device parameters (GETPARAM command carries k=v to set).
func (c *Client) SetParams(ctx context.Context, kv map[string]string) error {
	parts := make([]string, 0, len(kv))
	for k, v := range kv {
		parts = append(parts, k+"="+v)
	}
	_, err := c.exchange(cmdGetParam, []byte(strings.Join(parts, ",")))
	return err
}

// SyncTime sets the panel clock (DATETIME, encoded seconds-since-2000).
func (c *Client) SyncTime(ctx context.Context, t time.Time) error {
	enc := encodeC3Time(t.In(c.loc))
	payload := []byte("DateTime=" + strconv.FormatUint(uint64(enc), 10))
	_, err := c.exchange(cmdDateTime, payload)
	return err
}

// OpenDoor pulses a lock relay for the given number of seconds (ControlDevice
// OUTPUT operation). door is 1-based; seconds 1..254 (255 = normally-open).
func (c *Client) OpenDoor(ctx context.Context, door, seconds int) error {
	if door < 1 {
		return fmt.Errorf("c3: door must be >= 1")
	}
	if seconds < 1 {
		seconds = 1
	}
	if seconds > 254 {
		seconds = 254
	}
	// operation, param1=door, param2=address(1=door output), param3=duration
	payload := []byte{ctlOutput, byte(door), 1, byte(seconds), 0}
	_, err := c.exchange(cmdControl, payload)
	return err
}

// CloseDoor re-locks a door right away (ControlDevice OUTPUT with a 0 s
// duration), ending a pulse or a normally-open state the panel started.
func (c *Client) CloseDoor(ctx context.Context, door int) error {
	if door < 1 {
		return fmt.Errorf("c3: door must be >= 1")
	}
	payload := []byte{ctlOutput, byte(door), 1, 0, 0}
	_, err := c.exchange(cmdControl, payload)
	return err
}

// SetAux drives an auxiliary output on/off for a duration (0 = latch).
func (c *Client) SetAux(ctx context.Context, index int, on bool, seconds int) error {
	dur := 0
	if on {
		dur = seconds
		if dur < 1 {
			dur = 1
		}
	}
	payload := []byte{ctlOutput, byte(index), 2, byte(dur), 0} // address 2 = aux output
	_, err := c.exchange(cmdControl, payload)
	return err
}

// CancelAlarm clears an active alarm state (ControlDevice CANCEL_ALARM).
func (c *Client) CancelAlarm(ctx context.Context) error {
	payload := []byte{ctlCancelAlarm, 0, 0, 0, 0}
	_, err := c.exchange(cmdControl, payload)
	return err
}

// Restart reboots the panel (ControlDevice RESTART_DEVICE). The session drops;
// the supervisor reconnects automatically.
func (c *Client) Restart(ctx context.Context) error {
	payload := []byte{ctlRestart, 0, 0, 0, 0}
	_, err := c.exchange(cmdControl, payload)
	return err
}

// Status reads the current door/relay/alarm state from the panel's RTLog
// status record (sensor/relay/alarm key-value line).
func (c *Client) Status(ctx context.Context) (PanelStatus, error) {
	data, err := c.exchange(cmdRTLogKV, nil)
	if err != nil {
		return PanelStatus{}, err
	}
	var st PanelStatus
	st.Raw = string(data)
	for _, line := range strings.Split(string(data), "\r\n") {
		f := parseKV(strings.ReplaceAll(line, "\r", ""), "\t")
		if _, ok := f["sensor"]; ok {
			st.Sensor = f["sensor"]
			st.Relay = f["relay"]
			st.Alarm = f["alarm"]
			if t, e := time.ParseInLocation("2006-01-02 15:04:05", f["time"], c.loc); e == nil {
				st.Time = t
			}
			st.Doors = parseDoorStates(f["sensor"], f["relay"])
			return st, nil
		}
	}
	return st, nil
}

// parseDoorStates expands hex sensor/relay bytes into per-door state.
func parseDoorStates(sensorHex, relayHex string) []DoorState {
	sensor := hexByte(sensorHex)
	relay := hexByte(relayHex)
	var out []DoorState
	for d := 0; d < 4; d++ {
		bit := byte(1 << d)
		if sensor == 0 && relay == 0 && d >= 2 {
			continue // typical 2-door panel: don't invent doors 3/4
		}
		out = append(out, DoorState{
			Door:     d + 1,
			SensorOn: sensor&bit != 0,
			RelayOn:  relay&bit != 0,
		})
	}
	return out
}

func hexByte(s string) byte {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 16, 16)
	return byte(n)
}

// Events streams realtime access events by polling the key-value RTLog (0x79)
// and de-duplicating by the panel's monotonic record index.
func (c *Client) Events(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event, 128)
	go func() {
		defer close(out)
		t := time.NewTicker(c.pollInterval)
		defer t.Stop()

		// The 0x79 RTLog is a cycling rolling buffer: a single poll returns only
		// a partial window and old records reappear across polls. So we prime
		// across several polls to capture the true maximum index before emitting,
		// otherwise a reconnect would replay (and act on) stale events.
		lastIndex := -1
		primePolls := 5 // sample several polls to cover a full buffer cycle
		if c.replayBacklog {
			primePolls = 0 // replay/backfill mode: emit the buffered events
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			data, err := c.exchange(cmdRTLogKV, nil)
			if err != nil {
				return // drop stream; supervisor reconnects
			}
			evts, maxIdx := parseRTLogKV(string(data), c.loc)

			if primePolls > 0 {
				if maxIdx > lastIndex {
					lastIndex = maxIdx
				}
				primePolls--
				continue // still priming: absorb the backlog, emit nothing
			}
			for _, ev := range evts {
				if ev.index <= lastIndex {
					continue
				}
				lastIndex = ev.index
				select {
				case out <- ev.Event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// ---- card management (read/write of the panel data tables) ----
//
// Card enrollment writes to the panel's live user/authorization tables. The
// table field layout is queried via DATATABLE_CFG (0x06) then written via
// SETDATA. This is implemented in cards.go; kept separate to keep risky
// write-path code reviewable in isolation.

// ---- helpers ----

// parseKV parses "k=v<sep>k=v" into a map.
func parseKV(s, sep string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(s, sep) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

type indexedEvent struct {
	Event
	index int
}

// parseRTLogKV parses the tab-separated, CRLF-delimited key-value RTLog reply.
func parseRTLogKV(s string, loc *time.Location) (events []indexedEvent, maxIndex int) {
	maxIndex = -1
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := parseKV(line, "\t")
		if len(f) == 0 {
			continue
		}
		idx, _ := strconv.Atoi(f["index"])
		et, _ := strconv.Atoi(f["event"])
		verify, _ := strconv.Atoi(f["verifytype"])
		door, _ := strconv.Atoi(f["eventaddr"])
		ev := indexedEvent{index: idx, Event: Event{
			CardNo:    f["cardno"],
			PIN:       f["pin"],
			Door:      door,
			Reader:    door,
			EventType: et,
			EventName: EventName(et),
			Verify:    verify,
			InOut:     inOut(f["inoutstatus"]),
			Raw:       line,
		}}
		if ts, err := time.ParseInLocation("2006-01-02 15:04:05", f["time"], loc); err == nil {
			ev.Time = ts
		}
		events = append(events, ev)
		if idx > maxIndex {
			maxIndex = idx
		}
	}
	return events, maxIndex
}

func inOut(s string) string {
	switch s {
	case "0":
		return "in"
	case "1":
		return "out"
	default:
		return ""
	}
}

// unixOrZero returns the unix seconds of t, or 0 if t is the zero time.
func unixOrZero(t time.Time) int {
	if t.IsZero() {
		return 0
	}
	return int(t.Unix())
}

// encodeC3Time encodes a time as ZKTeco packed seconds-since-2000.
func encodeC3Time(t time.Time) uint32 {
	return uint32(((((t.Year()-2000)*12+int(t.Month())-1)*31+t.Day()-1)*24+t.Hour())*60*60 +
		t.Minute()*60 + t.Second())
}
