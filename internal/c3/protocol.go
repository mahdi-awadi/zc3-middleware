package c3

// Wire protocol for the ZKTeco C3 PULL protocol, validated against live
// hardware. Frame:  AA 01 <cmd> <len_lo> <len_hi> <data> <crc_lo> <crc_hi> 55
// CRC-16/ARC (reflected, poly 0xA001, init 0x0000) over VER..last-data-byte.
//
// This firmware rejects session mode (0x76 -> 0xC9 ERROR); we use session-less
// mode: connect with 0x01 and carry no session header on subsequent commands.

const (
	frameStart = 0xAA
	frameEnd   = 0x55
	protoVer   = 0x01

	cmdConnect    = 0x01 // session-less connect
	cmdDisconnect = 0x02
	cmdDateTime   = 0x03 // set panel clock
	cmdGetParam   = 0x04 // read device parameters (k=v)
	cmdControl    = 0x05 // control device (open door, aux, cancel alarm)
	cmdDataCfg    = 0x06 // data-table config (field layout)
	cmdSetData    = 0x07 // write a data table (binary, validated via dummy round-trip)
	cmdGetData    = 0x08 // read a data table
	cmdDelData    = 0x09 // delete from a data table (binary key, validated)
	cmdRTLogBin   = 0x0B // realtime log (binary records)
	cmdRTLogKV    = 0x79 // realtime log (ascii key=value) -- preferred
	replyData     = 0x0D // paged data chunk (large table reads); more follows
	replyOK       = 0xC8
	replyError    = 0xC9

	// ControlOperation
	ctlOutput      = 1 // pulse a door/aux output
	ctlCancelAlarm = 2
	ctlRestart     = 3
)

// crc16 computes CRC-16/ARC over b (reflected, poly 0xA001, init 0x0000).
func crc16(b []byte) uint16 {
	var crc uint16
	for _, x := range b {
		crc ^= uint16(x)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// encodeFrame builds a complete session-less request frame for cmd + payload.
func encodeFrame(cmd byte, payload []byte) []byte {
	n := len(payload)
	body := make([]byte, 0, 4+n)
	body = append(body, protoVer, cmd, byte(n), byte(n>>8))
	body = append(body, payload...)
	crc := crc16(body)
	out := make([]byte, 0, len(body)+4)
	out = append(out, frameStart)
	out = append(out, body...)
	out = append(out, byte(crc), byte(crc>>8), frameEnd)
	return out
}
