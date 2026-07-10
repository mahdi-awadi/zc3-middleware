package c3

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrWritesDisabled is returned by write operations when ALLOW_WRITES is off.
var ErrWritesDisabled = errors.New("c3: writes disabled (set ALLOW_WRITES=true to enable data-table writes)")

// ListCards returns enrolled credentials (users that carry a card number),
// with their authorized doors. Validated read path.
func (c *Client) ListCards(ctx context.Context) ([]Card, error) {
	users, err := c.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var cards []Card
	for _, u := range users {
		if u.CardNo == "" || u.CardNo == "0" {
			continue
		}
		var doors []int
		tz := 0
		for _, a := range u.Auth {
			doors = append(doors, a.Doors...)
			tz = a.TimezoneID
		}
		cards = append(cards, Card{
			PIN:      u.PIN,
			CardNo:   u.CardNo,
			Doors:    doors,
			Timezone: tz,
			Disabled: u.Disabled,
		})
	}
	return cards, nil
}

// SetCard enrolls or updates a credential: writes a user row and an
// authorization row (PIN -> doors -> timezone). Gated by ALLOW_WRITES.
//
// NOTE: the SETDATA wire format below is implemented from the PULL-SDK
// "field=value" convention but has NOT been validated against live hardware
// (writing to a production access panel was deliberately not brute-forced).
// Validate once with an explicit test before relying on it.
func (c *Client) SetCard(ctx context.Context, card Card) error {
	if !c.allowWrites {
		return ErrWritesDisabled
	}
	if card.PIN == "" {
		// The panel keys users by PIN; derive one from the card if absent.
		card.PIN = card.CardNo
	}
	tz := card.Timezone
	if tz == 0 {
		tz = 1 // default 24h timezone
	}
	// user row
	userRec := map[string]string{
		"Pin":            card.PIN,
		"CardNo":         card.CardNo,
		"Name":           "",
		"Password":       "",
		"Group":          "0",
		"StartTime":      strconv.Itoa(unixOrZero(card.Start)),
		"EndTime":        strconv.Itoa(unixOrZero(card.End)),
		"SuperAuthorize": "0",
		"Disable":        boolBit(card.Disabled),
	}
	if err := c.setData(ctx, "user", userRec); err != nil {
		return err
	}
	// authorization row
	authRec := map[string]string{
		"Pin":                 card.PIN,
		"AuthorizeTimezoneId": strconv.Itoa(tz),
		"AuthorizeDoorId":     strconv.Itoa(maskFromDoors(card.Doors)),
	}
	return c.setData(ctx, "userauthorize", authRec)
}

// DeleteCard revokes a credential by card number. Gated by ALLOW_WRITES.
func (c *Client) DeleteCard(ctx context.Context, cardNo string) error {
	if !c.allowWrites {
		return ErrWritesDisabled
	}
	// resolve the PIN for this card
	users, err := c.ListUsers(ctx)
	if err != nil {
		return err
	}
	pin := ""
	for _, u := range users {
		if u.CardNo == cardNo {
			pin = u.PIN
			break
		}
	}
	if pin == "" {
		return fmt.Errorf("c3: no enrolled card %q", cardNo)
	}
	if err := c.deleteData(ctx, "userauthorize", map[string]string{"Pin": pin}); err != nil {
		return err
	}
	return c.deleteData(ctx, "user", map[string]string{"Pin": pin})
}

// setData writes one record to a table (SETDATA). UNVALIDATED wire format.
func (c *Client) setData(ctx context.Context, table string, rec map[string]string) error {
	payload := append([]byte(table), 0)
	payload = append(payload, []byte(kvRecord(rec))...)
	_, err := c.exchange(cmdSetData, payload)
	return err
}

// deleteData removes records matching rec from a table (DELETEDATA). UNVALIDATED.
func (c *Client) deleteData(ctx context.Context, table string, rec map[string]string) error {
	payload := append([]byte(table), 0)
	payload = append(payload, []byte(kvRecord(rec))...)
	_, err := c.exchange(cmdDelData, payload)
	return err
}

// kvRecord renders a record as tab-separated field=value pairs.
func kvRecord(rec map[string]string) string {
	parts := make([]string, 0, len(rec))
	for k, v := range rec {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "\t")
}
