package gym

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// DoorOpener is the panel capability the engine needs.
type DoorOpener interface {
	OpenDoor(ctx context.Context, door, seconds int) error
}

// PanelGuard is implemented by panel clients that can undo a door the panel
// opened on its own (c3.Client does). The engine uses it when the panel lets a
// card through that the gym rules refuse.
type PanelGuard interface {
	CloseDoor(ctx context.Context, door int) error
	RevokeAuthorization(ctx context.Context, pin string) error
}

// PanelOpened reports whether a C3 access event means the panel itself
// unlocked the door for a card (as opposed to refusing or just logging it).
func PanelOpened(eventType int) bool {
	switch eventType {
	case 0, 1, 2, 3, 14, 16, 17, 18, 19, 203:
		return true
	}
	return false
}

// scanTTL is how long a captured swipe stays claimable by the link-card flow.
const scanTTL = 30 * time.Second

// Scan is the last swipe seen at any reader (used by the card-link flow).
type Scan struct {
	CardNo   string    `json:"card_no"`
	Time     time.Time `json:"time"`
	Known    bool      `json:"known"` // already linked to a member
	Member   string    `json:"member,omitempty"`
	MemberID string    `json:"member_id,omitempty"`
}

// Engine turns swipe events into gate decisions: it opens the door for valid
// members, records every outcome, and fans GateUpdates out to subscribers
// (the gate kiosk screen + dashboard live feed).
type Engine struct {
	store    *Store
	opener   DoorOpener
	log      *slog.Logger
	timeout  time.Duration
	openSecs int // fallback relay pulse (server door default)

	mu       sync.Mutex
	subs     map[chan GateUpdate]struct{}
	lastScan *Scan

	act chan actionReq // queued door-opens (non-blocking handoff)
}

type actionReq struct {
	lock          bool   // re-lock the door and revoke pin's panel access instead of opening
	pin           string // panel PIN of the refused card (lock only)
	door, seconds int
}

func NewEngine(store *Store, opener DoorOpener, openSecs int, timeout time.Duration, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if openSecs <= 0 {
		openSecs = 5
	}
	return &Engine{
		store: store, opener: opener, log: log, timeout: timeout, openSecs: openSecs,
		subs: map[chan GateUpdate]struct{}{}, act: make(chan actionReq, 64),
	}
}

// Run services queued door-open actions until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-e.act:
			cctx, cancel := context.WithTimeout(ctx, e.timeout)
			if a.lock {
				e.enforceDenied(cctx, a)
			} else if err := e.opener.OpenDoor(cctx, a.door, a.seconds); err != nil {
				e.log.Warn("gym: door open failed", "door", a.door, "err", err)
			}
			cancel()
		}
	}
}

// enforceDenied undoes a door the panel opened for a refused card: it re-locks
// the door at once and removes the card's panel authorization, so the panel
// stops letting that card in by itself on the next swipe.
func (e *Engine) enforceDenied(ctx context.Context, a actionReq) {
	g, ok := e.opener.(PanelGuard)
	if !ok {
		return
	}
	if err := g.CloseDoor(ctx, a.door); err != nil {
		e.log.Warn("gym: re-lock after refused card failed", "door", a.door, "err", err)
	}
	if a.pin == "" {
		return
	}
	if err := g.RevokeAuthorization(ctx, a.pin); err != nil {
		e.log.Warn("gym: revoking panel access of refused card failed", "pin", a.pin, "err", err)
		return
	}
	e.log.Warn("gym: panel opened for a refused card — door re-locked and its panel access removed", "pin", a.pin, "door", a.door)
}

// Subscribe registers a GateUpdate channel; the caller must Unsubscribe.
func (e *Engine) Subscribe(buf int) chan GateUpdate {
	if buf <= 0 {
		buf = 16
	}
	ch := make(chan GateUpdate, buf)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch
}

func (e *Engine) Unsubscribe(ch chan GateUpdate) {
	e.mu.Lock()
	delete(e.subs, ch)
	e.mu.Unlock()
}

func (e *Engine) broadcast(u GateUpdate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- u:
		default: // never block the event loop on a slow kiosk
		}
	}
}

// LastScan returns the most recent swipe if it is still fresh.
func (e *Engine) LastScan() *Scan {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastScan == nil || time.Since(e.lastScan.Time) > scanTTL {
		return nil
	}
	sc := *e.lastScan
	return &sc
}

// CaptureScan records a card as the last swipe for the link flow without
// running a gate decision or touching history (desk enrollment / demo).
func (e *Engine) CaptureScan(cardNo string) Scan {
	sc := Scan{CardNo: NormalizeCard(cardNo), Time: time.Now()}
	if mv := e.store.MemberByCard(cardNo); mv != nil {
		sc.Known, sc.Member, sc.MemberID = true, mv.Name, mv.ID
	}
	e.mu.Lock()
	e.lastScan = &sc
	e.mu.Unlock()
	return sc
}

// ClearScan drops the captured swipe (after the link flow consumes it).
func (e *Engine) ClearScan() {
	e.mu.Lock()
	e.lastScan = nil
	e.mu.Unlock()
}

// initials builds a compact Arabic-friendly "أ.ج" style monogram.
func initials(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "—"
	}
	first := []rune(parts[0])
	out := string(first[0])
	if len(parts) > 1 {
		last := []rune(parts[len(parts)-1])
		out += "." + string(last[0])
	}
	return out
}

// Handle processes one panel event. Only card swipes participate; door/aux
// state events pass through untouched.
func (e *Engine) Handle(ev c3.Event) {
	if ev.CardNo == "" || ev.CardNo == "0" {
		return
	}
	pin := ev.PIN
	if pin == "" || pin == "0" {
		pin = ev.CardNo
	}
	e.process(ev.CardNo, ev.Door, ev.Reader, ev.InOut, swipeTime(ev.Time, time.Now()), false, PanelOpened(ev.EventType), pin)
}

// swipeTime trusts the panel's event time unless its clock is clearly wrong
// (in the future, or more than 30 days old — a reset clock reports 2020), in
// which case the swipe is recorded at the moment the server received it.
func swipeTime(panel, now time.Time) time.Time {
	if panel.IsZero() || panel.After(now.Add(time.Hour)) || panel.Before(now.AddDate(0, 0, -30)) {
		return now
	}
	return panel
}

// Simulate injects a swipe as if the panel reported it (dev / demo / desk
// reader). It runs the full decision path but never opens a door when
// dryRun is requested by the caller via openDoors=false.
func (e *Engine) Simulate(cardNo string, door int, openDoors bool) GateUpdate {
	if door <= 0 {
		door = 1
	}
	return e.process(cardNo, door, door, "in", time.Now(), !openDoors, false, "")
}

// panelOpened says the panel already unlocked the door for this swipe; pin is
// the card's panel PIN (used to revoke its panel access when refused).
func (e *Engine) process(cardNo string, door, reader int, dir string, t time.Time, dryRun, panelOpened bool, pin string) GateUpdate {
	if t.IsZero() {
		t = time.Now()
	}
	var dec Decision
	var mv *MemberView
	if dir == "out" {
		dec, mv = e.store.DecideExit(cardNo, t)
	} else {
		dec, mv = e.store.Decide(cardNo, door, t)
	}

	// Capture for the link-card flow regardless of outcome.
	sc := Scan{CardNo: NormalizeCard(cardNo), Time: time.Now(), Known: mv != nil && SameCard(mv.CardNo, cardNo)}
	if sc.Known {
		sc.Member, sc.MemberID = mv.Name, mv.ID
	}
	e.mu.Lock()
	e.lastScan = &sc
	e.mu.Unlock()

	ck := Checkin{
		Time: t, CardNo: cardNo, Door: door, Reader: reader, Direction: dir,
		Result: dec.Result, Reason: dec.Reason,
	}
	if mv != nil {
		ck.MemberID = mv.ID
		ck.Name = mv.Name
	}
	var err error
	if ck, err = e.store.RecordCheckin(ck); err != nil {
		e.log.Error("gym: check-in not persisted", "card", cardNo, "err", err)
	}

	if !dec.Open && panelOpened && !dryRun && dir != "out" { // never lock anyone in
		// Refused here but the panel unlocked anyway (a leftover panel access
		// rule, e.g. from ZKAccess): lock it again and remove that rule.
		lockDoor := door
		if lockDoor <= 0 {
			lockDoor = 1
		}
		select {
		case e.act <- actionReq{door: lockDoor, lock: true, pin: pin}:
		default:
			e.log.Warn("gym: door action queue full, cannot re-lock", "card", cardNo)
		}
	}
	if dec.Open && !dryRun {
		secs := e.openSecs
		if c := e.store.Config(); c.OpenSeconds > 0 {
			secs = c.OpenSeconds
		}
		if door <= 0 {
			door = 1
		}
		select {
		case e.act <- actionReq{door: door, seconds: secs}:
		default:
			e.log.Warn("gym: door action queue full, dropping", "card", cardNo)
		}
	}

	u := GateUpdate{Checkin: ck}
	if mv != nil {
		u.Status = mv.Status
		u.Plan = mv.Plan
		u.DaysLeft = dec.DaysLeft
		u.Initials = initials(mv.Name)
		if sub := currentSub(&mv.Member); sub != nil && (mv.Status == StatusExpired || mv.Status == StatusNone) {
			u.ExpiredAt = fmtDate(sub.End)
		}
	} else {
		u.Initials = "—"
	}
	e.log.Info("gym: gate decision", "card", cardNo, "result", dec.Result, "reason", dec.Reason)
	e.broadcast(u)
	return u
}
