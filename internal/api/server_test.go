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
)

// fakePanel implements c3.Panel for handler tests.
type fakePanel struct {
	openedDoor, openedSecs int
	deleted                string
	setCard                *c3.Card
	writesOff              bool
}

func (f *fakePanel) Connect(context.Context) error { return nil }
func (f *fakePanel) Close() error                   { return nil }
func (f *fakePanel) DeviceInfo(context.Context) (c3.DeviceInfo, error) {
	return c3.DeviceInfo{SerialNumber: "ACP123", DeviceName: "C3-200", Doors: 2}, nil
}
func (f *fakePanel) GetParams(_ context.Context, keys ...string) (map[string]string, error) {
	return map[string]string{"IPAddress": "192.168.0.102"}, nil
}
func (f *fakePanel) SetParams(context.Context, map[string]string) error { return nil }
func (f *fakePanel) SyncTime(context.Context, time.Time) error          { return nil }
func (f *fakePanel) ListUsers(context.Context) ([]c3.User, error) {
	return []c3.User{{UID: 1, PIN: "1", CardNo: "999", Name: "T", Auth: []c3.AuthRule{{TimezoneID: 1, DoorMask: 3, Doors: []int{1, 2}}}}}, nil
}
func (f *fakePanel) ListCards(context.Context) ([]c3.Card, error) {
	return []c3.Card{{PIN: "1", CardNo: "999", Doors: []int{1, 2}, Timezone: 1}}, nil
}
func (f *fakePanel) TableNames(context.Context) ([]string, error) { return []string{"user", "timezone"}, nil }
func (f *fakePanel) GetTable(_ context.Context, n string) ([]map[string]string, error) {
	return []map[string]string{{"TimezoneId": "1"}}, nil
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
	return c3.PanelStatus{Sensor: "00", Relay: "00", Doors: []c3.DoorState{{Door: 1}}}, nil
}
func (f *fakePanel) Events(ctx context.Context) (<-chan c3.Event, error) {
	ch := make(chan c3.Event)
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

func newTestServer(p c3.Panel, apiKey string) http.Handler {
	return New(p, Options{APIKey: apiKey, DoorDefaultSecs: 5}).Routes()
}

func TestHealthz(t *testing.T) {
	h := newTestServer(&fakePanel{}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
}

func TestDeviceEndpoint(t *testing.T) {
	h := newTestServer(&fakePanel{}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/device", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ACP123") {
		t.Fatalf("device: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOpenDoorDefaultSeconds(t *testing.T) {
	f := &fakePanel{}
	h := newTestServer(f, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/doors/2/open", strings.NewReader("{}")))
	if rec.Code != 200 {
		t.Fatalf("open = %d", rec.Code)
	}
	if f.openedDoor != 2 || f.openedSecs != 5 {
		t.Fatalf("opened door=%d secs=%d, want 2/5", f.openedDoor, f.openedSecs)
	}
}

func TestOpenDoorExplicitSeconds(t *testing.T) {
	f := &fakePanel{}
	h := newTestServer(f, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/doors/1/open", strings.NewReader(`{"seconds":9}`)))
	if f.openedSecs != 9 {
		t.Fatalf("secs=%d want 9", f.openedSecs)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newTestServer(&fakePanel{}, "secret")
	// no token -> 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/device", nil))
	if rec.Code != 401 {
		t.Fatalf("no-auth = %d want 401", rec.Code)
	}
	// healthz is exempt
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz auth-exempt = %d", rec.Code)
	}
	// correct token -> 200
	req := httptest.NewRequest("GET", "/device", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("with-auth = %d", rec.Code)
	}
}

func TestWritesGatedError(t *testing.T) {
	h := newTestServer(&fakePanel{writesOff: true}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/cards", strings.NewReader(`{"card_no":"5","doors":[1]}`)))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "writes disabled") {
		t.Fatalf("gated write: %d %s", rec.Code, rec.Body.String())
	}
}

func TestListCards(t *testing.T) {
	h := newTestServer(&fakePanel{}, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/cards", nil))
	var cards []c3.Card
	if err := json.Unmarshal(rec.Body.Bytes(), &cards); err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].CardNo != "999" {
		t.Fatalf("cards = %v", cards)
	}
}
