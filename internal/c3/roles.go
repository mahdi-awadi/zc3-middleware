package c3

import "strings"

// Roles map to the panel's user "Group" field so a card's role is stored
// natively on the device (survives middleware restarts). Time/direction/holiday
// enforcement for each role is applied by the middleware from the event stream.
var roleByGroup = map[int]string{
	0: "none",
	1: "student",
	2: "teacher",
	3: "administrator",
}

var groupByRole = map[string]int{
	"none":          0,
	"student":       1,
	"teacher":       2,
	"administrator": 3,
}

// RoleName returns the role label for a group id.
func RoleName(group int) string {
	if r, ok := roleByGroup[group]; ok {
		return r
	}
	return "group" + itoa(group)
}

// GroupForRole returns the group id for a role label (case-insensitive), and
// whether the role is recognized.
func GroupForRole(role string) (int, bool) {
	g, ok := groupByRole[strings.ToLower(strings.TrimSpace(role))]
	return g, ok
}
