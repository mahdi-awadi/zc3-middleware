package gym

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---- gates registry ----

// Gates lists configured gates.
func (s *Store) Gates() []Gate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Gate, 0, len(s.d.Gates))
	for _, g := range s.d.Gates {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) gateByID(id string) *Gate {
	for _, g := range s.d.Gates {
		if g.ID == id {
			return g
		}
	}
	return nil
}

// Gate returns one gate.
func (s *Store) Gate(id string) (Gate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.gateByID(id)
	if g == nil {
		return Gate{}, ErrNotFound
	}
	return *g, nil
}

func validGate(addr string, door int) error {
	addr = strings.TrimSpace(addr)
	if addr == "" || !strings.Contains(addr, ":") {
		return errors.New("gym: gate addr must be host:port")
	}
	if door < 1 || door > 4 {
		return errors.New("gym: gate door must be 1..4")
	}
	return nil
}

// AddGate registers a gate (from the LAN scan or manual entry).
func (s *Store) AddGate(g Gate, now time.Time) (Gate, error) {
	g.Addr = strings.TrimSpace(g.Addr)
	if err := validGate(g.Addr, g.Door); err != nil {
		return Gate{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.d.Gates {
		if e.Addr == g.Addr && e.Door == g.Door {
			return Gate{}, fmt.Errorf("gym: gate for %s door %d already exists (%s)", g.Addr, g.Door, e.ID)
		}
	}
	if s.d.NextGateID == 0 {
		s.d.NextGateID = 1
	}
	g.ID = fmt.Sprintf("gate-%d", s.d.NextGateID)
	s.d.NextGateID++
	if strings.TrimSpace(g.Name) == "" {
		g.Name = "بوابة " + g.ID[len("gate-"):]
	}
	g.AddedAt = now
	gg := g
	s.d.Gates = append(s.d.Gates, &gg)
	// First gate becomes the active connection target automatically.
	if s.d.Config.ActiveGate == "" && len(s.d.Gates) == 1 {
		s.d.Config.ActiveGate = gg.ID
	}
	if err := s.save(); err != nil {
		return Gate{}, err
	}
	return gg, nil
}

// UpdateGate patches name / door / addr.
func (s *Store) UpdateGate(id string, patch Gate) (Gate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gateByID(id)
	if g == nil {
		return Gate{}, ErrNotFound
	}
	if strings.TrimSpace(patch.Name) != "" {
		g.Name = patch.Name
	}
	if patch.Addr != "" {
		g.Addr = strings.TrimSpace(patch.Addr)
	}
	if patch.Door != 0 {
		g.Door = patch.Door
	}
	if err := validGate(g.Addr, g.Door); err != nil {
		return Gate{}, err
	}
	if err := s.save(); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// DeleteGate removes a gate and scrubs it from plan mappings / active state.
func (s *Store) DeleteGate(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.d.Gates {
		if g.ID != id {
			continue
		}
		s.d.Gates = append(s.d.Gates[:i], s.d.Gates[i+1:]...)
		for plan, ids := range s.d.Config.PlanGates {
			out := ids[:0]
			for _, gid := range ids {
				if gid != id {
					out = append(out, gid)
				}
			}
			s.d.Config.PlanGates[plan] = out
		}
		if s.d.Config.ActiveGate == id {
			s.d.Config.ActiveGate = ""
		}
		return s.save()
	}
	return ErrNotFound
}

// SetActiveGate marks the gate whose panel the middleware connects to, and
// returns it (the caller switches the live connection).
func (s *Store) SetActiveGate(id string) (Gate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gateByID(id)
	if g == nil {
		return Gate{}, ErrNotFound
	}
	s.d.Config.ActiveGate = id
	if err := s.save(); err != nil {
		return Gate{}, err
	}
	return *g, nil
}

// ActiveGate returns the currently selected gate, if any.
func (s *Store) ActiveGate() *Gate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.d.Config.ActiveGate == "" {
		return nil
	}
	g := s.gateByID(s.d.Config.ActiveGate)
	if g == nil {
		return nil
	}
	gg := *g
	return &gg
}

// SetPlanGates replaces the access-level → gate-IDs mapping (empty list for a
// plan = all gates allowed). Unknown gate IDs are dropped.
func (s *Store) SetPlanGates(m map[string][]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clean := map[string][]string{}
	for plan, ids := range m {
		if p := s.planLocked(plan); p == nil || p.Deleted {
			continue
		}
		var keep []string
		for _, id := range ids {
			if s.gateByID(id) != nil {
				keep = append(keep, id)
			}
		}
		if len(keep) > 0 {
			clean[plan] = keep
		}
	}
	s.d.Config.PlanGates = clean
	return s.save()
}

// gateForDoor resolves the gate a swipe happened at (caller holds a lock).
// Door numbers are unique per panel; with one live panel this is exact.
func (s *Store) gateForDoor(door int) *Gate {
	if door <= 0 {
		return nil
	}
	// Prefer a gate on the active panel's address.
	var active *Gate
	if s.d.Config.ActiveGate != "" {
		active = s.gateByID(s.d.Config.ActiveGate)
	}
	var match *Gate
	for _, g := range s.d.Gates {
		if g.Door != door {
			continue
		}
		if active != nil && g.Addr == active.Addr {
			return g
		}
		if match == nil {
			match = g
		}
	}
	return match
}

// planAllowsGate reports whether an access level may open the given gate.
// No mapping (or an empty one) for the plan means all gates are allowed.
func (s *Store) planAllowsGate(access string, g *Gate) bool {
	if g == nil {
		return true
	}
	ids := s.d.Config.PlanGates[access]
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == g.ID {
			return true
		}
	}
	return false
}
