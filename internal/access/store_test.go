package access

import (
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("") // in-memory defaults
	if err != nil {
		t.Fatal(err)
	}
	// enable enforcement for tests
	cfg := s.Config()
	if err := s.SetGlobals(true, cfg.Holidays, cfg.ReaderDoor, false); err != nil {
		t.Fatal(err)
	}
	return s
}

// a Thursday (school day, Sun–Thu default) — 2026-07-09 is a Thursday.
func schoolDay(hh, mm int) time.Time {
	return time.Date(2026, 7, 9, hh, mm, 0, 0, time.UTC)
}

// 2026-07-10 is a Friday (weekend under Sun–Thu).
func weekend(hh, mm int) time.Time {
	return time.Date(2026, 7, 10, hh, mm, 0, 0, time.UTC)
}

func TestStudentEnterWindow(t *testing.T) {
	s := newStore(t)
	if d := s.Decide(1, DirIn, schoolDay(7, 15)); d.Action != Allow {
		t.Fatalf("student 07:15 in => %s (%s), want allow", d.Action, d.Reason)
	}
	if d := s.Decide(1, DirIn, schoolDay(8, 0)); d.Action != Deny {
		t.Fatalf("student 08:00 in => %s, want deny", d.Action)
	}
	// can't exit during the enter window
	if d := s.Decide(1, DirOut, schoolDay(7, 15)); d.Action != Deny {
		t.Fatalf("student 07:15 out => %s, want deny", d.Action)
	}
}

func TestStudentExitWindow(t *testing.T) {
	s := newStore(t)
	if d := s.Decide(1, DirOut, schoolDay(13, 45)); d.Action != Allow {
		t.Fatalf("student 13:45 out => %s (%s), want allow", d.Action, d.Reason)
	}
	if d := s.Decide(1, DirIn, schoolDay(13, 45)); d.Action != Deny {
		t.Fatalf("student 13:45 in => %s, want deny", d.Action)
	}
}

func TestWeekendBlocked(t *testing.T) {
	s := newStore(t)
	if d := s.Decide(1, DirIn, weekend(7, 15)); d.Action != Deny || d.Reason != "day not permitted" {
		t.Fatalf("student weekend => %s/%s, want deny/day not permitted", d.Action, d.Reason)
	}
}

func TestHolidayBlocked(t *testing.T) {
	s := newStore(t)
	s.SetGlobals(true, []string{"2026-07-09"}, nil, false)
	if d := s.Decide(1, DirIn, schoolDay(7, 15)); d.Action != Deny || d.Reason != "holiday" {
		t.Fatalf("student holiday => %s/%s, want deny/holiday", d.Action, d.Reason)
	}
}

func TestTeacherWindow(t *testing.T) {
	s := newStore(t)
	if d := s.Decide(2, DirIn, schoolDay(9, 0)); d.Action != Allow {
		t.Fatalf("teacher 09:00 => %s, want allow", d.Action)
	}
	if d := s.Decide(2, DirIn, schoolDay(18, 0)); d.Action != Deny {
		t.Fatalf("teacher 18:00 => %s, want deny", d.Action)
	}
}

func TestAdminNotEnforced(t *testing.T) {
	s := newStore(t)
	// administrator role has enforce=false -> engine ignores (panel decides)
	if d := s.Decide(3, DirIn, schoolDay(3, 0)); d.Action != Ignore {
		t.Fatalf("admin => %s, want ignore", d.Action)
	}
}

func TestDisabledMasterSwitch(t *testing.T) {
	s, _ := Open("")
	// enforcement off by default => everything ignored
	if d := s.Decide(1, DirIn, schoolDay(8, 0)); d.Action != Ignore {
		t.Fatalf("master off => %s, want ignore", d.Action)
	}
}

func TestUpsertAndDelete(t *testing.T) {
	s := newStore(t)
	if err := s.Upsert(Role{Group: 5, Name: "guard", Enforce: true}); err != nil {
		t.Fatal(err)
	}
	if s.RoleName(5) != "guard" {
		t.Fatalf("upsert role name = %q", s.RoleName(5))
	}
	if g, ok := s.GroupForName("GUARD"); !ok || g != 5 {
		t.Fatalf("GroupForName = %d,%v", g, ok)
	}
	if err := s.Delete(5); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GroupForName("guard"); ok {
		t.Fatal("role not deleted")
	}
	if err := s.Delete(0); err == nil {
		t.Fatal("deleting default role should error")
	}
}

func TestParseHM(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{{"07:30", 450, true}, {"00:00", 0, true}, {"23:59", 1439, true}, {"7:5", 425, true}, {"bad", 0, false}, {"25:00", 0, false}} {
		got, ok := parseHM(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseHM(%q)=%d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}
