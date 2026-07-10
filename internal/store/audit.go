// Package store provides a durable, dependency-free audit log of access events:
// an append-only JSONL file for permanence plus an in-memory ring buffer for
// fast history queries. No external database — matches the single-binary deploy.
package store

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// Filter narrows a history query. Zero values mean "no constraint".
type Filter struct {
	CardNo string
	Door   int
	Limit  int
	Offset int
}

// Store is an append-only event audit log.
type Store struct {
	mu   sync.RWMutex
	ring []c3.Event
	cap  int
	f    *os.File
	w    *bufio.Writer
}

// Open creates a store. If path is non-empty, events are also appended to that
// JSONL file (created if missing). ringSize bounds the in-memory query buffer.
func Open(path string, ringSize int) (*Store, error) {
	if ringSize <= 0 {
		ringSize = 10000
	}
	s := &Store{cap: ringSize, ring: make([]c3.Event, 0, ringSize)}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, err
		}
		s.f = f
		s.w = bufio.NewWriter(f)
		s.loadTail(path)
	}
	return s, nil
}

// loadTail preloads the ring from the existing file so history survives restart.
func (s *Store) loadTail(path string) {
	rf, err := os.Open(path)
	if err != nil {
		return
	}
	defer rf.Close()
	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var ev c3.Event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			s.appendRing(ev)
		}
	}
}

// Append records an event durably and in the ring.
func (s *Store) Append(ev c3.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendRing(ev)
	if s.w != nil {
		if b, err := json.Marshal(ev); err == nil {
			s.w.Write(b)
			s.w.WriteByte('\n')
			s.w.Flush()
		}
	}
}

func (s *Store) appendRing(ev c3.Event) {
	if len(s.ring) >= s.cap {
		copy(s.ring, s.ring[1:])
		s.ring[len(s.ring)-1] = ev
		return
	}
	s.ring = append(s.ring, ev)
}

// Query returns matching events newest-first, honoring the filter's paging.
func (s *Store) Query(f Filter) []c3.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []c3.Event
	skipped := 0
	for i := len(s.ring) - 1; i >= 0; i-- {
		ev := s.ring[i]
		if f.CardNo != "" && ev.CardNo != f.CardNo {
			continue
		}
		if f.Door != 0 && ev.Door != f.Door {
			continue
		}
		if skipped < f.Offset {
			skipped++
			continue
		}
		out = append(out, ev)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Count returns the number of events currently held in the ring.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.ring)
}

// Close flushes and closes the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		s.w.Flush()
	}
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}
