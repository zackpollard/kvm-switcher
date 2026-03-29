package ikvm

import (
	"encoding/binary"
	"testing"
)

// ---------------------------------------------------------------------------
// 7. HID reports -- keyboard and mouse
// ---------------------------------------------------------------------------

func TestBuildKeyboardReport(t *testing.T) {
	// Reset sequence counter for deterministic testing
	seqNum.Store(0)

	report := buildKeyboardReport(0x02, 0x04, true) // Shift + 'a', pressed

	// Report should be exactly 49 bytes
	if len(report) != 49 {
		t.Fatalf("Keyboard report length = %d, want 49", len(report))
	}

	// IVTP header: type=1, size=41, status=0
	ivtpType := binary.LittleEndian.Uint16(report[0:2])
	if ivtpType != IVTPHIDPkt {
		t.Errorf("IVTP type = %d, want %d (HIDPkt)", ivtpType, IVTPHIDPkt)
	}
	ivtpSize := binary.LittleEndian.Uint32(report[2:6])
	if ivtpSize != 41 {
		t.Errorf("IVTP pktSize = %d, want 41", ivtpSize)
	}
	ivtpStatus := binary.LittleEndian.Uint16(report[6:8])
	if ivtpStatus != 0 {
		t.Errorf("IVTP status = %d, want 0", ivtpStatus)
	}

	// IUSB signature
	sig := string(report[8:16])
	if sig != IUSBSignature {
		t.Errorf("IUSB signature = %q, want %q", sig, IUSBSignature)
	}

	// IUSB header fields
	if report[16] != 1 { // major version
		t.Errorf("Major version = %d, want 1", report[16])
	}
	if report[17] != 0 { // minor version
		t.Errorf("Minor version = %d, want 0", report[17])
	}
	if report[18] != 0x20 { // header size
		t.Errorf("Header size = 0x%02X, want 0x20", report[18])
	}

	// Data length
	dataLen := binary.LittleEndian.Uint32(report[20:24])
	if dataLen != 9 {
		t.Errorf("Data length = %d, want 9", dataLen)
	}

	// Device fields
	if report[25] != IUSBDeviceKeybd {
		t.Errorf("Device type = 0x%02X, want 0x%02X", report[25], IUSBDeviceKeybd)
	}
	if report[26] != IUSBProtoKeybdData {
		t.Errorf("Protocol = 0x%02X, want 0x%02X", report[26], IUSBProtoKeybdData)
	}
	if report[27] != IUSBFromRemote {
		t.Errorf("Direction = 0x%02X, want 0x%02X", report[27], IUSBFromRemote)
	}
	if report[28] != IUSBKeybdDevNum {
		t.Errorf("Device number = %d, want %d", report[28], IUSBKeybdDevNum)
	}
	if report[29] != IUSBKeybdIfNum {
		t.Errorf("Interface number = %d, want %d", report[29], IUSBKeybdIfNum)
	}

	// Sequence number should be 0 (first call after reset)
	seqVal := binary.LittleEndian.Uint32(report[32:36])
	if seqVal != 0 {
		t.Errorf("Sequence number = %d, want 0", seqVal)
	}

	// Data length byte
	if report[40] != 8 {
		t.Errorf("Data length byte = %d, want 8", report[40])
	}

	// Keyboard HID report: modifiers, down flag, keycode
	if report[41] != 0x02 {
		t.Errorf("Modifiers = 0x%02X, want 0x02", report[41])
	}
	if report[42] != 1 { // pressed=true => down flag = 1
		t.Errorf("Down flag = %d, want 1 (pressed)", report[42])
	}
	if report[43] != 0x04 {
		t.Errorf("Keycode = 0x%02X, want 0x04", report[43])
	}

	// Verify checksum: sum bytes [8..39], negate
	var cksum byte
	for i := 8; i < 40; i++ {
		cksum += report[i]
	}
	expected := -cksum
	// The checksum was computed before storing, so report[19] already has -sum
	// Re-verify: sum of bytes [8..39] including report[19] should be 0
	var verify byte
	for i := 8; i < 40; i++ {
		verify += report[i]
	}
	if verify != 0 {
		t.Errorf("Checksum verification failed: sum of bytes [8..39] = %d, want 0", verify)
	}
	_ = expected
}

func TestBuildKeyboardReportRelease(t *testing.T) {
	seqNum.Store(100)

	report := buildKeyboardReport(0x00, 0x28, false) // Return key, released

	// Down flag should be 0
	if report[42] != 0 {
		t.Errorf("Down flag = %d, want 0 (released)", report[42])
	}
	// Modifiers should be 0
	if report[41] != 0 {
		t.Errorf("Modifiers = 0x%02X, want 0x00", report[41])
	}
	// Keycode
	if report[43] != 0x28 {
		t.Errorf("Keycode = 0x%02X, want 0x28", report[43])
	}
	// Sequence should be 100
	seqVal := binary.LittleEndian.Uint32(report[32:36])
	if seqVal != 100 {
		t.Errorf("Sequence number = %d, want 100", seqVal)
	}
}

func TestBuildAbsMouseReport(t *testing.T) {
	seqNum.Store(0)

	report := buildAbsMouseReport(0x01, 16383, 16383, 0) // left button, center

	// Report should be exactly 47 bytes
	if len(report) != 47 {
		t.Fatalf("Mouse report length = %d, want 47", len(report))
	}

	// IVTP header: type=1, size=39, status=0
	ivtpType := binary.LittleEndian.Uint16(report[0:2])
	if ivtpType != IVTPHIDPkt {
		t.Errorf("IVTP type = %d, want %d", ivtpType, IVTPHIDPkt)
	}
	ivtpSize := binary.LittleEndian.Uint32(report[2:6])
	if ivtpSize != 39 {
		t.Errorf("IVTP pktSize = %d, want 39", ivtpSize)
	}

	// IUSB signature
	sig := string(report[8:16])
	if sig != IUSBSignature {
		t.Errorf("IUSB signature = %q, want %q", sig, IUSBSignature)
	}

	// Data length
	dataLen := binary.LittleEndian.Uint32(report[20:24])
	if dataLen != 7 {
		t.Errorf("Data length = %d, want 7", dataLen)
	}

	// Device fields
	if report[25] != IUSBDeviceMouse {
		t.Errorf("Device type = 0x%02X, want 0x%02X", report[25], IUSBDeviceMouse)
	}
	if report[26] != IUSBProtoMouseData {
		t.Errorf("Protocol = 0x%02X, want 0x%02X", report[26], IUSBProtoMouseData)
	}
	if report[27] != IUSBFromRemote {
		t.Errorf("Direction = 0x%02X, want 0x%02X", report[27], IUSBFromRemote)
	}
	if report[28] != IUSBMouseDevNum {
		t.Errorf("Device number = %d, want %d", report[28], IUSBMouseDevNum)
	}
	if report[29] != IUSBMouseIfNum {
		t.Errorf("Interface number = %d, want %d", report[29], IUSBMouseIfNum)
	}

	// Data length byte
	if report[40] != 6 {
		t.Errorf("Data length byte = %d, want 6", report[40])
	}

	// Mouse report: [buttons, x_lo, x_hi, y_lo, y_hi, wheel]
	if report[41] != 0x01 {
		t.Errorf("Buttons = 0x%02X, want 0x01", report[41])
	}
	x := binary.LittleEndian.Uint16(report[42:44])
	if x != 16383 {
		t.Errorf("X = %d, want 16383", x)
	}
	y := binary.LittleEndian.Uint16(report[44:46])
	if y != 16383 {
		t.Errorf("Y = %d, want 16383", y)
	}
	if report[46] != 0 {
		t.Errorf("Wheel = %d, want 0", report[46])
	}

	// Checksum verification
	var verify byte
	for i := 8; i < 40; i++ {
		verify += report[i]
	}
	if verify != 0 {
		t.Errorf("Checksum verification failed: sum = %d, want 0", verify)
	}
}

func TestBuildAbsMouseReportWithWheel(t *testing.T) {
	seqNum.Store(50)

	report := buildAbsMouseReport(0x04, 0, 32767, -1) // middle button, top-left-ish, scroll down

	if report[41] != 0x04 {
		t.Errorf("Buttons = 0x%02X, want 0x04", report[41])
	}
	x := binary.LittleEndian.Uint16(report[42:44])
	if x != 0 {
		t.Errorf("X = %d, want 0", x)
	}
	y := binary.LittleEndian.Uint16(report[44:46])
	if y != 32767 {
		t.Errorf("Y = %d, want 32767", y)
	}
	if report[46] != 0xFF {
		t.Errorf("Wheel = %d, want 255 (int8(-1) as byte)", report[46])
	}
}

func TestKeyboardSequenceIncrement(t *testing.T) {
	seqNum.Store(0)

	r1 := buildKeyboardReport(0, 0x04, true)
	r2 := buildKeyboardReport(0, 0x04, false)

	seq1 := binary.LittleEndian.Uint32(r1[32:36])
	seq2 := binary.LittleEndian.Uint32(r2[32:36])

	if seq2 != seq1+1 {
		t.Errorf("Sequence not incrementing: seq1=%d, seq2=%d", seq1, seq2)
	}
}

func TestMouseSequenceIncrement(t *testing.T) {
	seqNum.Store(0)

	r1 := buildAbsMouseReport(0, 100, 200, 0)
	r2 := buildAbsMouseReport(0, 300, 400, 0)

	seq1 := binary.LittleEndian.Uint32(r1[32:36])
	seq2 := binary.LittleEndian.Uint32(r2[32:36])

	if seq2 != seq1+1 {
		t.Errorf("Sequence not incrementing: seq1=%d, seq2=%d", seq1, seq2)
	}
}

func TestIUSBConstants(t *testing.T) {
	// Verify IUSB constants match the protocol specification
	if IUSBHeaderSize != 32 {
		t.Errorf("IUSBHeaderSize = %d, want 32", IUSBHeaderSize)
	}
	if IUSBSignature != "IUSB    " {
		t.Errorf("IUSBSignature = %q, want %q", IUSBSignature, "IUSB    ")
	}
	if len(IUSBSignature) != 8 {
		t.Errorf("IUSBSignature length = %d, want 8", len(IUSBSignature))
	}
	if IUSBProtoKeybdData != 0x10 {
		t.Errorf("IUSBProtoKeybdData = 0x%02X, want 0x10", IUSBProtoKeybdData)
	}
	if IUSBProtoMouseData != 0x20 {
		t.Errorf("IUSBProtoMouseData = 0x%02X, want 0x20", IUSBProtoMouseData)
	}
	if IUSBDeviceKeybd != 0x30 {
		t.Errorf("IUSBDeviceKeybd = 0x%02X, want 0x30", IUSBDeviceKeybd)
	}
	if IUSBDeviceMouse != 0x31 {
		t.Errorf("IUSBDeviceMouse = 0x%02X, want 0x31", IUSBDeviceMouse)
	}
	if IUSBFromRemote != 0x80 {
		t.Errorf("IUSBFromRemote = 0x%02X, want 0x80", IUSBFromRemote)
	}
}
