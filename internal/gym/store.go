package gym

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxCheckins caps the stored gate-decision history.
const maxCheckins = 20000

// ErrNotFound is returned when a member or card does not exist.
var ErrNotFound = errors.New("gym: not found")

// data is the on-disk document.
type data struct {
	Config      Config    `json:"config"`
	NextID      int       `json:"next_id"`
	NextReceipt int       `json:"next_receipt"`
	Members     []*Member `json:"members"`
	Cards       []*CardRec `json:"cards"`
	Checkins    []Checkin  `json:"checkins"`
}

// Store holds all gym state with safe concurrent access + JSON persistence.
type Store struct {
	mu   sync.RWMutex
	path string
	d    data
}

// Open loads the store from path, seeding defaults if absent. Empty path keeps
// everything in memory (tests).
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	s.d = defaultData()
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, s.save()
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, fmt.Errorf("gym: parse %s: %w", path, err)
	}
	if s.d.NextID == 0 {
		s.d.NextID = 1001
	}
	if s.d.NextReceipt == 0 {
		s.d.NextReceipt = 1000
	}
	return s, nil
}

func defaultData() data {
	return data{
		Config: Config{
			Enabled:     true,
			WarnDays:    7,
			OpenSeconds: 0,
			PriceGym:    45000,
			PriceClass:  45000,
			PriceFull:   55000,
		},
		NextID:      1001,
		NextReceipt: 1000,
	}
}

// save persists to disk (caller must hold the write lock).
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(&s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Config returns the current gym policy.
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Config
}

// SetConfig replaces the gym policy.
func (s *Store) SetConfig(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.WarnDays <= 0 {
		c.WarnDays = 7
	}
	s.d.Config = c
	return s.save()
}

// ---- status derivation ----

// currentSub returns the member's latest subscription, or nil.
func currentSub(m *Member) *Subscription {
	if len(m.Subs) == 0 {
		return nil
	}
	return &m.Subs[len(m.Subs)-1]
}

// statusAt derives the member's status and remaining days at time t.
func (s *Store) statusAt(m *Member, t time.Time) (string, int) {
	sub := currentSub(m)
	if sub == nil {
		return StatusNone, 0
	}
	if sub.FrozenAt != nil {
		return StatusFrozen, int(sub.End.Sub(t).Hours() / 24)
	}
	if !t.Before(sub.End) {
		return StatusExpired, 0
	}
	left := int(sub.End.Sub(t).Hours()/24) + 1
	if left <= s.d.Config.WarnDays {
		return StatusExpiring, left
	}
	return StatusActive, left
}

func fmtDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("02/01/2006")
}

// view builds the derived projection (caller must hold a lock).
func (s *Store) view(m *Member, t time.Time) MemberView {
	v := MemberView{Member: *m}
	v.Status, v.DaysLeft = s.statusAt(m, t)
	if sub := currentSub(m); sub != nil {
		v.Plan = sub.Access
		months := "شهر"
		if sub.Months > 1 {
			months = fmt.Sprintf("%d أشهر", sub.Months)
		}
		v.PlanMeta = fmt.Sprintf("%s · %s د.ع", months, group3(sub.Price))
		v.Expiry = fmtDate(sub.End)
	}
	return v
}

// group3 formats 135000 as "135,000".
func group3(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	str := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(str) {
		if i > 0 && (len(str)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// ---- members ----

// NewSubInput describes a subscription being sold (create or renew).
type NewSubInput struct {
	Start    time.Time `json:"-"`
	StartStr string    `json:"start"`  // "2026-08-27" (optional; default today)
	Months   int       `json:"months"`
	Price    int64     `json:"price"`  // total; 0 = months * default for access
	Discount int64     `json:"discount"`
	Access   string    `json:"access"`
	Pay      string    `json:"pay_method"`
}

func (s *Store) resolveSub(in NewSubInput, now time.Time) (Subscription, error) {
	if in.Months <= 0 || in.Months > 36 {
		return Subscription{}, errors.New("gym: months must be 1..36")
	}
	switch in.Access {
	case AccessGym, AccessClasses, AccessFull:
	default:
		return Subscription{}, fmt.Errorf("gym: unknown access %q", in.Access)
	}
	start := in.Start
	if start.IsZero() && in.StartStr != "" {
		t, err := time.ParseInLocation("2006-01-02", in.StartStr, now.Location())
		if err != nil {
			return Subscription{}, fmt.Errorf("gym: bad start date %q", in.StartStr)
		}
		start = t
	}
	if start.IsZero() {
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	price := in.Price
	if price == 0 {
		per := s.d.Config.PriceGym
		switch in.Access {
		case AccessClasses:
			per = s.d.Config.PriceClass
		case AccessFull:
			per = s.d.Config.PriceFull
		}
		price = per * int64(in.Months)
	}
	pay := in.Pay
	if pay == "" {
		pay = "cash"
	}
	return Subscription{
		Start:     start,
		Months:    in.Months,
		End:       start.AddDate(0, in.Months, 0),
		Price:     price,
		Discount:  in.Discount,
		Access:    in.Access,
		PayMethod: pay,
	}, nil
}

// AddMember creates a member with an initial subscription (and optional card).
func (s *Store) AddMember(m Member, sub NewSubInput, now time.Time) (MemberView, error) {
	if strings.TrimSpace(m.Name) == "" {
		return MemberView{}, errors.New("gym: name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sb, err := s.resolveSub(sub, now)
	if err != nil {
		return MemberView{}, err
	}
	m.ID = fmt.Sprintf("MGR-%d", s.d.NextID)
	s.d.NextID++
	m.JoinedAt = now
	m.Subs = []Subscription{sb}
	m.Payments = []Payment{{
		Time: now, Amount: sb.Price - sb.Discount, Method: sb.PayMethod,
		Receipt: s.d.NextReceipt, Note: "اشتراك جديد · " + AccessLabel(sb.Access),
	}}
	s.d.NextReceipt++
	m.Log = []LogEntry{{Time: now, Kind: "plan", Label: "اشتراك جديد",
		Meta: fmt.Sprintf("%s · %d أشهر", AccessLabel(sb.Access), sb.Months)}}

	card := strings.TrimSpace(m.CardNo)
	m.CardNo = ""
	mm := m
	s.d.Members = append(s.d.Members, &mm)
	if card != "" {
		if err := s.linkLocked(&mm, card, now); err != nil {
			// roll back the member so a card conflict doesn't half-create
			s.d.Members = s.d.Members[:len(s.d.Members)-1]
			s.d.NextID--
			return MemberView{}, err
		}
	}
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(&mm, now), nil
}

// find returns the member by id (caller must hold a lock).
func (s *Store) find(id string) *Member {
	for _, m := range s.d.Members {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// Member returns one member view.
func (s *Store) Member(id string, now time.Time) (MemberView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	return s.view(m, now), nil
}

// UpdateMember patches personal fields (never subscription/card state).
func (s *Store) UpdateMember(id string, patch Member, now time.Time) (MemberView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	if strings.TrimSpace(patch.Name) != "" {
		m.Name = patch.Name
	}
	m.Phone = patch.Phone
	m.Gender = patch.Gender
	m.BirthDate = patch.BirthDate
	m.Address = patch.Address
	m.Emergency = patch.Emergency
	m.Health = patch.Health
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

// DeleteMember removes a member and frees their card.
func (s *Store) DeleteMember(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.d.Members {
		if m.ID != id {
			continue
		}
		if m.CardNo != "" {
			if c := s.cardBySerial(m.CardNo); c != nil {
				c.Status = CardBlank
				c.MemberID = ""
			}
		}
		s.d.Members = append(s.d.Members[:i], s.d.Members[i+1:]...)
		return s.save()
	}
	return ErrNotFound
}

// Members lists member views, optionally filtered by derived status ("nocard"
// selects members without a linked card) and a free-text query.
func (s *Store) Members(status, q string, now time.Time) []MemberView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q = strings.ToLower(strings.TrimSpace(q))
	out := []MemberView{}
	for _, m := range s.d.Members {
		v := s.view(m, now)
		switch status {
		case "", "all":
		case "nocard":
			if m.CardNo != "" {
				continue
			}
		default:
			if v.Status != status {
				continue
			}
		}
		if q != "" {
			hay := strings.ToLower(m.Name + " " + m.ID + " " + m.CardNo + " " + m.Phone)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Renew appends a new subscription starting at the given date (default: after
// the current one if still active, else today) and records the payment.
func (s *Store) Renew(id string, in NewSubInput, now time.Time) (MemberView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	if in.Start.IsZero() && in.StartStr == "" {
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if sub := currentSub(m); sub != nil && sub.FrozenAt == nil && sub.End.After(start) {
			start = sub.End // extend, don't overlap
		}
		in.Start = start
	}
	sb, err := s.resolveSub(in, now)
	if err != nil {
		return MemberView{}, err
	}
	m.Subs = append(m.Subs, sb)
	m.Payments = append(m.Payments, Payment{
		Time: now, Amount: sb.Price - sb.Discount, Method: sb.PayMethod,
		Receipt: s.d.NextReceipt, Note: "تجديد · " + AccessLabel(sb.Access),
	})
	s.d.NextReceipt++
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "plan", Label: "تجديد الاشتراك",
		Meta: fmt.Sprintf("%s · %d أشهر", AccessLabel(sb.Access), sb.Months)})
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

// Freeze pauses the current subscription; Unfreeze resumes it and pushes the
// end date out by the frozen duration.
func (s *Store) Freeze(id string, days int, reason string, now time.Time) (MemberView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	sub := currentSub(m)
	if sub == nil {
		return MemberView{}, errors.New("gym: no subscription to freeze")
	}
	if sub.FrozenAt != nil {
		return MemberView{}, errors.New("gym: already frozen")
	}
	t := now
	sub.FrozenAt = &t
	sub.FrozenFor = days
	meta := reason
	if days > 0 {
		meta = fmt.Sprintf("%d يوماً", days)
		if reason != "" {
			meta += " · السبب: " + reason
		}
	}
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "freeze", Label: "تجميد الاشتراك", Meta: meta})
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

func (s *Store) Unfreeze(id string, now time.Time) (MemberView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	sub := currentSub(m)
	if sub == nil || sub.FrozenAt == nil {
		return MemberView{}, errors.New("gym: not frozen")
	}
	frozen := int(now.Sub(*sub.FrozenAt).Hours()/24) + 1
	sub.End = sub.End.AddDate(0, 0, frozen)
	sub.FrozenTot += frozen
	sub.FrozenAt = nil
	sub.FrozenFor = 0
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "freeze", Label: "إلغاء التجميد",
		Meta: fmt.Sprintf("مُدد الاشتراك %d يوماً", frozen)})
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

// ---- cards ----

func (s *Store) cardBySerial(serial string) *CardRec {
	for _, c := range s.d.Cards {
		if c.Serial == serial {
			return c
		}
	}
	return nil
}

// RegisterCard adds a blank card to the registry.
func (s *Store) RegisterCard(serial string, now time.Time) (CardRec, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return CardRec{}, errors.New("gym: serial is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.cardBySerial(serial); c != nil {
		return *c, nil // idempotent
	}
	c := &CardRec{Serial: serial, Status: CardBlank, IssuedAt: now}
	s.d.Cards = append(s.d.Cards, c)
	return *c, s.save()
}

// SetCardStatus marks a card blank/suspended/lost, or restores a suspended
// card to linked (only valid while it still has a member). Fresh links go
// through LinkCard.
func (s *Store) SetCardStatus(serial, status string) (CardRec, error) {
	switch status {
	case CardLinked, CardBlank, CardSuspended, CardLost:
	default:
		return CardRec{}, fmt.Errorf("gym: bad card status %q", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cardBySerial(serial)
	if c == nil {
		return CardRec{}, ErrNotFound
	}
	if status == CardBlank && c.MemberID != "" {
		return CardRec{}, errors.New("gym: unlink the member first")
	}
	if status == CardLinked && c.MemberID == "" {
		return CardRec{}, errors.New("gym: card has no member — use link")
	}
	c.Status = status
	return *c, s.save()
}

// linkLocked binds a card to a member (write lock held).
func (s *Store) linkLocked(m *Member, serial string, now time.Time) error {
	c := s.cardBySerial(serial)
	if c == nil {
		c = &CardRec{Serial: serial, Status: CardBlank, IssuedAt: now}
		s.d.Cards = append(s.d.Cards, c)
	}
	if c.MemberID != "" && c.MemberID != m.ID {
		return fmt.Errorf("gym: card %s already linked to %s", serial, c.MemberID)
	}
	if m.CardNo != "" && m.CardNo != serial {
		if old := s.cardBySerial(m.CardNo); old != nil {
			old.Status = CardBlank
			old.MemberID = ""
		}
	}
	c.Status = CardLinked
	c.MemberID = m.ID
	m.CardNo = serial
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "card", Label: "ربط بطاقة", Meta: "بطاقة " + serial})
	return nil
}

// LinkCard binds a card to a member.
func (s *Store) LinkCard(id, serial string, now time.Time) (MemberView, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return MemberView{}, errors.New("gym: serial is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, ErrNotFound
	}
	if err := s.linkLocked(m, serial, now); err != nil {
		return MemberView{}, err
	}
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

// UnlinkCard frees a member's card (card returns to the blank pool).
func (s *Store) UnlinkCard(id string, now time.Time) (MemberView, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(id)
	if m == nil {
		return MemberView{}, "", ErrNotFound
	}
	serial := m.CardNo
	if serial == "" {
		return MemberView{}, "", errors.New("gym: no card linked")
	}
	if c := s.cardBySerial(serial); c != nil {
		c.Status = CardBlank
		c.MemberID = ""
	}
	m.CardNo = ""
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "card", Label: "فك ربط البطاقة", Meta: "بطاقة " + serial})
	if err := s.save(); err != nil {
		return MemberView{}, "", err
	}
	return s.view(m, now), serial, nil
}

// Cards lists the registry with member names joined.
type CardView struct {
	CardRec
	MemberName string `json:"member_name,omitempty"`
}

func (s *Store) Cards() []CardView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []CardView{}
	for _, c := range s.d.Cards {
		v := CardView{CardRec: *c}
		if c.MemberID != "" {
			if m := s.find(c.MemberID); m != nil {
				v.MemberName = m.Name
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// ---- gate decisions ----

// Decide evaluates a swipe of cardNo at time t. It does NOT record anything —
// the engine calls RecordCheckin with the outcome.
func (s *Store) Decide(cardNo string, t time.Time) (Decision, *MemberView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.d.Config.Enabled {
		return Decision{Result: ResultUnknown, Reason: "النظام متوقف", Open: false}, nil
	}
	c := s.cardBySerial(cardNo)
	if c == nil || c.MemberID == "" {
		return Decision{Result: ResultUnknown, Reason: "بطاقة غير مسجّلة", Open: false}, nil
	}
	if c.Status == CardSuspended || c.Status == CardLost {
		v := MemberView{}
		if m := s.find(c.MemberID); m != nil {
			v = s.view(m, t)
		}
		return Decision{Result: ResultDenied, Reason: "البطاقة موقوفة", Open: false}, &v
	}
	m := s.find(c.MemberID)
	if m == nil {
		return Decision{Result: ResultUnknown, Reason: "بطاقة غير مسجّلة", Open: false}, nil
	}
	v := s.view(m, t)
	switch v.Status {
	case StatusFrozen:
		return Decision{Result: ResultDenied, Reason: "الاشتراك مجمّد", Open: false}, &v
	case StatusExpired, StatusNone:
		return Decision{Result: ResultDenied, Reason: "الاشتراك منتهٍ", Open: false}, &v
	case StatusExpiring:
		return Decision{Result: ResultWarn, Reason: "الاشتراك ينتهي قريباً",
			Open: true, DaysLeft: v.DaysLeft}, &v
	}
	return Decision{Result: ResultGranted, Open: true, DaysLeft: v.DaysLeft}, &v
}

// RecordCheckin stores a decision outcome, bumps visit counters, and stamps
// card last-used. Returns the stored record (with visit number).
func (s *Store) RecordCheckin(ck Checkin) Checkin {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.cardBySerial(ck.CardNo); c != nil {
		c.LastUsed = ck.Time
	}
	if ck.MemberID != "" && (ck.Result == ResultGranted || ck.Result == ResultWarn) {
		if m := s.find(ck.MemberID); m != nil {
			if ck.Direction != "out" {
				m.Visits++
				ck.Visit = m.Visits
			}
			m.LastSeen = ck.Time
		}
	}
	s.d.Checkins = append(s.d.Checkins, ck)
	if n := len(s.d.Checkins); n > maxCheckins {
		s.d.Checkins = append([]Checkin(nil), s.d.Checkins[n-maxCheckins:]...)
	}
	_ = s.save()
	return ck
}

// Checkins returns the most recent records, newest first, optionally filtered
// by member id.
func (s *Store) Checkins(memberID string, limit int) []Checkin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 {
		limit = 50
	}
	out := []Checkin{}
	for i := len(s.d.Checkins) - 1; i >= 0 && len(out) < limit; i-- {
		ck := s.d.Checkins[i]
		if memberID != "" && ck.MemberID != memberID {
			continue
		}
		out = append(out, ck)
	}
	return out
}

// Attendance returns granted-visit counts per day for the last `days` days
// (oldest first), for one member.
type DayCount struct {
	Date  string `json:"date"` // "2026-08-27"
	Count int    `json:"count"`
}

func (s *Store) Attendance(memberID string, days int, now time.Time) []DayCount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if days <= 0 {
		days = 112
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	counts := map[string]int{}
	for _, ck := range s.d.Checkins {
		if ck.MemberID != memberID || (ck.Result != ResultGranted && ck.Result != ResultWarn) {
			continue
		}
		if ck.Time.Before(start) || ck.Direction == "out" {
			continue
		}
		counts[ck.Time.Format("2006-01-02")]++
	}
	out := make([]DayCount, 0, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		out = append(out, DayCount{Date: key, Count: counts[key]})
	}
	return out
}

// ---- dashboard ----

// Summary is the dashboard payload.
type Summary struct {
	TodayEntries  int        `json:"today_entries"`
	ActiveMembers int        `json:"active_members"`
	ExpiringWeek  int        `json:"expiring_week"`
	RevenueToday  int64      `json:"revenue_today"`
	PaymentsToday int        `json:"payments_today"`
	Week          []DayCount `json:"week"` // entries per day, last 7 days oldest-first
	TotalMembers  int        `json:"total_members"`
	CardsLinked   int        `json:"cards_linked"`
	CardsBlank    int        `json:"cards_blank"`
	CardsBlocked  int        `json:"cards_blocked"`
}

func (s *Store) Summarize(now time.Time) Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sum := Summary{TotalMembers: len(s.d.Members)}
	today := now.Format("2006-01-02")
	weekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -6)
	perDay := map[string]int{}
	for _, ck := range s.d.Checkins {
		if ck.Result != ResultGranted && ck.Result != ResultWarn {
			continue
		}
		if ck.Direction == "out" || ck.Time.Before(weekStart) {
			continue
		}
		key := ck.Time.Format("2006-01-02")
		perDay[key]++
		if key == today {
			sum.TodayEntries++
		}
	}
	for i := 0; i < 7; i++ {
		d := weekStart.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		sum.Week = append(sum.Week, DayCount{Date: key, Count: perDay[key]})
	}
	for _, m := range s.d.Members {
		st, _ := s.statusAt(m, now)
		switch st {
		case StatusActive, StatusExpiring:
			sum.ActiveMembers++
		}
		if st == StatusExpiring {
			sum.ExpiringWeek++
		}
		for _, p := range m.Payments {
			if p.Time.Format("2006-01-02") == today {
				sum.RevenueToday += p.Amount
				sum.PaymentsToday++
			}
		}
	}
	for _, c := range s.d.Cards {
		switch c.Status {
		case CardLinked:
			sum.CardsLinked++
		case CardBlank:
			sum.CardsBlank++
		default:
			sum.CardsBlocked++
		}
	}
	return sum
}
