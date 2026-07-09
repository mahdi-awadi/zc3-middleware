// Package supervisor owns the lifecycle of a single panel connection: it keeps
// the panel connected, streams realtime events to a sink, and reconnects with
// exponential backoff + jitter when the link drops. The panel is single-session
// for control, so exactly one supervisor goroutine owns the connection.
package supervisor

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// EventSink receives normalized events (e.g. the API server's SSE fan-out).
type EventSink func(c3.Event)

type Config struct {
	ReconnectMin time.Duration
	ReconnectMax time.Duration
	Logger       *slog.Logger
}

type Supervisor struct {
	panel     c3.Panel
	sink      EventSink
	cfg       Config
	log       *slog.Logger
	connected atomic.Bool
}

func New(panel c3.Panel, sink EventSink, cfg Config) *Supervisor {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Supervisor{panel: panel, sink: sink, cfg: cfg, log: cfg.Logger}
}

// Connected reports whether the panel session is currently up (for readiness).
func (s *Supervisor) Connected() bool { return s.connected.Load() }

// Run blocks until ctx is cancelled, maintaining the connection and event stream.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.cfg.ReconnectMin
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		established, err := s.session(ctx)
		s.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		if established {
			backoff = s.cfg.ReconnectMin // healthy session: reset backoff
		}
		s.log.Warn("panel session ended, reconnecting", "err", err, "backoff", backoff.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(jitter(backoff)):
		}
		if backoff *= 2; backoff > s.cfg.ReconnectMax {
			backoff = s.cfg.ReconnectMax
		}
	}
}

// session connects and pumps events until an error or ctx cancellation. The
// bool reports whether the panel connection was successfully established.
func (s *Supervisor) session(ctx context.Context) (bool, error) {
	if err := s.panel.Connect(ctx); err != nil {
		return false, err
	}
	defer s.panel.Close()
	s.connected.Store(true)
	s.log.Info("panel connected")

	events, err := s.panel.Events(ctx)
	if err != nil {
		return true, err
	}
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return true, errStreamClosed
			}
			if s.sink != nil {
				s.sink(ev)
			}
		}
	}
}

// jitter returns d randomized in [d/2, d] to de-synchronize reconnect storms.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errStreamClosed sentinel = "event stream closed by panel"
