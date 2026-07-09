// Package access implements optional middleware-driven access reactions: when a
// card is read at a reader, open the mapped door. This deliberately places the
// middleware in the unlock path (normally the panel decides locally), so it is
// off by default and gated by an explicit card allowlist.
package access

import (
	"context"
	"log/slog"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// DoorOpener is the panel capability the reactor needs.
type DoorOpener interface {
	OpenDoor(ctx context.Context, door, seconds int) error
}

// Policy configures the auto-open behavior.
type Policy struct {
	Enabled    bool
	Seconds    int          // pulse duration
	AllowAny   bool         // true => any card opens (test mode)
	Allowed    map[string]bool
	ReaderDoor map[int]int  // reader/eventaddr -> door; empty => identity
	Timeout    time.Duration
}

// Reactor consumes access events and opens doors per Policy. Actions run on a
// worker goroutine so panel I/O never blocks the event pump.
type Reactor struct {
	opener DoorOpener
	pol    Policy
	log    *slog.Logger
	ch     chan action
}

type action struct {
	door, seconds int
	card          string
	reader        int
}

func New(opener DoorOpener, pol Policy, log *slog.Logger) *Reactor {
	if pol.Timeout <= 0 {
		pol.Timeout = 5 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reactor{opener: opener, pol: pol, log: log, ch: make(chan action, 32)}
}

// Enabled reports whether the reactor will act on events.
func (r *Reactor) Enabled() bool { return r.pol.Enabled }

// Run processes queued open actions until ctx is cancelled.
func (r *Reactor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-r.ch:
			c, cancel := context.WithTimeout(ctx, r.pol.Timeout)
			err := r.opener.OpenDoor(c, a.door, a.seconds)
			cancel()
			if err != nil {
				r.log.Error("auto-open failed", "door", a.door, "card", a.card, "err", err)
			} else {
				r.log.Info("auto-open", "door", a.door, "reader", a.reader, "card", a.card, "seconds", a.seconds)
			}
		}
	}
}

// Handle inspects an event and enqueues a door-open if policy allows. It never
// blocks: if the worker is saturated the action is dropped with a warning.
func (r *Reactor) Handle(ev c3.Event) {
	if !r.pol.Enabled {
		return
	}
	// Only react to genuine card reads (a card number present).
	if ev.CardNo == "" || ev.CardNo == "0" {
		return
	}
	if !r.pol.AllowAny && !r.pol.Allowed[ev.CardNo] {
		r.log.Debug("auto-open skipped: card not allowed", "card", ev.CardNo, "reader", ev.Reader)
		return
	}
	door := ev.Reader
	if r.pol.ReaderDoor != nil {
		if d, ok := r.pol.ReaderDoor[ev.Reader]; ok {
			door = d
		}
	}
	if door < 1 {
		return
	}
	select {
	case r.ch <- action{door: door, seconds: r.pol.Seconds, card: ev.CardNo, reader: ev.Reader}:
	default:
		r.log.Warn("auto-open dropped: worker saturated", "card", ev.CardNo, "door", door)
	}
}
