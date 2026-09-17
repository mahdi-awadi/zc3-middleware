package gym

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	NextGateID  int       `json:"next_gate_id"`
	Members     []*Member `json:"members"`
	Cards       []*CardRec `json:"cards"`
	Gates       []*Gate    `json:"gates"`
	Checkins    []Checkin  `json:"checkins"`
	// ArchivedPayments keeps the money of deleted members for revenue reports.
	ArchivedPayments []PaymentRecord `json:"archived_payments,omitempty"`
}

// Store holds all gym state with safe concurrent access + JSON persistence.
// Member photos live as files in a sibling "gym-photos" directory.
type Store struct {
	mu       sync.RWMutex
	path     string
	photoDir string
	d        data
}

// Open loads the store from path, seeding defaults if absent. Empty path keeps
// everything in memory (tests).
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	s.d = defaultData()
	if path == "" {
		return s, nil
	}
	s.photoDir = filepath.Join(filepath.Dir(path), "gym-photos")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, s.save()
		}
		return nil, err
	}
	// Plans in the defaults must not shadow a file saved before plans existed:
	// those get their plans from the file's own legacy prices below.
	s.d.Config.Plans = nil
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, fmt.Errorf("gym: parse %s: %w", path, err)
	}
	if s.d.NextID == 0 {
		s.d.NextID = 1001
	}
	if s.d.NextReceipt == 0 {
		s.d.NextReceipt = 1000
	}
	if s.d.Config.Plans == nil {
		s.d.Config.Plans = legacyPlans(s.d.Config)
	}
	for i := range s.d.Config.Plans {
		p := &s.d.Config.Plans[i]
		if p.Price1D == 0 && p.Price3D == 0 && p.Price7D == 0 {
			p.Price1D, p.Price3D, p.Price7D = defaultPassPrice(p.Price, 1), defaultPassPrice(p.Price, 3), defaultPassPrice(p.Price, 7)
		}
	}
	return s, nil
}

func defaultData() data {
	d := data{
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
	d.Config.Plans = legacyPlans(d.Config)
	return d
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

// SetConfig replaces the gym policy. Gate state (active gate, plan→gate map)
// is server-managed: the active gate is always preserved, and the plan map is
// preserved unless the caller provides one.
func (s *Store) SetConfig(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.WarnDays <= 0 {
		c.WarnDays = 7
	}
	c.ActiveGate = s.d.Config.ActiveGate
	if c.Plans == nil {
		c.Plans = s.d.Config.Plans
	}
	if c.Business == (Business{}) {
		c.Business = s.d.Config.Business // managed through SetBusiness
	}
	if c.PlanGates == nil {
		c.PlanGates = s.d.Config.PlanGates
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
	if _, ok := s.photoPath(m.ID); ok {
		v.HasPhoto = true
	}
	if sub := currentSub(m); sub != nil {
		v.Plan = sub.Access
		v.PlanMeta = fmt.Sprintf("%s · %s د.ع", DurationLabel(*sub), group3(sub.Price))
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
	Days     int       `json:"days"`  // short pass length in days; when set, Months must be 0
	Price    int64     `json:"price"`  // total; 0 = months * default for access
	Discount int64     `json:"discount"`
	Access   string    `json:"access"`
	Pay      string    `json:"pay_method"`
}

func (s *Store) resolveSub(in NewSubInput, now time.Time) (Subscription, error) {
	switch {
	case in.Days > 0 && in.Months > 0:
		return Subscription{}, errors.New("gym: give months or days, not both")
	case in.Days > 0:
		if in.Days > 31 {
			return Subscription{}, errors.New("gym: days must be 1..31")
		}
	case in.Months <= 0 || in.Months > 36:
		return Subscription{}, errors.New("gym: months must be 1..36")
	}
	plan := s.planLocked(in.Access)
	if plan == nil || plan.Deleted {
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
		if in.Days > 0 {
			start = now // a day pass runs 24h per day from the moment it is sold
		}
	}
	price := in.Price
	if price == 0 {
		if in.Days > 0 {
			price = plan.PassPrice(in.Days)
		} else {
			price = plan.Price * int64(in.Months)
		}
	}
	end := start.AddDate(0, in.Months, 0)
	if in.Days > 0 {
		end = start.Add(time.Duration(in.Days) * 24 * time.Hour)
	}
	pay := in.Pay
	if pay == "" {
		pay = "cash"
	}
	return Subscription{
		Start:     start,
		Months:    in.Months,
		Days:      in.Days,
		End:       end,
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
	m.Visits = 0
	m.LastSeen = time.Time{}
	m.Subs = []Subscription{sb}
	m.Payments = []Payment{{
		Time: now, Amount: sb.Price - sb.Discount, Method: sb.PayMethod,
		Receipt: s.d.NextReceipt, Note: "اشتراك جديد · " + s.planNameLocked(sb.Access),
	}}
	s.d.NextReceipt++
	m.Log = []LogEntry{{Time: now, Kind: "plan", Label: "اشتراك جديد",
		Meta: s.planNameLocked(sb.Access) + " · " + DurationLabel(sb)}}

	card := strings.TrimSpace(m.CardNo)
	m.CardNo = ""
	mm := m
	s.d.Members = append(s.d.Members, &mm)
	if card != "" {
		if err := s.linkLocked(&mm, card, now); err != nil {
			// roll back the member so a card conflict doesn't half-create
			s.d.Members = s.d.Members[:len(s.d.Members)-1]
			s.d.NextID--
			s.d.NextReceipt--
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
// Payments lists every payment ever taken, oldest first, including those of
// deleted members.
func (s *Store) Payments() []PaymentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]PaymentRecord(nil), s.d.ArchivedPayments...)
	for _, m := range s.d.Members {
		for _, p := range m.Payments {
			out = append(out, PaymentRecord{Payment: p, MemberID: m.ID, Name: m.Name})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

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
		for _, p := range m.Payments {
			s.d.ArchivedPayments = append(s.d.ArchivedPayments, PaymentRecord{Payment: p, MemberID: m.ID, Name: m.Name, Deleted: true})
		}
		s.d.Members = append(s.d.Members[:i], s.d.Members[i+1:]...)
		if p, ok := s.photoPath(id); ok {
			_ = os.Remove(p)
		}
		return s.save()
	}
	return ErrNotFound
}

// ---- member photos (files beside the data file) ----

const maxPhotoBytes = 3 << 20 // 3 MB

// photoPath returns the on-disk photo for a member and whether it exists. The
// id comes from the URL, so anything that is not a plain file name is refused.
func (s *Store) photoPath(id string) (string, bool) {
	if s.photoDir == "" || id == "" || id != filepath.Base(id) || strings.Contains(id, "..") {
		return "", false
	}
	p := filepath.Join(s.photoDir, id+".img")
	if _, err := os.Stat(p); err != nil {
		return p, false
	}
	return p, true
}

// SavePhoto stores a member's photo (raw image bytes).
func (s *Store) SavePhoto(id string, data []byte) error {
	if s.photoDir == "" {
		return errors.New("gym: photo storage unavailable (in-memory store)")
	}
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return fmt.Errorf("gym: photo must be 1 byte .. %d MB", maxPhotoBytes>>20)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(id) == nil {
		return ErrNotFound
	}
	if err := os.MkdirAll(s.photoDir, 0o755); err != nil {
		return err
	}
	p, _ := s.photoPath(id)
	if p == "" {
		return ErrNotFound
	}
	return os.WriteFile(p, data, 0o644)
}

// Photo returns the raw photo bytes for a member.
func (s *Store) Photo(id string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.find(id) == nil {
		return nil, ErrNotFound
	}
	p, ok := s.photoPath(id)
	if !ok {
		return nil, ErrNotFound
	}
	return os.ReadFile(p)
}

// DeletePhoto removes a member's photo.
func (s *Store) DeletePhoto(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(id) == nil {
		return ErrNotFound
	}
	p, ok := s.photoPath(id)
	if !ok {
		return ErrNotFound
	}
	return os.Remove(p)
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
	// A frozen subscription is lifted first so its remaining days are credited
	// (pushed onto the end) instead of being lost under the new period.
	if sub := currentSub(m); sub != nil && sub.FrozenAt != nil {
		s.unfreezeLocked(m, sub, now)
	}
	if in.Start.IsZero() && in.StartStr == "" {
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if in.Days > 0 {
			start = now
		}
		if sub := currentSub(m); sub != nil && sub.End.After(start) {
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
		Receipt: s.d.NextReceipt, Note: "تجديد · " + s.planNameLocked(sb.Access),
	})
	s.d.NextReceipt++
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "plan", Label: "تجديد الاشتراك",
		Meta: s.planNameLocked(sb.Access) + " · " + DurationLabel(sb)})
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
	if !now.Before(sub.End) {
		return MemberView{}, errors.New("gym: subscription already expired")
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
	s.unfreezeLocked(m, sub, now)
	if err := s.save(); err != nil {
		return MemberView{}, err
	}
	return s.view(m, now), nil
}

// unfreezeLocked lifts the freeze on sub, crediting the frozen days to its end
// date (caller holds the write lock and saves).
func (s *Store) unfreezeLocked(m *Member, sub *Subscription, now time.Time) {
	frozen := int(now.Sub(*sub.FrozenAt).Hours()/24) + 1
	sub.End = sub.End.AddDate(0, 0, frozen)
	sub.FrozenTot += frozen
	sub.FrozenAt = nil
	sub.FrozenFor = 0
	m.Log = append(m.Log, LogEntry{Time: now, Kind: "freeze", Label: "إلغاء التجميد",
		Meta: fmt.Sprintf("مُدد الاشتراك %d يوماً", frozen)})
}

// ---- cards ----

// NormalizeCard canonicalises a card number so the same card matches whether
// it came from the C3 panel ("12345678") or a USB desk reader, which usually
// types it zero-padded ("0012345678").
func NormalizeCard(serial string) string {
	serial = strings.TrimSpace(serial)
	if t := strings.TrimLeft(serial, "0"); t != "" {
		return t
	}
	if serial != "" {
		return "0"
	}
	return ""
}

// SameCard reports whether two card numbers identify the same physical card.
// Besides leading zeros, a USB desk reader types the card's full 32-bit ID
// while the C3 panel over Wiegand-26 only sees its low 24 bits (facility +
// number), so a long number also matches its 24-bit form.
func SameCard(a, b string) bool {
	a, b = NormalizeCard(a), NormalizeCard(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		return false
	}
	const w26 = 1<<24 - 1
	switch {
	case x > w26 && y <= w26:
		return x&w26 == y
	case y > w26 && x <= w26:
		return y&w26 == x
	}
	return false
}

func (s *Store) cardBySerial(serial string) *CardRec {
	serial = NormalizeCard(serial)
	for _, c := range s.d.Cards {
		if NormalizeCard(c.Serial) == serial {
			return c
		}
	}
	for _, c := range s.d.Cards {
		if SameCard(c.Serial, serial) {
			return c
		}
	}
	return nil
}

// RegisterCard adds a blank card to the registry.
func (s *Store) RegisterCard(serial string, now time.Time) (CardRec, error) {
	serial = NormalizeCard(serial)
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
	serial = NormalizeCard(serial)
	c := s.cardBySerial(serial)
	if c == nil {
		c = &CardRec{Serial: serial, Status: CardBlank, IssuedAt: now}
		s.d.Cards = append(s.d.Cards, c)
	}
	if c.MemberID != "" && c.MemberID != m.ID {
		return fmt.Errorf("gym: card %s already linked to %s", serial, c.MemberID)
	}
	if m.CardNo != "" && !SameCard(m.CardNo, serial) {
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

// Decide evaluates a swipe of cardNo at the given door at time t. It does NOT
// record anything — the engine calls RecordCheckin with the outcome.
func (s *Store) Decide(cardNo string, door int, t time.Time) (Decision, *MemberView) {
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
	}
	// Plan → gate permission: the swipe's door resolves to a gate, and the
	// member's access level must include it.
	if g := s.gateForDoor(door); g != nil && !s.planAllowsGate(v.Plan, g) {
		return Decision{Result: ResultDenied,
			Reason: "الخطة لا تشمل بوابة «" + g.Name + "»", Open: false}, &v
	}
	if v.Status == StatusExpiring {
		return Decision{Result: ResultWarn, Reason: "الاشتراك ينتهي قريباً",
			Open: true, DaysLeft: v.DaysLeft}, &v
	}
	return Decision{Result: ResultGranted, Open: true, DaysLeft: v.DaysLeft}, &v
}

// DecideExit evaluates a swipe on an exit ("out") reader. Leaving is never
// blocked for a registered member card, even with an expired, frozen or
// suspended subscription; only cards that belong to nobody are refused.
func (s *Store) DecideExit(cardNo string, t time.Time) (Decision, *MemberView) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cardBySerial(cardNo)
	if c == nil || c.MemberID == "" {
		return Decision{Result: ResultUnknown, Reason: "بطاقة غير مسجّلة", Open: false}, nil
	}
	m := s.find(c.MemberID)
	if m == nil {
		return Decision{Result: ResultUnknown, Reason: "بطاقة غير مسجّلة", Open: false}, nil
	}
	v := s.view(m, t)
	return Decision{Result: ResultGranted, Reason: "خروج", Open: true, DaysLeft: v.DaysLeft}, &v
}

// MemberByCard resolves a card serial to its linked member, if any.
func (s *Store) MemberByCard(serial string) *MemberView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.cardBySerial(serial)
	if c == nil || c.MemberID == "" {
		return nil
	}
	m := s.find(c.MemberID)
	if m == nil {
		return nil
	}
	v := s.view(m, time.Now())
	return &v
}

// RecordCheckin stores a decision outcome, bumps visit counters, and stamps
// card last-used. Returns the stored record (with visit number); the record is
// kept in memory even when persisting it fails.
func (s *Store) RecordCheckin(ck Checkin) (Checkin, error) {
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
	return ck, s.save()
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
	for _, p := range s.d.ArchivedPayments {
		if p.Time.Format("2006-01-02") == today {
			sum.RevenueToday += p.Amount
			sum.PaymentsToday++
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
