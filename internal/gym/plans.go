package gym

import (
	"errors"
	"fmt"
	"strings"
)

// Plan is a sellable access level. Subscriptions and the plan → gate map refer
// to it by Key, which never changes; Name and the default monthly Price are
// editable. A deleted plan is kept (flagged) so members still on it keep a
// readable name and their gate rules until they renew onto another plan.
type Plan struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Price   int64  `json:"price"`    // monthly
	Price1D int64  `json:"price_1d"` // 24-hour pass
	Price3D int64  `json:"price_3d"` // 3-day pass
	Price7D int64  `json:"price_7d"` // 7-day pass
	Deleted bool   `json:"deleted,omitempty"`
}

// PlanPrices is the editable price set of a plan.
type PlanPrices struct {
	Name    string `json:"name"`
	Price   int64  `json:"price"`
	Price1D int64  `json:"price_1d"`
	Price3D int64  `json:"price_3d"`
	Price7D int64  `json:"price_7d"`
}

// PassPrice is the default price of a short pass. 1, 3 and 7 days use the
// plan's own prices when set; otherwise (and for other lengths) the monthly
// price is prorated at a 1.5× day rate, rounded up to 500 IQD.
func (p Plan) PassPrice(days int) int64 {
	switch {
	case days == 1 && p.Price1D > 0:
		return p.Price1D
	case days == 3 && p.Price3D > 0:
		return p.Price3D
	case days == 7 && p.Price7D > 0:
		return p.Price7D
	}
	return defaultPassPrice(p.Price, days)
}

func defaultPassPrice(monthly int64, days int) int64 {
	v := monthly * int64(days) * 3 / 60 // monthly/30 × days × 1.5
	return (v + 499) / 500 * 500
}

// DurationLabel names a subscription length in Arabic ("24 ساعة", "3 أيام", "شهر", "3 أشهر").
func DurationLabel(sub Subscription) string {
	switch {
	case sub.Days == 1:
		return "24 ساعة"
	case sub.Days == 2:
		return "يومان"
	case sub.Days > 2 && sub.Days <= 10:
		return fmt.Sprintf("%d أيام", sub.Days)
	case sub.Days > 10:
		return fmt.Sprintf("%d يوماً", sub.Days)
	case sub.Months == 1:
		return "شهر"
	case sub.Months == 2:
		return "شهران"
	case sub.Months > 2 && sub.Months <= 10:
		return fmt.Sprintf("%d أشهر", sub.Months)
	}
	return fmt.Sprintf("%d شهراً", sub.Months)
}

// legacyPlans builds the three built-in plans from the old price fields.
func legacyPlans(c Config) []Plan {
	mk := func(key string, monthly int64) Plan {
		return Plan{Key: key, Name: AccessLabel(key), Price: monthly,
			Price1D: defaultPassPrice(monthly, 1), Price3D: defaultPassPrice(monthly, 3), Price7D: defaultPassPrice(monthly, 7)}
	}
	return []Plan{mk(AccessGym, c.PriceGym), mk(AccessClasses, c.PriceClass), mk(AccessFull, c.PriceFull)}
}

// planLocked finds a plan by key, deleted ones included (caller holds a lock).
func (s *Store) planLocked(key string) *Plan {
	for i := range s.d.Config.Plans {
		if s.d.Config.Plans[i].Key == key {
			return &s.d.Config.Plans[i]
		}
	}
	return nil
}

// planNameLocked is the display name for a plan key (caller holds a lock).
func (s *Store) planNameLocked(key string) string {
	if p := s.planLocked(key); p != nil && p.Name != "" {
		return p.Name
	}
	return AccessLabel(key)
}

// Plans returns every plan, deleted ones included (clients hide those).
func (s *Store) Plans() []Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Plan(nil), s.d.Config.Plans...)
}

func cleanPlan(in PlanPrices) (PlanPrices, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return in, errors.New("gym: plan name is required")
	}
	if in.Price < 0 || in.Price1D < 0 || in.Price3D < 0 || in.Price7D < 0 {
		return in, errors.New("gym: plan price cannot be negative")
	}
	// Unset pass prices follow the monthly price.
	if in.Price1D == 0 {
		in.Price1D = defaultPassPrice(in.Price, 1)
	}
	if in.Price3D == 0 {
		in.Price3D = defaultPassPrice(in.Price, 3)
	}
	if in.Price7D == 0 {
		in.Price7D = defaultPassPrice(in.Price, 7)
	}
	return in, nil
}

// AddPlan creates a plan with a new stable key.
func (s *Store) AddPlan(in PlanPrices) (Plan, error) {
	in, err := cleanPlan(in)
	if err != nil {
		return Plan{}, err
	}
	name := in.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.d.Config.Plans {
		if !p.Deleted && p.Name == name {
			return Plan{}, fmt.Errorf("gym: a plan named %q already exists", name)
		}
	}
	key := ""
	for n := len(s.d.Config.Plans) + 1; ; n++ {
		if key = fmt.Sprintf("plan-%d", n); s.planLocked(key) == nil {
			break
		}
	}
	p := Plan{Key: key, Name: name, Price: in.Price, Price1D: in.Price1D, Price3D: in.Price3D, Price7D: in.Price7D}
	s.d.Config.Plans = append(s.d.Config.Plans, p)
	return p, s.save()
}

// UpdatePlan renames / reprices a plan. Existing subscriptions keep the price
// they were sold at; the new price applies to new sales and renewals.
func (s *Store) UpdatePlan(key string, in PlanPrices) (Plan, error) {
	in, err := cleanPlan(in)
	if err != nil {
		return Plan{}, err
	}
	name := in.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.planLocked(key)
	if p == nil || p.Deleted {
		return Plan{}, ErrNotFound
	}
	for _, o := range s.d.Config.Plans {
		if o.Key != key && !o.Deleted && o.Name == name {
			return Plan{}, fmt.Errorf("gym: a plan named %q already exists", name)
		}
	}
	p.Name, p.Price, p.Price1D, p.Price3D, p.Price7D = name, in.Price, in.Price1D, in.Price3D, in.Price7D
	s.syncLegacyPrice(*p)
	return *p, s.save()
}

// DeletePlan hides a plan from new sales. Members already on it keep their
// subscription (and its gate rules) until it ends. The last plan cannot go.
func (s *Store) DeletePlan(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.planLocked(key)
	if p == nil || p.Deleted {
		return ErrNotFound
	}
	live := 0
	for _, o := range s.d.Config.Plans {
		if !o.Deleted {
			live++
		}
	}
	if live <= 1 {
		return errors.New("gym: cannot delete the last plan")
	}
	p.Deleted = true
	return s.save()
}

// syncLegacyPrice keeps the old per-plan price fields current for clients that
// still read them (caller holds the write lock).
func (s *Store) syncLegacyPrice(p Plan) {
	switch p.Key {
	case AccessGym:
		s.d.Config.PriceGym = p.Price
	case AccessClasses:
		s.d.Config.PriceClass = p.Price
	case AccessFull:
		s.d.Config.PriceFull = p.Price
	}
}
