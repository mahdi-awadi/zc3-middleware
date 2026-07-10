package access

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// DoorOpener is the panel capability the engine needs.
type DoorOpener interface {
	OpenDoor(ctx context.Context, door, seconds int) error
}

// UserLister supplies card -> group resolution (the panel user roster).
type UserLister interface {
	ListUsers(ctx context.Context) ([]c3.User, error)
}

// DecisionRecord is a logged enforcement outcome for the UI/audit.
type DecisionRecord struct {
	Time      time.Time `json:"time"`
	CardNo    string    `json:"card_no"`
	Name      string    `json:"name"`
	Reader    int       `json:"reader"`
	Door      int       `json:"door"`
	Direction string    `json:"direction"`
	Role      string    `json:"role"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`

	openSeconds int // relay pulse to use when Action==allow
}

// Engine applies role rules to live events and opens doors when permitted.
type Engine struct {
	store   *Store
	opener  DoorOpener
	users   UserLister
	log     *slog.Logger
	timeout time.Duration

	mu       sync.RWMutex
	cardMeta map[string]userMeta // card_no -> {group,name}
	recent   []DecisionRecord

	act chan DecisionRecord // queued door-opens (non-blocking handoff)
}

type userMeta struct {
	group int
	name  string
}

func NewEngine(store *Store, opener DoorOpener, users UserLister, timeout time.Duration, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Engine{
		store: store, opener: opener, users: users, log: log, timeout: timeout,
		cardMeta: map[string]userMeta{}, act: make(chan DecisionRecord, 64),
	}
}

// Run refreshes the user cache periodically and services door-open actions.
func (e *Engine) Run(ctx context.Context) {
	e.refresh(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.refresh(ctx)
		case a := <-e.act:
			c, cancel := context.WithTimeout(ctx, e.timeout)
			err := e.opener.OpenDoor(c, a.Door, a.reasonOpenSeconds())
			cancel()
			if err != nil {
				e.log.Error("enforce open failed", "door", a.Door, "card", a.CardNo, "err", err)
			} else {
				e.log.Info("enforce open", "door", a.Door, "card", a.CardNo, "role", a.Role, "dir", a.Direction, "reason", a.Reason)
			}
		}
	}
}

func (a DecisionRecord) reasonOpenSeconds() int { return a.openSeconds }

// Refresh reloads the card->group cache from the panel (call after enroll/revoke).
func (e *Engine) Refresh(ctx context.Context) { e.refresh(ctx) }

func (e *Engine) refresh(ctx context.Context) {
	c, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	users, err := e.users.ListUsers(c)
	if err != nil {
		e.log.Debug("engine: user refresh failed", "err", err)
		return
	}
	m := make(map[string]userMeta, len(users))
	for _, u := range users {
		if u.CardNo != "" && u.CardNo != "0" {
			m[u.CardNo] = userMeta{group: u.Group, name: u.Name}
		}
	}
	e.mu.Lock()
	e.cardMeta = m
	e.mu.Unlock()
}

// Handle evaluates one event and enqueues a door-open when access is permitted.
func (e *Engine) Handle(ev c3.Event) {
	if ev.CardNo == "" || ev.CardNo == "0" {
		return
	}
	e.mu.RLock()
	meta, known := e.cardMeta[ev.CardNo]
	e.mu.RUnlock()
	if !known {
		return // unknown card: panel already handled it
	}

	cfg := e.store.Config()
	dir := ev.InOut
	if cfg.SwapInOut {
		dir = flip(dir)
	}
	dec := e.store.Decide(meta.group, dir, ev.Time)
	if dec.Action == Ignore {
		return // role not enforced: panel decides
	}

	door := e.resolveDoor(cfg, ev)
	rec := DecisionRecord{
		Time: ev.Time, CardNo: ev.CardNo, Name: meta.name, Reader: ev.Reader,
		Door: door, Direction: dir, Role: dec.Role, Action: string(dec.Action), Reason: dec.Reason,
		openSeconds: dec.Open,
	}
	e.record(rec)

	if dec.Action == Allow {
		select {
		case e.act <- rec:
		default:
			e.log.Warn("enforce queue full; open dropped", "card", ev.CardNo)
		}
	} else {
		e.log.Info("enforce deny", "card", ev.CardNo, "role", dec.Role, "dir", dir, "reason", dec.Reason)
	}
}

func (e *Engine) resolveDoor(cfg Config, ev c3.Event) int {
	if d, ok := cfg.ReaderDoor[ev.Reader]; ok && d > 0 {
		return d
	}
	if ev.Door > 0 {
		return ev.Door
	}
	return 1
}

func (e *Engine) record(r DecisionRecord) {
	e.mu.Lock()
	e.recent = append(e.recent, r)
	if len(e.recent) > 200 {
		e.recent = e.recent[len(e.recent)-200:]
	}
	e.mu.Unlock()
}

// Decisions returns the most recent enforcement decisions, newest first.
func (e *Engine) Decisions(limit int) []DecisionRecord {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if limit <= 0 || limit > len(e.recent) {
		limit = len(e.recent)
	}
	out := make([]DecisionRecord, 0, limit)
	for i := len(e.recent) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, e.recent[i])
	}
	return out
}

func flip(d string) string {
	switch d {
	case DirIn:
		return DirOut
	case DirOut:
		return DirIn
	}
	return d
}
