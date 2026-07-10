package store

import (
	"path/filepath"
	"testing"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

func TestAppendQueryFilter(t *testing.T) {
	s, err := Open("", 100)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(c3.Event{CardNo: "111", Door: 1})
	s.Append(c3.Event{CardNo: "222", Door: 2})
	s.Append(c3.Event{CardNo: "111", Door: 2})

	if n := s.Count(); n != 3 {
		t.Fatalf("count=%d", n)
	}
	// newest-first
	all := s.Query(Filter{})
	if len(all) != 3 || all[0].CardNo != "111" || all[0].Door != 2 {
		t.Fatalf("query order wrong: %+v", all)
	}
	// filter by card
	byCard := s.Query(Filter{CardNo: "111"})
	if len(byCard) != 2 {
		t.Fatalf("filter card=111 got %d", len(byCard))
	}
	// filter by door
	byDoor := s.Query(Filter{Door: 2})
	if len(byDoor) != 2 {
		t.Fatalf("filter door=2 got %d", len(byDoor))
	}
	// limit
	if got := s.Query(Filter{Limit: 1}); len(got) != 1 {
		t.Fatalf("limit=1 got %d", len(got))
	}
}

func TestRingEviction(t *testing.T) {
	s, _ := Open("", 2)
	for i := 0; i < 5; i++ {
		s.Append(c3.Event{CardNo: "c"})
	}
	if s.Count() != 2 {
		t.Fatalf("ring cap not enforced: %d", s.Count())
	}
}

func TestFilePersistenceAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	s, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	s.Append(c3.Event{CardNo: "abc", Door: 1})
	s.Close()

	// reopen: history should reload from file
	s2, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.Count() != 1 {
		t.Fatalf("reload count=%d want 1", s2.Count())
	}
	if got := s2.Query(Filter{}); len(got) != 1 || got[0].CardNo != "abc" {
		t.Fatalf("reload query=%+v", got)
	}
}
