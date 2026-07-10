package access

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

type recOpener struct {
	mu    sync.Mutex
	opens []int
}

func (r *recOpener) OpenDoor(_ context.Context, door, _ int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens = append(r.opens, door)
	return nil
}
func (r *recOpener) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.opens) }

func run(t *testing.T, pol Policy, evs ...c3.Event) *recOpener {
	t.Helper()
	op := &recOpener{}
	rx := New(op, pol, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rx.Run(ctx)
	for _, e := range evs {
		rx.Handle(e)
	}
	// allow the worker to drain
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return op
}

func TestReactorDisabled(t *testing.T) {
	op := run(t, Policy{Enabled: false, AllowAny: true}, c3.Event{CardNo: "1", Reader: 1})
	if op.count() != 0 {
		t.Fatalf("disabled reactor opened %d", op.count())
	}
}

func TestReactorAllowlist(t *testing.T) {
	pol := Policy{Enabled: true, Allowed: map[string]bool{"212532046": true}, Seconds: 3}
	op := run(t, pol,
		c3.Event{CardNo: "999", Reader: 1},       // not allowed
		c3.Event{CardNo: "212532046", Reader: 2}, // allowed -> door 2
	)
	if op.count() != 1 || op.opens[0] != 2 {
		t.Fatalf("opens=%v want [2]", op.opens)
	}
}

func TestReactorReaderDoorMap(t *testing.T) {
	pol := Policy{Enabled: true, AllowAny: true, ReaderDoor: map[int]int{3: 1, 4: 2}, Seconds: 3}
	op := run(t, pol, c3.Event{CardNo: "5", Reader: 3})
	if op.count() != 1 || op.opens[0] != 1 {
		t.Fatalf("mapped opens=%v want [1]", op.opens)
	}
}

func TestReactorIgnoresNonCardEvents(t *testing.T) {
	pol := Policy{Enabled: true, AllowAny: true, Seconds: 3}
	op := run(t, pol, c3.Event{CardNo: "0", Reader: 1}, c3.Event{CardNo: "", Reader: 1})
	if op.count() != 0 {
		t.Fatalf("opened on non-card events: %v", op.opens)
	}
}
