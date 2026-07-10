// Package access implements user-defined roles and the enforcement engine that
// applies each role's time / direction / holiday rules to live swipe events and
// opens the door when access is permitted.
//
// Roles are identified by the panel's user "Group" field, so a card's role is
// stored natively on the device; the rules for each role live here (persisted
// as JSON) and are evaluated by the middleware.
package access

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Direction of a swipe.
const (
	DirIn  = "in"
	DirOut = "out"
	DirAny = "any"
)

// Window is one permitted time span for a direction. From/To are "HH:MM".
type Window struct {
	Direction string `json:"direction"` // in | out | any
	From      string `json:"from"`      // "07:00"
	To        string `json:"to"`        // "07:30"
}

// Role is a named access policy bound to a panel Group id.
type Role struct {
	Group         int      `json:"group"`          // panel Group field (role id); 0 = none
	Name          string   `json:"name"`           // e.g. "student"
	Enforce       bool     `json:"enforce"`        // true = middleware gates access by these rules
	Days          []int    `json:"days"`           // allowed weekdays 0=Sun..6=Sat; empty = all
	BlockHolidays bool     `json:"block_holidays"` // deny on configured holidays
	OpenSeconds   int      `json:"open_seconds"`   // relay pulse when allowed (0 = default)
	Windows       []Window `json:"windows"`        // permitted spans; empty = any time (on allowed days)
}

// Config is the whole access policy: roles + globals, persisted as one file.
type Config struct {
	Enabled    bool        `json:"enabled"`     // master switch for enforcement
	Holidays   []string    `json:"holidays"`    // "2026-07-14"
	ReaderDoor map[int]int `json:"reader_door"` // reader/eventaddr -> door to open; empty = use event door
	SwapInOut  bool        `json:"swap_in_out"` // flip the panel's in/out interpretation
	Roles      []Role      `json:"roles"`
}

// Store holds the access config with safe concurrent access + JSON persistence.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Action is the engine's decision for an event.
type Action string

const (
	Allow  Action = "allow"  // enforced role, rules satisfied -> open the door
	Deny   Action = "deny"   // enforced role, rules not satisfied -> keep closed
	Ignore Action = "ignore" // not enforced (panel decides) or unknown card
)

// Decision is the outcome of evaluating one event.
type Decision struct {
	Action Action `json:"action"`
	Reason string `json:"reason"`
	Role   string `json:"role"`
	Open   int    `json:"open_seconds"`
}

// Open loads the config from path, seeding sensible defaults if absent.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		s.cfg = defaultConfig()
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.cfg = defaultConfig()
			return s, s.save()
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, fmt.Errorf("access: parse %s: %w", path, err)
	}
	if s.cfg.ReaderDoor == nil {
		s.cfg.ReaderDoor = map[int]int{}
	}
	return s, nil
}

// defaultConfig seeds the school roles described by the operator. Enforcement is
// OFF by default so nothing changes until explicitly enabled.
func defaultConfig() Config {
	return Config{
		Enabled:    false,
		Holidays:   []string{},
		ReaderDoor: map[int]int{},
		Roles: []Role{
			{Group: 0, Name: "none", Enforce: false},
			{Group: 1, Name: "student", Enforce: true, BlockHolidays: true, OpenSeconds: 5,
				Days: []int{0, 1, 2, 3, 4}, // Sun–Thu
				Windows: []Window{
					{Direction: DirIn, From: "07:00", To: "07:30"},
					{Direction: DirOut, From: "13:30", To: "14:00"},
				}},
			{Group: 2, Name: "teacher", Enforce: true, BlockHolidays: true, OpenSeconds: 5,
				Days:    []int{0, 1, 2, 3, 4},
				Windows: []Window{{Direction: DirAny, From: "07:00", To: "17:00"}}},
			{Group: 3, Name: "administrator", Enforce: false, OpenSeconds: 5,
				Windows: []Window{{Direction: DirAny, From: "00:00", To: "23:59"}}},
		},
	}
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(s.cfg, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- accessors ----

// Config returns a copy of the current config.
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetGlobals updates the non-role fields (enforcement switch, holidays, maps).
func (s *Store) SetGlobals(enabled bool, holidays []string, readerDoor map[int]int, swap bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Enabled = enabled
	s.cfg.Holidays = holidays
	if readerDoor != nil {
		s.cfg.ReaderDoor = readerDoor
	}
	s.cfg.SwapInOut = swap
	return s.save()
}

// Roles returns the role list (sorted by group).
func (s *Store) Roles() []Role {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]Role(nil), s.cfg.Roles...)
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// Upsert creates or replaces a role by group id.
func (s *Store) Upsert(r Role) error {
	if r.Name == "" {
		return fmt.Errorf("access: role name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Roles {
		if s.cfg.Roles[i].Group == r.Group {
			s.cfg.Roles[i] = r
			return s.save()
		}
	}
	s.cfg.Roles = append(s.cfg.Roles, r)
	return s.save()
}

// Delete removes a role by group id (group 0 "none" is protected).
func (s *Store) Delete(group int) error {
	if group == 0 {
		return fmt.Errorf("access: cannot delete the default role")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.cfg.Roles[:0]
	for _, r := range s.cfg.Roles {
		if r.Group != group {
			out = append(out, r)
		}
	}
	s.cfg.Roles = out
	return s.save()
}

// RoleName returns the role name for a group id, or "group<N>" if unknown.
func (s *Store) RoleName(group int) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.cfg.Roles {
		if r.Group == group {
			return r.Name
		}
	}
	return fmt.Sprintf("group%d", group)
}

// GroupForName resolves a role name (case-insensitive) to its group id.
func (s *Store) GroupForName(name string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.cfg.Roles {
		if strings.EqualFold(r.Name, name) {
			return r.Group, true
		}
	}
	return 0, false
}

// EnforcedGroup reports whether a group's role has enforcement on.
func (s *Store) EnforcedGroup(group int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.cfg.Roles {
		if r.Group == group {
			return s.cfg.Enabled && r.Enforce
		}
	}
	return false
}

// Decide evaluates an event for a group's role at direction/time.
func (s *Store) Decide(group int, direction string, t time.Time) Decision {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var role *Role
	for i := range s.cfg.Roles {
		if s.cfg.Roles[i].Group == group {
			role = &s.cfg.Roles[i]
			break
		}
	}
	if !s.cfg.Enabled || role == nil || !role.Enforce {
		return Decision{Action: Ignore, Reason: "not enforced", Role: roleName(role)}
	}
	name := role.Name
	open := role.OpenSeconds

	// Holiday check.
	if role.BlockHolidays {
		day := t.Format("2006-01-02")
		for _, h := range s.cfg.Holidays {
			if strings.TrimSpace(h) == day {
				return Decision{Action: Deny, Reason: "holiday", Role: name}
			}
		}
	}
	// Weekday check.
	if len(role.Days) > 0 {
		wd := int(t.Weekday())
		ok := false
		for _, d := range role.Days {
			if d == wd {
				ok = true
				break
			}
		}
		if !ok {
			return Decision{Action: Deny, Reason: "day not permitted", Role: name}
		}
	}
	// Window check. No windows => allowed at any time on permitted days.
	if len(role.Windows) == 0 {
		return Decision{Action: Allow, Reason: "no time restriction", Role: name, Open: open}
	}
	nowMin := t.Hour()*60 + t.Minute()
	for _, w := range role.Windows {
		if w.Direction != DirAny && direction != "" && !strings.EqualFold(w.Direction, direction) {
			continue
		}
		from, ok1 := parseHM(w.From)
		to, ok2 := parseHM(w.To)
		if !ok1 || !ok2 {
			continue
		}
		if nowMin >= from && nowMin <= to {
			return Decision{Action: Allow, Reason: fmt.Sprintf("%s %s–%s", w.Direction, w.From, w.To), Role: name, Open: open}
		}
	}
	return Decision{Action: Deny, Reason: "outside permitted window", Role: name}
}

func roleName(r *Role) string {
	if r == nil {
		return ""
	}
	return r.Name
}

// parseHM parses "HH:MM" into minutes-since-midnight.
func parseHM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	hh, e1 := atoiSafe(h)
	mm, e2 := atoiSafe(m)
	if e1 != nil || e2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

func atoiSafe(s string) (int, error) {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("nan")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
