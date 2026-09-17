package c3

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
			Name:     u.Name,
			Role:     u.Role,
			Group:    u.Group,
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
// Wire format (SETDATA 0x07, binary) validated against live hardware via a
// dummy round-trip: see scripts/c3enroll_validate.py.
func (c *Client) SetCard(ctx context.Context, card Card) error {
	if !c.allowWrites {
		return ErrWritesDisabled
	}
	if card.CardNo == "" {
		return fmt.Errorf("c3: card_no required")
	}
	if card.PIN == "" {
		card.PIN = card.CardNo // panel keys users by PIN
	}
	tz := card.Timezone
	if tz == 0 {
		tz = 1 // default 24h timezone
	}
	// Role (if given) sets the panel Group field; explicit Group otherwise.
	group := card.Group
	if g, ok := GroupForRole(card.Role); ok {
		group = g
	}

	// Preserve an existing user's UID on update; assign the next free UID on
	// insert (the panel keys the user table by UID).
	uid := 0
	users, err := c.ListUsers(ctx)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.PIN == card.PIN || u.CardNo == card.CardNo {
			uid = u.UID
			break
		}
	}
	if uid == 0 {
		uid = c.nextUID(ctx)
	}

	userRec := map[string]string{
		"UID":            strconv.Itoa(uid),
		"Pin":            card.PIN,
		"CardNo":         card.CardNo,
		"Name":           card.Name,
		"Password":       "",
		"Group":          strconv.Itoa(group),
		"StartTime":      strconv.Itoa(unixOrZero(card.Start)),
		"EndTime":        strconv.Itoa(unixOrZero(card.End)),
		"SuperAuthorize": "0",
		"Disable":        boolBit(card.Disabled),
	}
	if err := c.setData(ctx, "user", userRec); err != nil {
		return err
	}
	if card.NoPanelAuth {
		// Enforced-role card: no panel authorization -> panel denies; the
		// middleware engine opens the door when the role's rules are satisfied.
		// Clear any stale authorization so the panel doesn't open on its own.
		_ = c.deleteData(ctx, "userauthorize", "Pin", card.PIN)
		return nil
	}
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
	if err := c.deleteData(ctx, "userauthorize", "Pin", pin); err != nil {
		return err
	}
	return c.deleteData(ctx, "user", "Pin", pin)
}

// setData writes one record to a table (SETDATA 0x07, binary format).
// RevokeAuthorization deletes the panel's own access rule for a PIN, so the
// panel stops opening doors for that card by itself. The user record stays.
func (c *Client) RevokeAuthorization(ctx context.Context, pin string) error {
	if !c.allowWrites {
		return ErrWritesDisabled
	}
	if pin == "" {
		return fmt.Errorf("c3: pin required")
	}
	return c.deleteData(ctx, "userauthorize", "Pin", pin)
}

func (c *Client) setData(ctx context.Context, table string, values map[string]string) error {
	sch, err := c.schema(ctx)
	if err != nil {
		return err
	}
	tc, ok := sch[table]
	if !ok {
		return fmt.Errorf("c3: unknown table %q", table)
	}
	_, err = c.exchange(cmdSetData, encodeRecord(tc, values))
	return err
}

// deleteData removes records matching keyField=keyValue (DELETE 0x09, binary).
func (c *Client) deleteData(ctx context.Context, table, keyField, keyValue string) error {
	sch, err := c.schema(ctx)
	if err != nil {
		return err
	}
	tc, ok := sch[table]
	if !ok {
		return fmt.Errorf("c3: unknown table %q", table)
	}
	payload, err := encodeDeleteKey(tc, keyField, keyValue)
	if err != nil {
		return err
	}
	_, err = c.exchange(cmdDelData, payload)
	return err
}
