// Package gym implements the gym membership layer on top of the C3 gate:
// members with time-boxed subscriptions bound to access cards, a decision
// engine that grants/denies the gate on every swipe, attendance history, and
// a card registry. State persists as one JSON file (see Store).
package gym

import "time"

// Access levels a subscription can grant.
const (
	AccessGym     = "gym"         // صالة فقط
	AccessClasses = "gym_classes" // صالة + حصص
	AccessFull    = "full"        // كامل الوصول
)

// Derived member status.
const (
	StatusActive   = "active"   // نشط
	StatusExpiring = "expiring" // ينتهي قريباً
	StatusExpired  = "expired"  // منتهٍ
	StatusFrozen   = "frozen"   // مجمّد
	StatusNone     = "none"     // بلا اشتراك
)

// Gate decision results.
const (
	ResultGranted   = "granted" // open the door
	ResultWarn      = "warn"    // open the door + renewal warning
	ResultDenied    = "denied"  // keep closed (expired / frozen / card blocked)
	ResultUnknown   = "unknown" // card not registered to any member
	ResultCheckedIn = ""        // sentinel: not a gym decision (unused card event)
)

// Card registry statuses.
const (
	CardLinked    = "linked"    // مرتبطة
	CardBlank     = "blank"     // فارغة
	CardSuspended = "suspended" // موقوفة
	CardLost      = "lost"      // مفقودة
)

// Subscription is one paid access period. The last entry in Member.Subs is the
// current one; earlier entries are history.
type Subscription struct {
	Start     time.Time  `json:"start"`
	Months    int        `json:"months"`
	End       time.Time  `json:"end"` // Start + Months, extended by freezes
	Price     int64      `json:"price"`
	Discount  int64      `json:"discount"`
	Access    string     `json:"access"`     // gym | gym_classes | full
	PayMethod string     `json:"pay_method"` // cash | card | transfer
	FrozenAt  *time.Time `json:"frozen_at,omitempty"`
	FrozenFor int        `json:"frozen_for,omitempty"` // requested days for current freeze
	FrozenTot int        `json:"frozen_total,omitempty"`
}

// Payment is one receipt.
type Payment struct {
	Time    time.Time `json:"time"`
	Amount  int64     `json:"amount"`
	Method  string    `json:"method"`
	Receipt int       `json:"receipt"` // sequential receipt number
	Note    string    `json:"note"`    // e.g. "اشتراك جديد", "تجديد"
}

// LogEntry is a member-timeline event (plan change, freeze, card link, ...).
type LogEntry struct {
	Time  time.Time `json:"time"`
	Kind  string    `json:"kind"` // plan | card | freeze | note
	Label string    `json:"label"`
	Meta  string    `json:"meta"`
}

// Member is one gym member with full history.
type Member struct {
	ID        string     `json:"id"` // e.g. "MGR-1042"
	Name      string     `json:"name"`
	Phone     string     `json:"phone"`
	Gender    string     `json:"gender"` // male | female | ""
	BirthDate string     `json:"birth_date"`
	Address   string     `json:"address"`
	Emergency string     `json:"emergency"`
	Health    string     `json:"health"` // health notes
	CardNo    string     `json:"card_no"`
	JoinedAt  time.Time  `json:"joined_at"`
	Visits    int        `json:"visits"`
	LastSeen  time.Time  `json:"last_seen"`
	Subs      []Subscription `json:"subs"`
	Payments  []Payment      `json:"payments"`
	Log       []LogEntry     `json:"log"`
}

// CardRec is one physical card in the registry.
type CardRec struct {
	Serial   string    `json:"serial"` // decimal Wiegand number
	Status   string    `json:"status"` // linked | blank | suspended | lost
	MemberID string    `json:"member_id,omitempty"`
	IssuedAt time.Time `json:"issued_at"`
	LastUsed time.Time `json:"last_used"`
}

// Checkin is one gate decision, stored newest-last.
type Checkin struct {
	Time      time.Time `json:"time"`
	MemberID  string    `json:"member_id"`
	Name      string    `json:"name"`
	CardNo    string    `json:"card_no"`
	Door      int       `json:"door"`
	Reader    int       `json:"reader"`
	Direction string    `json:"direction"` // in | out | ""
	Result    string    `json:"result"`    // granted | warn | denied | unknown
	Reason    string    `json:"reason"`
	Visit     int       `json:"visit,omitempty"` // visit number when granted
}

// Gate is one physical entrance: a ZKAccess C3 panel address plus the door
// (relay) number on it. Gates are discovered on the LAN or added manually,
// and plans grant access per gate.
type Gate struct {
	ID      string    `json:"id"`   // "gate-1"
	Name    string    `json:"name"` // e.g. "البوابة الرئيسية"
	Addr    string    `json:"addr"` // "192.168.0.102:4370"
	Door    int       `json:"door"` // relay number on the panel (1..4)
	SN      string    `json:"sn,omitempty"`
	Model   string    `json:"model,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// Config is the gym-level policy stored alongside the data.
type Config struct {
	Enabled     bool  `json:"enabled"`      // master switch: engine opens doors
	WarnDays    int   `json:"warn_days"`    // "expiring soon" window (days)
	OpenSeconds int   `json:"open_seconds"` // relay pulse; 0 = server default
	PriceGym    int64 `json:"price_gym"`    // default monthly price: صالة فقط
	PriceClass  int64 `json:"price_classes"`
	PriceFull   int64 `json:"price_full"`

	// ActiveGate is the gate whose panel the middleware is connected to.
	ActiveGate string `json:"active_gate,omitempty"`
	// PlanGates maps an access level (gym | gym_classes | full) to the gate
	// IDs it may open. A missing or empty list means ALL gates are allowed.
	PlanGates map[string][]string `json:"plan_gates,omitempty"`
}

// Decision is the outcome the engine acts on for one swipe.
type Decision struct {
	Result   string `json:"result"` // granted | warn | denied | unknown
	Reason   string `json:"reason"`
	Open     bool   `json:"open"`
	DaysLeft int    `json:"days_left"`
}

// GateUpdate is what the gate kiosk screen receives per swipe (SSE payload).
type GateUpdate struct {
	Checkin
	Status    string `json:"status"`     // member status after decision
	Plan      string `json:"plan"`       // access level key
	DaysLeft  int    `json:"days_left"`
	Initials  string `json:"initials"`
	ExpiredAt string `json:"expired_at,omitempty"` // dd/mm/yyyy when denied-expired
}

// MemberView is a list/detail projection with derived fields.
type MemberView struct {
	Member
	Status   string `json:"status"`
	DaysLeft int    `json:"days_left"`
	Plan     string `json:"plan"`     // current access key ("" if none)
	PlanMeta string `json:"plan_meta"`// months + price of current sub
	Expiry   string `json:"expiry"`   // dd/mm/yyyy
	HasPhoto bool   `json:"has_photo"`
}

// AccessLabel maps an access key to its Arabic label (used in logs/receipts).
func AccessLabel(key string) string {
	switch key {
	case AccessGym:
		return "صالة فقط"
	case AccessClasses:
		return "صالة + حصص"
	case AccessFull:
		return "كامل الوصول"
	}
	return key
}
