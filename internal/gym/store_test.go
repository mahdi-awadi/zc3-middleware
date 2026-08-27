package gym

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("") // in-memory
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addMember(t *testing.T, s *Store, name, card string, months int) MemberView {
	t.Helper()
	v, err := s.AddMember(Member{Name: name, CardNo: card},
		NewSubInput{Months: months, Access: AccessClasses, Pay: "cash"}, now)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAddMemberDerivesStatus(t *testing.T) {
	s := newStore(t)
	v := addMember(t, s, "أحمد الجبوري", "497420652", 3)
	if v.ID != "MGR-1001" {
		t.Fatalf("id = %s", v.ID)
	}
	if v.Status != StatusActive {
		t.Fatalf("status = %s", v.Status)
	}
	if v.CardNo != "497420652" {
		t.Fatalf("card = %s", v.CardNo)
	}
	if len(v.Payments) != 1 || v.Payments[0].Amount != 3*45000 {
		t.Fatalf("payments = %+v", v.Payments)
	}
	// The card lands in the registry as linked.
	cards := s.Cards()
	if len(cards) != 1 || cards[0].Status != CardLinked || cards[0].MemberID != v.ID {
		t.Fatalf("cards = %+v", cards)
	}
}

func TestStatusTransitions(t *testing.T) {
	s := newStore(t)
	v := addMember(t, s, "زينب الكاظمي", "111", 1) // ends 2026-09-27

	if got, _ := s.Member(v.ID, now); got.Status != StatusActive {
		t.Fatalf("want active, got %s", got.Status)
	}
	// 5 days before end -> expiring (warn window 7).
	if got, _ := s.Member(v.ID, now.AddDate(0, 0, 26)); got.Status != StatusExpiring {
		got2, _ := s.Member(v.ID, now.AddDate(0, 0, 26))
		t.Fatalf("want expiring, got %s (days_left=%d)", got2.Status, got2.DaysLeft)
	}
	// After end -> expired.
	if got, _ := s.Member(v.ID, now.AddDate(0, 2, 0)); got.Status != StatusExpired {
		t.Fatalf("want expired")
	}
}

func TestDecide(t *testing.T) {
	s := newStore(t)
	active := addMember(t, s, "أحمد الجبوري", "100", 3)
	expiring := addMember(t, s, "زينب الكاظمي", "200", 1)
	_ = expiring
	expired := addMember(t, s, "حسين الربيعي", "300", 1)

	// Unknown card.
	if d, _ := s.Decide("999", 1, now); d.Result != ResultUnknown || d.Open {
		t.Fatalf("unknown: %+v", d)
	}
	// Active card opens.
	if d, mv := s.Decide("100", 1, now); d.Result != ResultGranted || !d.Open || mv.ID != active.ID {
		t.Fatalf("active: %+v", d)
	}
	// Warn window still opens.
	if d, _ := s.Decide("200", 1, now.AddDate(0, 0, 26)); d.Result != ResultWarn || !d.Open {
		t.Fatalf("warn: %+v", d)
	}
	// Expired denies.
	if d, _ := s.Decide("300", 1, now.AddDate(0, 2, 0)); d.Result != ResultDenied || d.Open {
		t.Fatalf("expired: %+v", d)
	}
	_ = expired

	// Frozen denies.
	if _, err := s.Freeze(active.ID, 14, "سفر", now); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Decide("100", 1, now); d.Result != ResultDenied || d.Open {
		t.Fatalf("frozen: %+v", d)
	}
	// Unfreeze extends the end date.
	un, err := s.Unfreeze(active.ID, now.AddDate(0, 0, 10))
	if err != nil {
		t.Fatal(err)
	}
	if un.Status != StatusActive {
		t.Fatalf("after unfreeze: %s", un.Status)
	}
	sub := un.Subs[len(un.Subs)-1]
	if !sub.End.After(now.AddDate(0, 3, 0)) {
		t.Fatalf("end not extended: %v", sub.End)
	}

	// Suspended card denies even for an active member.
	if _, err := s.SetCardStatus("100", CardSuspended); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Decide("100", 1, now); d.Result != ResultDenied || d.Open {
		t.Fatalf("suspended card: %+v", d)
	}
	// A suspended-but-linked card cannot silently return to the blank pool.
	if _, err := s.SetCardStatus("100", CardBlank); err == nil {
		t.Fatal("blanking a linked card must fail")
	}
	// But it can be restored to linked, and then grants again.
	if _, err := s.SetCardStatus("100", CardLinked); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Decide("100", 1, now); d.Result != ResultGranted {
		t.Fatalf("restored card: %+v", d)
	}
}

func TestRenewExtendsFromCurrentEnd(t *testing.T) {
	s := newStore(t)
	v := addMember(t, s, "علي الدليمي", "500", 1) // ends 2026-09-27
	r, err := s.Renew(v.ID, NewSubInput{Months: 3, Access: AccessFull, Pay: "cash", Discount: 10000}, now)
	if err != nil {
		t.Fatal(err)
	}
	sub := r.Subs[len(r.Subs)-1]
	wantStart := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	if !sub.Start.Equal(wantStart) {
		t.Fatalf("start = %v, want %v", sub.Start, wantStart)
	}
	if sub.Price != 3*55000 {
		t.Fatalf("price = %d", sub.Price)
	}
	if got := r.Payments[len(r.Payments)-1].Amount; got != 3*55000-10000 {
		t.Fatalf("payment = %d", got)
	}
}

func TestCardLinkConflictAndUnlink(t *testing.T) {
	s := newStore(t)
	a := addMember(t, s, "أ", "700", 1)
	b := addMember(t, s, "ب", "", 1)

	if _, err := s.LinkCard(b.ID, "700", now); err == nil {
		t.Fatal("linking an owned card must fail")
	}
	if _, serial, err := s.UnlinkCard(a.ID, now); err != nil || serial != "700" {
		t.Fatalf("unlink: %v %s", err, serial)
	}
	if _, err := s.LinkCard(b.ID, "700", now); err != nil {
		t.Fatalf("relink freed card: %v", err)
	}
	// Members filter: nocard.
	if got := s.Members("nocard", "", now); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("nocard filter: %+v", got)
	}
}

func TestCheckinsAndSummary(t *testing.T) {
	s := newStore(t)
	v := addMember(t, s, "كرار الموسوي", "800", 3)
	for i := 0; i < 3; i++ {
		s.RecordCheckin(Checkin{Time: now.Add(time.Duration(i) * time.Hour),
			MemberID: v.ID, Name: v.Name, CardNo: "800", Direction: "in", Result: ResultGranted})
	}
	s.RecordCheckin(Checkin{Time: now, CardNo: "999", Result: ResultUnknown})

	if got, _ := s.Member(v.ID, now); got.Visits != 3 {
		t.Fatalf("visits = %d", got.Visits)
	}
	sum := s.Summarize(now)
	if sum.TodayEntries != 3 || sum.ActiveMembers != 1 || sum.RevenueToday != 135000 {
		t.Fatalf("summary = %+v", sum)
	}
	att := s.Attendance(v.ID, 7, now)
	if len(att) != 7 || att[6].Count != 3 {
		t.Fatalf("attendance = %+v", att)
	}
	feed := s.Checkins("", 10)
	if len(feed) != 4 || feed[0].Result != ResultUnknown {
		t.Fatalf("feed = %+v", feed)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := t.TempDir() + "/gym.json"
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s1.AddMember(Member{Name: "فاطمة الحسيني", CardNo: "901"},
		NewSubInput{Months: 6, Access: AccessGym, Pay: "transfer"}, now)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Member(v.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "فاطمة الحسيني" || got.CardNo != "901" || got.Status != StatusActive {
		t.Fatalf("reloaded = %+v", got)
	}
	if len(s2.Cards()) != 1 {
		t.Fatal("cards not persisted")
	}
}

func TestGatesAndPlanAccess(t *testing.T) {
	s := newStore(t)
	g1, err := s.AddGate(Gate{Name: "البوابة الرئيسية", Addr: "192.168.0.102:4370", Door: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.AddGate(Gate{Name: "قاعة الحصص", Addr: "192.168.0.102:4370", Door: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	// First gate auto-becomes the active connection target.
	if ag := s.ActiveGate(); ag == nil || ag.ID != g1.ID {
		t.Fatalf("active gate = %+v", ag)
	}
	// Duplicate addr+door rejected.
	if _, err := s.AddGate(Gate{Addr: "192.168.0.102:4370", Door: 1}, now); err == nil {
		t.Fatal("duplicate gate must fail")
	}

	// Member on "gym only" plan; restrict that plan to gate-1 only.
	_, err = s.AddMember(Member{Name: "عضو صالة", CardNo: "9001"},
		NewSubInput{Months: 1, Access: AccessGym}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPlanGates(map[string][]string{AccessGym: {g1.ID}}); err != nil {
		t.Fatal(err)
	}
	// Door 1 (gate-1) -> granted; door 2 (gate-2) -> denied by plan.
	if d, _ := s.Decide("9001", 1, now); d.Result != ResultGranted {
		t.Fatalf("gate1: %+v", d)
	}
	if d, _ := s.Decide("9001", 2, now); d.Result != ResultDenied || d.Open {
		t.Fatalf("gate2 should deny: %+v", d)
	}
	// Unmapped plan (full) passes everywhere.
	f, _ := s.AddMember(Member{Name: "عضو كامل", CardNo: "9002"},
		NewSubInput{Months: 1, Access: AccessFull}, now)
	_ = f
	if d, _ := s.Decide("9002", 2, now); d.Result != ResultGranted {
		t.Fatalf("full plan: %+v", d)
	}
	// A door with no configured gate is not restricted.
	if d, _ := s.Decide("9001", 3, now); d.Result != ResultGranted {
		t.Fatalf("unmapped door: %+v", d)
	}

	// Deleting a gate scrubs mappings and active state.
	if err := s.DeleteGate(g1.ID); err != nil {
		t.Fatal(err)
	}
	if ag := s.ActiveGate(); ag != nil {
		t.Fatalf("active gate should clear, got %+v", ag)
	}
	if ids := s.Config().PlanGates[AccessGym]; len(ids) != 0 {
		t.Fatalf("plan mapping not scrubbed: %v", ids)
	}
	// With the mapping gone, door 1 resolves to no gate -> allowed again... but
	// gate-2 still exists on door 2 and the plan has no mapping now -> allowed.
	if d, _ := s.Decide("9001", 2, now); d.Result != ResultGranted {
		t.Fatalf("after scrub: %+v", d)
	}
	_ = g2
}

func TestPhotoRoundTrip(t *testing.T) {
	path := t.TempDir() + "/gym.json"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.AddMember(Member{Name: "صاحب صورة"}, NewSubInput{Months: 1, Access: AccessGym}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Member(v.ID, now); got.HasPhoto {
		t.Fatal("no photo yet")
	}
	img := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3} // jpeg magic
	if err := s.SavePhoto(v.ID, img); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Member(v.ID, now)
	if !got.HasPhoto {
		t.Fatal("has_photo should be true")
	}
	raw, err := s.Photo(v.ID)
	if err != nil || len(raw) != len(img) {
		t.Fatalf("photo read: %v %d", err, len(raw))
	}
	if err := s.SavePhoto("MGR-9999", img); err == nil {
		t.Fatal("unknown member must fail")
	}
	// Deleting the member removes the photo file.
	if err := s.DeleteMember(v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Photo(v.ID); err == nil {
		t.Fatal("photo should be gone with the member")
	}
}
