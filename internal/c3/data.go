package c3

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Data-table access over the C3 protocol.
//
// Schema:  DATATABLE_CFG (0x06, no payload) returns "name=idx,Field=<type><index>,..."
//          one table per line (\n). Types: i=int, s=string, L=long, B=blob.
// Read:    GETDATA (0x08) request = [tableIdx, nFields, fieldIdx..., 0, 0];
//          reply = [tableIdx, fieldCount, fieldIdx..., records] where each field
//          is [size, value] (little-endian int for i/L, ascii for s).

type tableField struct {
	Name  string
	Type  byte // 'i','s','L','B'
	Index int
}

type tableCfg struct {
	Name   string
	Index  int
	Fields []tableField
}

func (t tableCfg) fieldByIndex(i int) (tableField, bool) {
	for _, f := range t.Fields {
		if f.Index == i {
			return f, true
		}
	}
	return tableField{}, false
}

// schema reads and parses DATATABLE_CFG.
func (c *Client) schema(ctx context.Context) (map[string]tableCfg, error) {
	data, err := c.exchange(cmdDataCfg, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]tableCfg{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		name, idxStr, ok := strings.Cut(parts[0], "=")
		if !ok {
			continue
		}
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		tc := tableCfg{Name: name, Index: idx}
		for _, p := range parts[1:] {
			fn, spec, ok := strings.Cut(p, "=")
			if !ok || len(spec) < 2 {
				continue
			}
			fi, err := strconv.Atoi(spec[1:])
			if err != nil {
				continue
			}
			tc.Fields = append(tc.Fields, tableField{Name: fn, Type: spec[0], Index: fi})
		}
		out[name] = tc
	}
	return out, nil
}

// getTable reads all rows of a named table as string-valued maps.
func (c *Client) getTable(ctx context.Context, name string) ([]map[string]string, error) {
	sch, err := c.schema(ctx)
	if err != nil {
		return nil, err
	}
	tc, ok := sch[name]
	if !ok {
		return nil, fmt.Errorf("c3: unknown table %q", name)
	}

	idxs := make([]int, 0, len(tc.Fields))
	for _, f := range tc.Fields {
		idxs = append(idxs, f.Index)
	}
	sort.Ints(idxs)

	payload := make([]byte, 0, len(idxs)+4)
	payload = append(payload, byte(tc.Index), byte(len(idxs)))
	for _, i := range idxs {
		payload = append(payload, byte(i))
	}
	payload = append(payload, 0, 0)

	data, err := c.exchangeData(cmdGetData, payload)
	if err != nil {
		return nil, err
	}
	return decodeTable(tc, data)
}

// decodeTable parses the binary GETDATA reply into rows.
func decodeTable(tc tableCfg, data []byte) ([]map[string]string, error) {
	// An empty table (or an empty paged response) does not carry the usual
	// [tableIdx, fieldCount, ...] header, so a non-matching first byte means
	// "no rows" rather than an error (matches the reference implementation).
	if len(data) < 2 || int(data[0]) != tc.Index {
		return nil, nil
	}
	fcnt := int(data[1])
	if len(data) < 2+fcnt {
		return nil, fmt.Errorf("c3: truncated getdata header")
	}
	respIdx := make([]int, fcnt)
	for i := 0; i < fcnt; i++ {
		respIdx[i] = int(data[2+i])
	}
	body := data[2+fcnt:]

	var rows []map[string]string
	for len(body) > 0 {
		row := map[string]string{}
		ok := true
		for _, fi := range respIdx {
			if len(body) < 1 {
				ok = false
				break
			}
			size := int(body[0])
			if len(body) < 1+size {
				ok = false
				break
			}
			val := body[1 : 1+size]
			f, known := tc.fieldByIndex(fi)
			name := strconv.Itoa(fi)
			if known {
				name = f.Name
			}
			if known && (f.Type == 'i' || f.Type == 'L') {
				row[name] = strconv.FormatUint(leUint(val), 10)
			} else {
				row[name] = strings.TrimRight(string(val), "\x00")
			}
			body = body[1+size:]
		}
		if !ok || len(row) == 0 {
			break
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func leUint(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// ---- typed reads ----

// GetTable exposes any panel table generically (for the /tables endpoint).
func (c *Client) GetTable(ctx context.Context, name string) ([]map[string]string, error) {
	return c.getTable(ctx, name)
}

// TableNames lists the panel's data tables.
func (c *Client) TableNames(ctx context.Context) ([]string, error) {
	sch, err := c.schema(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(sch))
	for n := range sch {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// ListUsers returns enrolled users joined with their access authorizations.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	urows, err := c.getTable(ctx, "user")
	if err != nil {
		return nil, err
	}
	arows, _ := c.getTable(ctx, "userauthorize")
	authByPin := map[string][]AuthRule{}
	for _, a := range arows {
		tz, _ := strconv.Atoi(a["AuthorizeTimezoneId"])
		doors, _ := strconv.Atoi(a["AuthorizeDoorId"])
		authByPin[a["Pin"]] = append(authByPin[a["Pin"]], AuthRule{
			TimezoneID: tz,
			DoorMask:   doors,
			Doors:      doorsFromMask(doors),
		})
	}
	var users []User
	for _, r := range urows {
		g := atoi(r["Group"])
		users = append(users, User{
			UID:      atoi(r["UID"]),
			PIN:      r["Pin"],
			CardNo:   r["CardNo"],
			Name:     r["Name"],
			Role:     RoleName(g),
			Password: r["Password"],
			Group:    g,
			Super:    r["SuperAuthorize"] == "1",
			Disabled: r["Disable"] == "1",
			Auth:     authByPin[r["Pin"]],
		})
	}
	return users, nil
}

// doorsFromMask expands a door bitmask (1=door1,2=door2,4=door3,8=door4) to a list.
func doorsFromMask(mask int) []int {
	var doors []int
	for i := 0; i < 8; i++ {
		if mask&(1<<i) != 0 {
			doors = append(doors, i+1)
		}
	}
	return doors
}

func maskFromDoors(doors []int) int {
	m := 0
	for _, d := range doors {
		if d >= 1 {
			m |= 1 << (d - 1)
		}
	}
	return m
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// encodeRecord builds a binary SETDATA record for a table, validated against
// live hardware: [tableIdx, nFields, fieldIdxs..., [size,value]...] with fields
// in ascending index order. Missing values default to 0/"".
func encodeRecord(tc tableCfg, values map[string]string) []byte {
	fields := make([]tableField, len(tc.Fields))
	copy(fields, tc.Fields)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Index < fields[j].Index })

	out := []byte{byte(tc.Index), byte(len(fields))}
	for _, f := range fields {
		out = append(out, byte(f.Index))
	}
	for _, f := range fields {
		out = append(out, encodeField(f.Type, values[f.Name])...)
	}
	return out
}

// encodeDeleteKey builds a binary DELETE payload keyed by one field.
func encodeDeleteKey(tc tableCfg, keyField, keyValue string) ([]byte, error) {
	var kf tableField
	found := false
	for _, f := range tc.Fields {
		if f.Name == keyField {
			kf, found = f, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("c3: table %q has no field %q", tc.Name, keyField)
	}
	out := []byte{byte(tc.Index), 1, byte(kf.Index)}
	out = append(out, encodeField(kf.Type, keyValue)...)
	return out, nil
}

// encodeField encodes one [size, value] field. Ints are minimal little-endian.
func encodeField(typ byte, val string) []byte {
	if typ == 'i' || typ == 'L' {
		n, _ := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		vb := encodeUintLE(n)
		return append([]byte{byte(len(vb))}, vb...)
	}
	vb := []byte(val)
	return append([]byte{byte(len(vb))}, vb...)
}

func encodeUintLE(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var b []byte
	for v > 0 {
		b = append(b, byte(v))
		v >>= 8
	}
	return b
}

// nextUID returns max(UID)+1 from the user table (for new enrollments).
func (c *Client) nextUID(ctx context.Context) int {
	rows, err := c.getTable(ctx, "user")
	if err != nil {
		return 1
	}
	max := 0
	for _, r := range rows {
		if u := atoi(r["UID"]); u > max {
			max = u
		}
	}
	return max + 1
}
