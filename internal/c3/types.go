// Package c3 implements the ZKTeco C3-series access-control panel wire protocol
// (the "PULL SDK" / C3 communication protocol) spoken over TCP port 4370.
//
// The exact byte layouts implemented in client.go are documented in
// docs/C3-PROTOCOL.md (produced from the panel protocol spec).
package c3

import (
	"context"
	"errors"
	"time"
)

// ErrNotImplemented marks protocol operations not yet wired to the wire format.
var ErrNotImplemented = errors.New("c3: not implemented yet")

// Card is a credential enrolled on the panel. CardNo is the normalized decimal
// card number; the panel itself makes the access decision locally using Doors.
type Card struct {
	PIN      string    `json:"pin"`       // user id / PIN stored on the panel
	CardNo   string    `json:"card_no"`   // normalized decimal Wiegand card number
	Doors    []int     `json:"doors"`     // door numbers this card may open (1..4)
	Timezone int       `json:"timezone"`  // access timezone id (0 = default 24h)
	Start    time.Time `json:"start"`     // validity window start (zero = always)
	End      time.Time `json:"end"`       // validity window end (zero = never expires)
	Disabled bool      `json:"disabled"`  // true = enrolled but denied
}

// Event is a single realtime record streamed from the panel (RTLog): a card
// swipe, a door state change, a passage-sensor trip, or an alarm.
type Event struct {
	Time      time.Time `json:"time"`
	CardNo    string    `json:"card_no"`
	PIN       string    `json:"pin"`
	Door      int       `json:"door"`       // door / relay number
	Reader    int       `json:"reader"`     // Wiegand reader index
	EventType int       `json:"event_type"` // raw event-type code
	EventName string    `json:"event_name"` // human name for EventType
	Verify    int       `json:"verify"`     // verify-mode code (card/pin/fp...)
	InOut     string    `json:"in_out"`     // "in" | "out" | ""
	Raw       string    `json:"raw"`        // hex of the raw record, for debugging
}

// DeviceInfo is a snapshot of panel identity/parameters.
type DeviceInfo struct {
	SerialNumber string `json:"serial_number"`
	DeviceName   string `json:"device_name"`
	Firmware     string `json:"firmware"`
	MAC          string `json:"mac"`
	IPAddress    string `json:"ip_address"`
	Doors        int    `json:"doors"` // number of doors/relays (1/2/4)
}

// Panel is the full capability surface of a C3 controller. The API layer is
// written against this interface so the protocol implementation is swappable
// and testable.
type Panel interface {
	Connect(ctx context.Context) error
	Close() error

	DeviceInfo(ctx context.Context) (DeviceInfo, error)
	GetParams(ctx context.Context, keys ...string) (map[string]string, error)
	SetParams(ctx context.Context, kv map[string]string) error
	SyncTime(ctx context.Context, t time.Time) error

	ListCards(ctx context.Context) ([]Card, error)
	SetCard(ctx context.Context, c Card) error   // create or update
	DeleteCard(ctx context.Context, cardNo string) error

	OpenDoor(ctx context.Context, door, seconds int) error
	SetAux(ctx context.Context, index int, on bool, seconds int) error
	CancelAlarm(ctx context.Context) error

	// Events opens the realtime stream. The channel closes when ctx is done or
	// the connection drops; the caller is expected to reconnect.
	Events(ctx context.Context) (<-chan Event, error)
}
