package c3

import (
	"context"
	"fmt"
)

// Card management writes to the panel's live user and authorization tables.
// These paths are intentionally gated until the table field layout is validated
// against the hardware (DATATABLE_CFG 0x06 -> field indices, then SETDATA), so
// we never push malformed records to a live access-control device.
//
// The read/validate/write sequence is:
//  1. DATATABLE_CFG "user" and "userauthorize" -> field-name -> index map
//  2. GETDATA to enumerate existing cards
//  3. SETDATA with a tab-delimited record using the discovered field indices
//
// Tracked as the next milestone; OpenDoor / Events / params / time are live.

var errCardWriteGated = fmt.Errorf("c3: card table read/write not yet enabled (pending table-layout validation against hardware)")

func (c *Client) ListCards(ctx context.Context) ([]Card, error) {
	return nil, errCardWriteGated
}

func (c *Client) SetCard(ctx context.Context, card Card) error {
	return errCardWriteGated
}

func (c *Client) DeleteCard(ctx context.Context, cardNo string) error {
	return errCardWriteGated
}
