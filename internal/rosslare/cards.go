package rosslare

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// state is what the driver remembers, because the AC-215 has no known command to
// read users or cards back: every card it wrote, by panel slot (user slot = card
// slot), the next event number to ask for, and the panel added in the console.
type state struct {
	Panel     *savedPanel     `json:"panel,omitempty"`
	NextEvent *int            `json:"next_event,omitempty"`
	Cards     map[int]c3.Card `json:"cards"`
}

type savedPanel struct {
	Bus     string `json:"bus"`     // adapter host:port
	Address int    `json:"address"` // RS-485 bus address
}

func (c *Client) load() error {
	c.st.Cards = map[int]c3.Card{}
	b, err := os.ReadFile(c.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err == nil {
		err = json.Unmarshal(b, &c.st)
	}
	if err != nil {
		return fmt.Errorf("rosslare: state file %s: %w", c.statePath, err)
	}
	if c.st.Cards == nil {
		c.st.Cards = map[int]c3.Card{}
	}
	return nil
}

// save writes the state file atomically. Caller holds c.smu.
func (c *Client) save() error {
	b, err := json.MarshalIndent(c.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("rosslare: save state: %w", err)
	}
	return os.Rename(tmp, c.statePath)
}

func (c *Client) savedCursor() int {
	c.smu.Lock()
	defer c.smu.Unlock()
	if c.st.NextEvent == nil {
		return -1
	}
	return *c.st.NextEvent
}

func (c *Client) saveCursor(n int) {
	c.smu.Lock()
	defer c.smu.Unlock()
	c.st.NextEvent = &n
	_ = c.save() // best effort: a lost cursor only means replay starts later
}

func (c *Client) cardAt(slot int) (c3.Card, bool) {
	c.smu.Lock()
	defer c.smu.Unlock()
	k, ok := c.st.Cards[slot]
	return k, ok
}

// cards returns the stored cards in slot order with Role derived from Group.
func (c *Client) cards() (slots []int, cards []c3.Card) {
	c.smu.Lock()
	defer c.smu.Unlock()
	for s := range c.st.Cards {
		slots = append(slots, s)
	}
	slices.Sort(slots)
	for _, s := range slots {
		k := c.st.Cards[s]
		k.Role = c3.RoleName(k.Group)
		cards = append(cards, k)
	}
	return slots, cards
}

// ListUsers returns the users this driver enrolled (from the state file).
func (c *Client) ListUsers(ctx context.Context) ([]c3.User, error) {
	slots, cards := c.cards()
	users := make([]c3.User, 0, len(cards))
	for i, k := range cards {
		u := c3.User{UID: slots[i], PIN: k.PIN, CardNo: k.CardNo, Name: k.Name, Role: k.Role, Group: k.Group, Disabled: k.Disabled}
		if !k.NoPanelAuth {
			mask := 0
			for _, d := range k.Doors {
				mask |= 1 << (d - 1)
			}
			u.Auth = []c3.AuthRule{{TimezoneID: k.Timezone, DoorMask: mask, Doors: k.Doors}}
		}
		users = append(users, u)
	}
	return users, nil
}

// ListCards returns the cards this driver enrolled (from the state file).
func (c *Client) ListCards(ctx context.Context) ([]c3.Card, error) {
	_, cards := c.cards()
	if cards == nil {
		cards = []c3.Card{}
	}
	return cards, nil
}

// SetCard enrolls or updates a card: UserData (34) then CodeData (32) in the
// same slot. Doors 1 / 2 map to Reader 1 (in) / Reader 2 (out); each reader gets
// the card's time zone (0 = always) or zone 0 (never) if not listed. Disabled
// and NoPanelAuth cards get zone 0 on both readers, so the panel refuses them
// and the enforcement engine decides. Gated by ALLOW_WRITES.
func (c *Client) SetCard(ctx context.Context, card c3.Card) error {
	if !c.allowWrites {
		return c3.ErrWritesDisabled
	}
	code, err := strconv.ParseUint(card.CardNo, 10, 64)
	if err != nil || code == 0 {
		return fmt.Errorf("rosslare: card_no must be the decimal card number, got %q", card.CardNo)
	}
	card.CardNo = strconv.FormatUint(code, 10)
	if card.PIN == "" {
		card.PIN = card.CardNo
	}
	if g, ok := c3.GroupForRole(card.Role); ok {
		card.Group = g
	}
	if card.Timezone == 0 {
		card.Timezone = 1 // zone 1 = always
	}
	if card.Timezone < 0 || card.Timezone > 255 {
		return fmt.Errorf("rosslare: timezone must be 0..255, got %d", card.Timezone)
	}
	if len(card.Doors) == 0 {
		card.Doors = []int{1, 2} // a turnstile card normally passes both ways
	}
	for _, d := range card.Doors {
		if d != 1 && d != 2 {
			return fmt.Errorf("rosslare: doors must be 1 (Reader 1, in) or 2 (Reader 2, out), got %d", d)
		}
	}

	c.smu.Lock()
	defer c.smu.Unlock()
	slot := c.slotFor(card)
	if _, err := c.exchange(cmdUserData, userRecord(slot, card, time.Now(), c.loc), ansAck); err != nil {
		return err
	}
	if _, err := c.exchange(cmdCodeData, codeRecord(slot, code), ansAck); err != nil {
		return err
	}
	card.Role = ""
	c.st.Cards[slot] = card
	return c.save()
}

// slotFor reuses the slot of the same PIN or card number, else the lowest free
// one (AxTraxNG starts at 1). Caller holds c.smu.
func (c *Client) slotFor(card c3.Card) int {
	for s, k := range c.st.Cards {
		if k.PIN == card.PIN || k.CardNo == card.CardNo {
			return s
		}
	}
	s := 1
	for {
		if _, used := c.st.Cards[s]; !used {
			return s
		}
		s++
	}
}

// DeleteCard clears the card's CodeData and UserData slots (slot + all FF).
func (c *Client) DeleteCard(ctx context.Context, cardNo string) error {
	if !c.allowWrites {
		return c3.ErrWritesDisabled
	}
	if n, err := strconv.ParseUint(cardNo, 10, 64); err == nil {
		cardNo = strconv.FormatUint(n, 10)
	}
	c.smu.Lock()
	defer c.smu.Unlock()
	for s, k := range c.st.Cards {
		if k.CardNo != cardNo {
			continue
		}
		if _, err := c.exchange(cmdCodeData, cleared(s, 12), ansAck); err != nil {
			return err
		}
		if _, err := c.exchange(cmdUserData, cleared(s, 36), ansAck); err != nil {
			return err
		}
		delete(c.st.Cards, s)
		return c.save()
	}
	return fmt.Errorf("rosslare: no enrolled card %q", cardNo)
}

func cleared(slot, n int) []byte {
	b := bytes.Repeat([]byte{0xFF}, n)
	binary.BigEndian.PutUint16(b, uint16(slot))
	return b
}

// userRecord builds UserData (36 bytes):
//
//	0-1 slot | 2-5 keypad PIN (FF = none) | 6-7 valid from | 8-9 valid until (FF FF = never)
//	10-11 from hh mm | 12-13 time zone on Reader 1, Reader 2 | 20 = 80 | 24-25 until hh mm | 28-31 FF
//
// c3's PIN is a user id, not a code to type, so no keypad PIN is written.
func userRecord(slot int, k c3.Card, now time.Time, loc *time.Location) []byte {
	b := make([]byte, 36)
	binary.BigEndian.PutUint16(b, uint16(slot))
	copy(b[2:6], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	from := now.In(loc) // AxTraxNG sends the enrolment day with time 00:00
	if !k.Start.IsZero() {
		from = k.Start.In(loc)
		b[10], b[11] = byte(from.Hour()), byte(from.Minute())
	}
	d := packDate(from)
	b[6], b[7] = d[0], d[1]
	b[8], b[9] = 0xFF, 0xFF
	if !k.End.IsZero() {
		end := k.End.In(loc)
		d := packDate(end)
		b[8], b[9] = d[0], d[1]
		h, m := end.Hour(), end.Minute()
		if h == 0 && m == 0 {
			h, m = 23, 59 // a bare date means through that day, as AxTraxNG sends it
		}
		b[24], b[25] = byte(h), byte(m)
	}
	if !k.Disabled && !k.NoPanelAuth {
		for _, door := range k.Doors {
			b[12+door-1] = byte(k.Timezone)
		}
	}
	b[20] = 0x80 // sent by AxTraxNG, meaning unknown
	copy(b[28:32], []byte{0xFF, 0xFF, 0xFF, 0xFF})
	return b
}

// codeRecord builds CodeData (12 bytes): card slot, user slot, 8-byte code (raw
// Wiegand value, W26 facility 123 card 4660 = 0x7B1234 = card_no 8065588).
func codeRecord(slot int, code uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b, uint16(slot))
	binary.BigEndian.PutUint16(b[2:], uint16(slot))
	binary.BigEndian.PutUint64(b[4:], code)
	return b
}
