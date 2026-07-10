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
	Name     string    `json:"name"`      // cardholder name
	Role     string    `json:"role"`      // student|teacher|administrator|none
	Group    int       `json:"group"`     // panel Group field (role id); set from Role if given
	Doors    []int     `json:"doors"`     // door numbers this card may open (1..4)
	Timezone int       `json:"timezone"`  // access timezone id (0 = default 24h)
	Start    time.Time `json:"start"`     // validity window start (zero = always)
	End      time.Time `json:"end"`       // validity window end (zero = never expires)
	Disabled bool      `json:"disabled"`  // true = enrolled but denied

	// NoPanelAuth enrolls the user WITHOUT a panel authorization row, so the
	// panel denies by default and the middleware enforcement engine is the sole
	// gate (used for enforced roles). Default false = panel-native access.
	NoPanelAuth bool `json:"no_panel_auth,omitempty"`
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

// AuthRule authorizes a PIN to a set of doors during a timezone.
type AuthRule struct {
	TimezoneID int   `json:"timezone_id"`
	DoorMask   int   `json:"door_mask"`
	Doors      []int `json:"doors"`
}

// User is a full enrolled-user record from the panel's user table, joined with
// its access authorizations.
type User struct {
	UID      int        `json:"uid"`
	PIN      string     `json:"pin"`
	CardNo   string     `json:"card_no"`
	Name     string     `json:"name"`
	Role     string     `json:"role"` // derived from Group
	Password string     `json:"password,omitempty"`
	Group    int        `json:"group"`
	Super    bool       `json:"super_authorize"`
	Disabled bool       `json:"disabled"`
	Auth     []AuthRule `json:"auth"`
}

// DoorState is the live state of one door.
type DoorState struct {
	Door     int  `json:"door"`
	SensorOn bool `json:"sensor_on"` // door-contact sensor active
	RelayOn  bool `json:"relay_on"`  // lock relay energized
}

// PanelStatus is a live snapshot of door sensors, relays and alarms.
type PanelStatus struct {
	Time   time.Time   `json:"time"`
	Sensor string      `json:"sensor"`
	Relay  string      `json:"relay"`
	Alarm  string      `json:"alarm"`
	Doors  []DoorState `json:"doors"`
	Raw    string      `json:"raw,omitempty"`
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

	// Data tables (read).
	ListUsers(ctx context.Context) ([]User, error)
	ListCards(ctx context.Context) ([]Card, error)
	TableNames(ctx context.Context) ([]string, error)
	GetTable(ctx context.Context, name string) ([]map[string]string, error)

	// Data tables (write) — gated by ALLOW_WRITES; return ErrWritesDisabled when off.
	SetCard(ctx context.Context, c Card) error   // create or update
	DeleteCard(ctx context.Context, cardNo string) error

	OpenDoor(ctx context.Context, door, seconds int) error
	SetAux(ctx context.Context, index int, on bool, seconds int) error
	CancelAlarm(ctx context.Context) error
	Restart(ctx context.Context) error
	Status(ctx context.Context) (PanelStatus, error)

	// Events opens the realtime stream. The channel closes when ctx is done or
	// the connection drops; the caller is expected to reconnect.
	Events(ctx context.Context) (<-chan Event, error)
}
