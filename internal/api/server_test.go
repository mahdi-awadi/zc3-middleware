package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
	"github.com/awadi/zk-c3-middleware/internal/gym"
)

// fakePanel implements c3.Panel for handler tests.
type fakePanel struct {
	openedDoor, openedSecs int
	deleted                string
	setCard                *c3.Card
	writesOff              bool
}

func (f *fakePanel) Connect(context.Context) error { return nil }
func (f *fakePanel) Close() error                  { return nil }
func (f *fakePanel) DeviceInfo(context.Context) (c3.DeviceInfo, error) {
	return c3.DeviceInfo{SerialNumber: "ACP123", DeviceName: "C3-200", Doors: 2}, nil
}
func (f *fakePanel) GetParams(_ context.Context, keys ...string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (f *fakePanel) SetParams(context.Context, map[string]string) error { return nil }
func (f *fakePanel) SyncTime(context.Context, time.Time) error          { return nil }
func (f *fakePanel) ListUsers(context.Context) ([]c3.User, error)      { return nil, nil }
func (f *fakePanel) ListCards(context.Context) ([]c3.Card, error)      { return nil, nil }
func (f *fakePanel) TableNames(context.Context) ([]string, error)      { return nil, nil }
func (f *fakePanel) GetTable(context.Context, string) ([]map[string]string, error) {
	return nil, nil
}
func (f *fakePanel) SetCard(_ context.Context, c c3.Card) error {
	if f.writesOff {
		return c3.ErrWritesDisabled
	}
	f.setCard = &c
	return nil
}
func (f *fakePanel) DeleteCard(_ context.Context, cardNo string) error {
	if f.writesOff {
		return c3.ErrWritesDisabled
	}
	f.deleted = cardNo
	return nil
}
func (f *fakePanel) OpenDoor(_ context.Context, door, secs int) error {
	f.openedDoor, f.openedSecs = door, secs
	return nil
}
func (f *fakePanel) SetAux(context.Context, int, bool, int) error { return nil }
func (f *fakePanel) CancelAlarm(context.Context) error            { return nil }
func (f *fakePanel) Restart(context.Context) error                { return nil }
func (f *fakePanel) Status(context.Context) (c3.PanelStatus, error) {
	return c3.PanelStatus{}, nil
}
func (f *fakePanel) Events(ctx context.Context) (<-chan c3.Event, error) {
	ch := make(chan c3.Event)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

// newTestServer wires a full server with an in-memory gym store + engine.
func newTestServer(t *testing.T, p c3.Panel, apiKey string) http.Handler {
	t.Helper()
	s := New(p, Options{APIKey: apiKey, DoorDefaultSecs: 5})
	st, err := gym.Open("")
	if err != nil {
		t.Fatal(err)
	}
	var opener gym.DoorOpener
	if fp, ok := p.(*fakePanel); ok {
		opener = fp
	} else {
		opener = p
	}
	s.SetGym(st, gym.NewEngine(st, opener, 5, time.Second, nil))
	return s.Routes()
}

func TestHealthz(t *testing.T) {
	h := newTestServer(t, &fakePanel{}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
}

func TestRootServesGymConsole(t *testing.T) {
	h := newTestServer(t, &fakePanel{}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "أمد جم") {
		t.Fatalf("root: %d", rec.Code)
	}
}

func TestOpenDoorDefaultSeconds(t *testing.T) {
	f := &fakePanel{}
	h := newTestServer(t, f, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/doors/2/open", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("open = %d", rec.Code)
	}
	if f.openedDoor != 2 || f.openedSecs != 5 {
		t.Fatalf("opened door=%d secs=%d, want 2/5", f.openedDoor, f.openedSecs)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newTestServer(t, &fakePanel{}, "secret")
	// gym data routes require the key
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/gym/summary", nil))
	if rec.Code != 401 {
		t.Fatalf("no-auth = %d want 401", rec.Code)
	}
	// the console shell and probes are exempt
	for _, p := range []string{"/", "/healthz", "/config", "/metrics"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Fatalf("%s auth-exempt = %d", p, rec.Code)
		}
	}
	// correct token -> 200
	req := httptest.NewRequest("GET", "/gym/summary", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("with-auth = %d", rec.Code)
	}
}

// TestGymFlow drives the member lifecycle through HTTP: create with card,
// simulate a swipe (granted), then check the summary counted it.
func TestGymFlow(t *testing.T) {
	f := &fakePanel{}
	h := newTestServer(t, f, "")

	body := `{"name":"Test Member","card_no":"12345678","plan":{"months":3,"access":"gym_classes"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/gym/members", strings.NewReader(body)))
	if rec.Code != 201 {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Member      gym.MemberView `json:"member"`
		PanelSynced bool           `json:"panel_synced"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Member.Status != gym.StatusActive || !created.PanelSynced {
		t.Fatalf("created = %+v", created)
	}
	// The card was mirrored to the panel engine-gated.
	if f.setCard == nil || f.setCard.CardNo != "12345678" || !f.setCard.NoPanelAuth {
		t.Fatalf("panel card = %+v", f.setCard)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/gym/simulate",
		strings.NewReader(`{"card_no":"12345678","dry_run":true}`)))
	var u gym.GateUpdate
	if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	if u.Result != gym.ResultGranted || u.Visit != 1 {
		t.Fatalf("simulate = %+v", u)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/gym/summary", nil))
	var sum gym.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.TodayEntries != 1 || sum.ActiveMembers != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

// TestRemovedSurface pins the redesign: legacy admin endpoints are gone.
func TestRemovedSurface(t *testing.T) {
	h := newTestServer(t, &fakePanel{}, "")
	for _, p := range []string{"/device", "/users", "/cards", "/tables", "/roles", "/access/config", "/events", "/events/history", "/ui/", "/ui/gym/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 {
			t.Fatalf("%s = %d, want 404", p, rec.Code)
		}
	}
}
