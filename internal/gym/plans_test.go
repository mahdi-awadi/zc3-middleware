package gym

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

func TestPlansMigrateFromLegacyPrices(t *testing.T) {
	path := t.TempDir() + "/gym.json"
	legacy := `{"config":{"enabled":true,"warn_days":7,"price_gym":40000,"price_classes":50000,"price_full":60000},"next_id":1001,"next_receipt":1000}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := s.Plans()
	if len(ps) != 3 || ps[0].Key != AccessGym || ps[0].Price != 40000 || ps[2].Name != "كامل الوصول" || ps[2].Price != 60000 {
		t.Fatalf("plans = %+v", ps)
	}
}

func TestEditPlanPricesNewSalesAndNamesReceipts(t *testing.T) {
	s := newStore(t)
	if _, err := s.UpdatePlan(AccessGym, PlanPrices{Name: "صالة شهرية", Price: 30000}); err != nil {
		t.Fatal(err)
	}
	v, err := s.AddMember(Member{Name: "علي"}, NewSubInput{Months: 2, Access: AccessGym}, now)
	if err != nil {
		t.Fatal(err)
	}
	sub := v.Subs[len(v.Subs)-1]
	if sub.Price != 60000 {
		t.Fatalf("price = %d, want 2 × 30000", sub.Price)
	}
	if !strings.Contains(v.Log[0].Meta, "صالة شهرية") {
		t.Fatalf("log meta = %q", v.Log[0].Meta)
	}
	if s.Config().PriceGym != 30000 {
		t.Fatal("legacy price field not kept in sync")
	}
	if _, err := s.UpdatePlan(AccessGym, PlanPrices{Name: "  ", Price: 1}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := s.UpdatePlan(AccessFull, PlanPrices{Name: "صالة شهرية", Price: 1}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if _, err := s.UpdatePlan("nope", PlanPrices{Name: "x", Price: 1}); err != ErrNotFound {
		t.Fatalf("unknown key err = %v", err)
	}
}

func TestDeleteAndAddPlan(t *testing.T) {
	s := newStore(t)
	m := addMember(t, s, "زينب", "777", 3) // on gym_classes

	if err := s.DeletePlan(AccessClasses); err != nil {
		t.Fatal(err)
	}
	// No new sales on a deleted plan…
	if _, err := s.AddMember(Member{Name: "x"}, NewSubInput{Months: 1, Access: AccessClasses}, now); err == nil {
		t.Fatal("sold a deleted plan")
	}
	// …but the existing member keeps access and a readable plan name.
	if dec, _ := s.Decide("777", 1, now.Add(time.Hour)); !dec.Open {
		t.Fatalf("existing member refused after plan delete: %+v", dec)
	}
	if got := s.planNameLocked(m.Plan); got != "صالة + حصص" {
		t.Fatalf("deleted plan name = %q", got)
	}

	p, err := s.AddPlan(PlanPrices{Name: "صباحي", Price: 25000})
	if err != nil {
		t.Fatal(err)
	}
	if p.Key == "" || p.Key == AccessGym || p.Key == AccessClasses || p.Key == AccessFull {
		t.Fatalf("new key = %q", p.Key)
	}
	v, err := s.AddMember(Member{Name: "حسن"}, NewSubInput{Months: 1, Access: p.Key}, now)
	if err != nil {
		t.Fatal(err)
	}
	if v.Plan != p.Key || v.Subs[0].Price != 25000 {
		t.Fatalf("member on new plan = plan %q price %d", v.Plan, v.Subs[0].Price)
	}

	if err := s.DeletePlan(AccessGym); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlan(AccessFull); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlan(p.Key); err == nil {
		t.Fatal("deleted the last plan")
	}
}

// fakePanel records what the engine asks the panel to do.
type fakePanel struct {
	mu      sync.Mutex
	opened  []int
	closed  []int
	revoked []string
}

func (f *fakePanel) OpenDoor(_ context.Context, door, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, door)
	return nil
}

func (f *fakePanel) CloseDoor(_ context.Context, door int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, door)
	return nil
}

func (f *fakePanel) RevokeAuthorization(_ context.Context, pin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, pin)
	return nil
}

func (f *fakePanel) snapshot() (opened, closed []int, revoked []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.opened...), append([]int(nil), f.closed...), append([]string(nil), f.revoked...)
}

func TestRefusedCardThePanelOpenedGetsRelocked(t *testing.T) {
	s := newStore(t)
	// Live panel events are checked against the real clock, so build the
	// members around it too.
	today := time.Now()
	frozen, err := s.AddMember(Member{Name: "مجمّد", CardNo: "507346370"}, NewSubInput{Months: 3, Access: AccessClasses}, today)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Freeze(frozen.ID, 10, "سفر", today); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(Member{Name: "نشط", CardNo: "497420652"}, NewSubInput{Months: 3, Access: AccessClasses}, today); err != nil {
		t.Fatal(err)
	}

	panel := &fakePanel{}
	e := NewEngine(s, panel, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	at := today.Add(time.Minute)
	// Frozen member, panel says "Normal Punch Open" (it unlocked by itself).
	e.Handle(c3.Event{Time: at, CardNo: "507346370", PIN: "507346370", Door: 2, EventType: 0})
	// Unknown card the panel also let through.
	e.Handle(c3.Event{Time: at, CardNo: "123456", PIN: "123456", Door: 1, EventType: 0})
	// Refused card the panel refused too: nothing to undo.
	e.Handle(c3.Event{Time: at, CardNo: "999", PIN: "999", Door: 1, EventType: 27})
	// Valid member: normal open, never re-locked.
	e.Handle(c3.Event{Time: at, CardNo: "497420652", PIN: "497420652", Door: 1, EventType: 0})

	deadline := time.Now().Add(2 * time.Second)
	for {
		opened, closed, revoked := panel.snapshot()
		if len(opened) == 1 && len(closed) == 2 && len(revoked) == 2 {
			if closed[0] != 2 || revoked[0] != "507346370" || closed[1] != 1 || revoked[1] != "123456" || opened[0] != 1 {
				t.Fatalf("opened %v closed %v revoked %v", opened, closed, revoked)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("opened %v closed %v revoked %v", opened, closed, revoked)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulated (desk) swipes never touch the panel's rules.
	e.Simulate("507346370", 1, true)
	time.Sleep(50 * time.Millisecond)
	if _, closed, revoked := panel.snapshot(); len(closed) != 2 || len(revoked) != 2 {
		t.Fatalf("simulate triggered a re-lock: closed %v revoked %v", closed, revoked)
	}
}

func TestSwipeTimeIgnoresBrokenPanelClock(t *testing.T) {
	now := time.Date(2026, 9, 17, 6, 0, 0, 0, time.UTC)
	cases := map[string]struct{ panel, want time.Time }{
		"good clock":     {now.Add(-2 * time.Minute), now.Add(-2 * time.Minute)},
		"reset to 2020":  {time.Date(2020, 3, 22, 5, 0, 0, 0, time.UTC), now},
		"future":         {now.Add(48 * time.Hour), now},
		"missing":        {time.Time{}, now},
		"recent backlog": {now.AddDate(0, 0, -3), now.AddDate(0, 0, -3)},
	}
	for name, c := range cases {
		if got := swipeTime(c.panel, now); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", name, got, c.want)
		}
	}
}

func TestDayPasses(t *testing.T) {
	s := newStore(t)
	if _, err := s.UpdatePlan(AccessGym, PlanPrices{Name: "صالة فقط", Price: 45000, Price1D: 3000}); err != nil {
		t.Fatal(err)
	}
	sold := now.Add(15*time.Hour + 30*time.Minute) // 15:30
	v, err := s.AddMember(Member{Name: "زائر"}, NewSubInput{Days: 1, Access: AccessGym}, sold)
	if err != nil {
		t.Fatal(err)
	}
	sub := v.Subs[0]
	if !sub.Start.Equal(sold) || !sub.End.Equal(sold.Add(24*time.Hour)) {
		t.Fatalf("24h pass runs %v → %v", sub.Start, sub.End)
	}
	if sub.Price != 3000 || v.Payments[0].Amount != 3000 {
		t.Fatalf("pass price = %d", sub.Price)
	}
	if !strings.Contains(v.Log[0].Meta, "24 ساعة") || !strings.Contains(v.PlanMeta, "24 ساعة") {
		t.Fatalf("labels: log %q meta %q", v.Log[0].Meta, v.PlanMeta)
	}
	if got, _ := s.Member(v.ID, sold.Add(23*time.Hour)); got.Status == StatusExpired {
		t.Fatal("expired before 24h")
	}
	if got, _ := s.Member(v.ID, sold.Add(24*time.Hour)); got.Status != StatusExpired {
		t.Fatalf("status after 24h = %s", got.Status)
	}

	// 7-day pass without a set price: prorated default, renewal after expiry starts at purchase time.
	later := sold.Add(3 * 24 * time.Hour)
	r, err := s.Renew(v.ID, NewSubInput{Days: 7, Access: AccessClasses}, later)
	if err != nil {
		t.Fatal(err)
	}
	last := r.Subs[len(r.Subs)-1]
	if !last.Start.Equal(later) || !last.End.Equal(later.Add(7*24*time.Hour)) {
		t.Fatalf("7-day renewal runs %v → %v", last.Start, last.End)
	}
	if want := defaultPassPrice(45000, 7); last.Price != want || want != 16000 {
		t.Fatalf("7-day price = %d, default %d", last.Price, want)
	}

	if _, err := s.AddMember(Member{Name: "x"}, NewSubInput{Days: 3, Months: 1, Access: AccessGym}, now); err == nil {
		t.Fatal("accepted months and days together")
	}
	if _, err := s.AddMember(Member{Name: "x"}, NewSubInput{Days: 40, Access: AccessGym}, now); err == nil {
		t.Fatal("accepted a 40-day pass")
	}
}

func TestExitAlwaysLetsMembersOut(t *testing.T) {
	s := newStore(t)
	today := time.Now()
	m, err := s.AddMember(Member{Name: "منتهٍ", CardNo: "4242"}, NewSubInput{Days: 1, Access: AccessGym}, today.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	panel := &fakePanel{}
	e := NewEngine(s, panel, 3, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	at := today.Add(-time.Minute)
	in := e.process("4242", 1, 1, "in", at, false, true, "4242")   // expired: refused on the way in
	out := e.process("4242", 1, 2, "out", at, false, true, "4242") // but free to leave
	strange := e.process("777", 1, 2, "out", at, false, true, "777")
	if in.Result != ResultDenied || out.Result != ResultGranted || strange.Result != ResultUnknown {
		t.Fatalf("in %s, out %s, unknown out %s", in.Result, out.Result, strange.Result)
	}
	time.Sleep(100 * time.Millisecond)
	opened, closed, revoked := panel.snapshot()
	if len(opened) != 1 || len(closed) != 1 || len(revoked) != 1 {
		t.Fatalf("exit must open and never re-lock: opened %v closed %v revoked %v", opened, closed, revoked)
	}
	if got, _ := s.Member(m.ID, today); got.Visits != 0 {
		t.Fatalf("an exit counted as a visit: %d", got.Visits)
	}
}

func TestDeletedMembersMoneyStays(t *testing.T) {
	s := newStore(t)
	today := time.Now()
	a, _ := s.AddMember(Member{Name: "باقٍ"}, NewSubInput{Months: 1, Access: AccessGym}, today)
	b, _ := s.AddMember(Member{Name: "محذوف"}, NewSubInput{Months: 2, Access: AccessGym}, today)
	if _, err := s.Renew(b.ID, NewSubInput{Months: 1, Access: AccessGym}, today); err != nil {
		t.Fatal(err)
	}
	before := s.Summarize(today).RevenueToday
	if err := s.DeleteMember(b.ID); err != nil {
		t.Fatal(err)
	}
	if after := s.Summarize(today).RevenueToday; after != before {
		t.Fatalf("revenue today changed on delete: %d → %d", before, after)
	}
	pays := s.Payments()
	var total int64
	deleted := 0
	for _, p := range pays {
		total += p.Amount
		if p.Deleted {
			deleted++
			if p.MemberID != b.ID || p.Name != "محذوف" {
				t.Fatalf("archived payment lost its payer: %+v", p)
			}
		}
	}
	if len(pays) != 3 || deleted != 2 || total != 45000*4 {
		t.Fatalf("payments = %d (deleted %d) total %d", len(pays), deleted, total)
	}
	_ = a
}
