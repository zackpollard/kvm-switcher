package ikvm

import (
	"encoding/binary"
	"sync/atomic"
)

// seqNum is the shared HID sequence counter.
var seqNum atomic.Uint32

// buildKeyboardReport builds a complete IVTP keyboard HID packet (49 bytes).
// Layout: [IVTPHeader 8B][IUSB Header 32B][Key data 8B + 1B pad]
// Mirrors USBKeyboardRep.report() from JViewer.
func buildKeyboardReport(modifiers byte, keycode byte, pressed bool) []byte {
	buf := make([]byte, 49)

	// IVTP header: type=1 (HID), size=41, status=0
	binary.LittleEndian.PutUint16(buf[0:2], IVTPHIDPkt)
	binary.LittleEndian.PutUint32(buf[2:6], 41)
	binary.LittleEndian.PutUint16(buf[6:8], 0)

	// IUSB signature "IUSB    "
	copy(buf[8:16], []byte(IUSBSignature))

	// IUSB header fields
	buf[16] = 1    // major version
	buf[17] = 0    // minor version
	buf[18] = 0x20 // header size
	buf[19] = 0    // checksum (filled below)

	// Data length (4 bytes LE) = 9 (keyboard report size + type byte)
	binary.LittleEndian.PutUint32(buf[20:24], 9)

	buf[24] = 0                // reserved
	buf[25] = IUSBDeviceKeybd  // device type = keyboard (0x30)
	buf[26] = IUSBProtoKeybdData // protocol = keyboard data (0x10)
	buf[27] = IUSBFromRemote   // direction = from remote (0x80)
	buf[28] = IUSBKeybdDevNum  // device number = 2
	buf[29] = IUSBKeybdIfNum   // interface number = 0
	buf[30] = 0                // reserved
	buf[31] = 0                // reserved

	// Sequence number (4 bytes LE)
	seq := seqNum.Add(1) - 1
	binary.LittleEndian.PutUint32(buf[32:36], seq)

	buf[36] = 0 // reserved
	buf[37] = 0 // reserved
	buf[38] = 0 // reserved
	buf[39] = 0 // reserved

	// Data length byte = 8 (USB keyboard report)
	buf[40] = 8

	// USB HID keyboard report (6 bytes used of 8)
	buf[41] = modifiers
	if pressed {
		buf[42] = 1 // down flag
	} else {
		buf[42] = 0
	}
	buf[43] = keycode
	// buf[44..48] = 0 (padding)

	// Compute checksum: sum bytes [8..39], negate, store at [19]
	var cksum byte
	for i := 8; i < 40; i++ {
		cksum += buf[i]
	}
	buf[19] = -cksum

	return buf
}

// buildAbsMouseReport builds a complete IVTP absolute mouse HID packet (47 bytes).
// x, y are in 0-32767 range. Mirrors USBMouseRep.ABSreport() from JViewer.
func buildAbsMouseReport(buttons byte, x, y uint16, wheel int8) []byte {
	buf := make([]byte, 47)

	// IVTP header: type=1 (HID), size=39, status=0
	binary.LittleEndian.PutUint16(buf[0:2], IVTPHIDPkt)
	binary.LittleEndian.PutUint32(buf[2:6], 39)
	binary.LittleEndian.PutUint16(buf[6:8], 0)

	// IUSB signature
	copy(buf[8:16], []byte(IUSBSignature))

	buf[16] = 1    // major
	buf[17] = 0    // minor
	buf[18] = 0x20 // header size
	buf[19] = 0    // checksum (filled below)

	// Data length = 7
	binary.LittleEndian.PutUint32(buf[20:24], 7)

	buf[24] = 0
	buf[25] = IUSBDeviceMouse    // 0x31
	buf[26] = IUSBProtoMouseData // 0x20
	buf[27] = IUSBFromRemote     // 0x80
	buf[28] = IUSBMouseDevNum    // 2
	buf[29] = IUSBMouseIfNum     // 1
	buf[30] = 0
	buf[31] = 0

	seq := seqNum.Add(1) - 1
	binary.LittleEndian.PutUint32(buf[32:36], seq)

	buf[36] = 0
	buf[37] = 0
	buf[38] = 0
	buf[39] = 0

	// Data length byte = 6
	buf[40] = 6

	// Mouse report: [buttons, x_lo, x_hi, y_lo, y_hi, wheel]
	buf[41] = buttons
	binary.LittleEndian.PutUint16(buf[42:44], x)
	binary.LittleEndian.PutUint16(buf[44:46], y)
	buf[46] = byte(wheel)

	// Checksum
	var cksum byte
	for i := 8; i < 40; i++ {
		cksum += buf[i]
	}
	buf[19] = -cksum

	return buf
}
