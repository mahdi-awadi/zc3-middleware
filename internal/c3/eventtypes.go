package c3

// eventNames maps ZKTeco C3 access-event type codes to human names. Access
// events are 0..99; door/device status events are 100+. Confirmed against the
// live panel: code 27 = "Unregistered Card" (an unknown card read).
var eventNames = map[int]string{
	0:   "Normal Punch Open",
	1:   "Punch during Normal Open Time Zone",
	2:   "First Card Normal Open",
	3:   "Multi-Card Open",
	4:   "Emergency Password Open",
	5:   "Open during Normal Open Time Zone",
	6:   "Linkage Event Triggered",
	7:   "Cancel Alarm",
	8:   "Remote Opening",
	9:   "Remote Closing",
	10:  "Disable Intraday Normal Open Time Zone",
	11:  "Enable Intraday Normal Open Time Zone",
	12:  "Open Auxiliary Output",
	13:  "Close Auxiliary Output",
	14:  "Press Fingerprint Open",
	15:  "Multi-Card Authentication (Punch Card)",
	16:  "Press Fingerprint during Normal Open Time Zone",
	17:  "Card plus Fingerprint Open",
	18:  "First Card Normal Open (Press Fingerprint)",
	19:  "First Card Normal Open (Card plus Fingerprint)",
	20:  "Too Short Punch Interval",
	21:  "Door Inactive Time Zone (Punch Card)",
	22:  "Illegal Time Zone",
	23:  "Access Denied",
	24:  "Anti-Passback",
	25:  "Interlock",
	26:  "Multi-Card Authentication (Press Fingerprint)",
	27:  "Unregistered Card",
	28:  "Opening Timeout",
	29:  "Card Expired",
	30:  "Password Error",
	31:  "Too Short Fingerprint Pressing Interval",
	32:  "Multi-Card Authentication (Card plus Fingerprint)",
	33:  "Fingerprint Expired",
	34:  "Unregistered Fingerprint",
	35:  "Door Setting Open Normal Open Time Zone",
	101: "Duress Password Open",
	102: "Opened Accidentally",
	103: "Duress Fingerprint Open",
	200: "Door Opened Correctly",
	201: "Door Closed Correctly",
	202: "Exit Button Open",
	203: "Multi-Card Open (Punch Card)",
	204: "Normal Open Time Zone Over",
	205: "Remote Normal Opening",
	206: "Device Start",
	220: "Auxiliary Input Disconnected",
	221: "Auxiliary Input Shorted",
	255: "Current Door and Alarm Status",
}

// EventName returns a human name for a C3 event-type code.
func EventName(code int) string {
	if n, ok := eventNames[code]; ok {
		return n
	}
	return "Event " + itoa(code)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
