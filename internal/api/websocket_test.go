package api

import (
	"encoding/binary"
	"net/http"
	"testing"

	"github.com/zackpollard/kvm-switcher/internal/models"
)

// --- rewriteVNCServerInit tests ---

func TestRewriteVNCServerInit_ReplacesDesktopName(t *testing.T) {
	// Build a valid ServerInit: 2B width + 2B height + 16B pixel-format + 4B name-len + name
	origName := []byte("iDRAC8 KVM Virtual Console")
	header := make([]byte, 24)
	binary.BigEndian.PutUint16(header[0:2], 1024) // width
	binary.BigEndian.PutUint16(header[2:4], 768)  // height
	// pixel format bytes 4..19 left as zeroes (not relevant to rewrite)
	binary.BigEndian.PutUint32(header[20:24], uint32(len(origName)))
	data := append(header, origName...)

	result := rewriteVNCServerInit("test-session", data)

	// The result should have the new name "Intel(r) AMT KVM"
	newName := "Intel(r) AMT KVM"
	expectedLen := 24 + len(newName)
	if len(result) != expectedLen {
		t.Fatalf("result length = %d, want %d", len(result), expectedLen)
	}

	// Width and height should be preserved
	gotW := binary.BigEndian.Uint16(result[0:2])
	gotH := binary.BigEndian.Uint16(result[2:4])
	if gotW != 1024 || gotH != 768 {
		t.Errorf("dimensions = %dx%d, want 1024x768", gotW, gotH)
	}

	// Name length should match new name
	gotNameLen := binary.BigEndian.Uint32(result[20:24])
	if gotNameLen != uint32(len(newName)) {
		t.Errorf("name length = %d, want %d", gotNameLen, len(newName))
	}

	// Actual name should match
	gotName := string(result[24:])
	if gotName != newName {
		t.Errorf("desktop name = %q, want %q", gotName, newName)
	}
}

func TestRewriteVNCServerInit_PreservesPixelFormat(t *testing.T) {
	// Build a ServerInit with a recognizable pixel format
	header := make([]byte, 24)
	binary.BigEndian.PutUint16(header[0:2], 800) // width
	binary.BigEndian.PutUint16(header[2:4], 600) // height
	// Set some pixel format bytes to non-zero values
	header[4] = 32  // bits-per-pixel
	header[5] = 24  // depth
	header[6] = 0   // big-endian-flag
	header[7] = 1   // true-colour-flag
	header[8] = 0
	header[9] = 0xFF // red-max high byte
	origName := []byte("Test Desktop")
	binary.BigEndian.PutUint32(header[20:24], uint32(len(origName)))
	data := append(header, origName...)

	result := rewriteVNCServerInit("test-session", data)

	// First 20 bytes (pixel format area) should be preserved exactly
	for i := 0; i < 20; i++ {
		if result[i] != data[i] {
			t.Errorf("byte %d: got 0x%02x, want 0x%02x", i, result[i], data[i])
		}
	}
}

func TestRewriteVNCServerInit_TooShort(t *testing.T) {
	// Data shorter than 24 bytes should be returned unchanged
	data := []byte{0x01, 0x02, 0x03}
	result := rewriteVNCServerInit("test-session", data)
	if len(result) != len(data) {
		t.Fatalf("expected unchanged data, got length %d", len(result))
	}
	for i := range data {
		if result[i] != data[i] {
			t.Errorf("byte %d changed: got 0x%02x, want 0x%02x", i, result[i], data[i])
		}
	}
}

func TestRewriteVNCServerInit_NameLengthMismatch(t *testing.T) {
	// Header claims a name length larger than what's actually present
	header := make([]byte, 24)
	binary.BigEndian.PutUint32(header[20:24], 100) // claims 100 bytes of name
	// But only provide 5 bytes of name
	data := append(header, []byte("short")...)

	result := rewriteVNCServerInit("test-session", data)

	// Should return data unchanged since name-length doesn't match actual data
	if len(result) != len(data) {
		t.Fatalf("expected unchanged data (length %d), got length %d", len(data), len(result))
	}
}

func TestRewriteVNCServerInit_EmptyName(t *testing.T) {
	// ServerInit with zero-length desktop name
	header := make([]byte, 24)
	binary.BigEndian.PutUint16(header[0:2], 640)
	binary.BigEndian.PutUint16(header[2:4], 480)
	binary.BigEndian.PutUint32(header[20:24], 0) // zero-length name

	result := rewriteVNCServerInit("test-session", header)

	newName := "Intel(r) AMT KVM"
	gotNameLen := binary.BigEndian.Uint32(result[20:24])
	if gotNameLen != uint32(len(newName)) {
		t.Errorf("name length = %d, want %d", gotNameLen, len(newName))
	}
	gotName := string(result[24:])
	if gotName != newName {
		t.Errorf("desktop name = %q, want %q", gotName, newName)
	}
}

// --- rewriteVNCClientMessage tests ---

func TestRewriteVNCClientMessage_SetEncodings(t *testing.T) {
	// Build a SetEncodings message with many encodings
	// Type 2, padding 1 byte, number-of-encodings 2 bytes, then 4 bytes each
	encodings := []int32{0, 1, 5, 6, 7, 16, -239, -223} // various encodings
	data := make([]byte, 4+len(encodings)*4)
	data[0] = 2 // SetEncodings
	binary.BigEndian.PutUint16(data[2:4], uint16(len(encodings)))
	for i, enc := range encodings {
		binary.BigEndian.PutUint32(data[4+i*4:], uint32(enc))
	}

	result := rewriteVNCClientMessage("test-session", data)

	// Should be rewritten to exactly 3 encodings: Raw(0), CopyRect(1), DesktopSize(-223)
	if result[0] != 2 {
		t.Errorf("message type = %d, want 2", result[0])
	}
	gotCount := binary.BigEndian.Uint16(result[2:4])
	if gotCount != 3 {
		t.Errorf("encoding count = %d, want 3", gotCount)
	}
	expectedLen := 4 + 3*4
	if len(result) != expectedLen {
		t.Fatalf("result length = %d, want %d", len(result), expectedLen)
	}

	// Verify the specific encodings
	wantEncodings := []int32{0, 1, -223}
	for i, want := range wantEncodings {
		got := int32(binary.BigEndian.Uint32(result[4+i*4:]))
		if got != want {
			t.Errorf("encoding[%d] = %d, want %d", i, got, want)
		}
	}
}

func TestRewriteVNCClientMessage_NonSetEncodings(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"SetPixelFormat", append([]byte{0}, make([]byte, 19)...)},        // type 0
		{"KeyEvent", []byte{4, 1, 0, 0, 0, 0, 0, 0x41}},                 // type 4
		{"PointerEvent", []byte{5, 0x01, 0x00, 0x64, 0x00, 0xC8}},       // type 5
		{"FramebufferUpdateRequest", []byte{3, 0, 0, 0, 0, 0, 4, 0, 3}}, // type 3
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := rewriteVNCClientMessage("test-session", tt.data)
			if len(result) != len(tt.data) {
				t.Fatalf("length changed: got %d, want %d", len(result), len(tt.data))
			}
			for i := range tt.data {
				if result[i] != tt.data[i] {
					t.Errorf("byte %d: got 0x%02x, want 0x%02x", i, result[i], tt.data[i])
				}
			}
		})
	}
}

func TestRewriteVNCClientMessage_EmptyData(t *testing.T) {
	result := rewriteVNCClientMessage("test-session", []byte{})
	if len(result) != 0 {
		t.Errorf("expected empty result, got %d bytes", len(result))
	}
}

func TestRewriteVNCClientMessage_SetEncodingsTooShort(t *testing.T) {
	// Type 2 but only 3 bytes (less than 4) -- should pass through unchanged
	data := []byte{2, 0, 0}
	result := rewriteVNCClientMessage("test-session", data)
	if len(result) != len(data) {
		t.Fatalf("length changed: got %d, want %d", len(result), len(data))
	}
}

// --- wsUpgrader CheckOrigin tests ---

func newTestServerWithOrigins(origins []string) *Server {
	cfg := &models.AppConfig{
		Settings: models.Settings{
			CORSOrigins: origins,
		},
	}
	return &Server{Config: cfg}
}

func TestWSUpgrader_WildcardAllowsAny(t *testing.T) {
	s := newTestServerWithOrigins([]string{"*"})
	upgrader := s.wsUpgrader()

	// With wildcard, any origin should be accepted
	tests := []string{
		"https://example.com",
		"http://localhost:3000",
		"",
		"https://anything.example.org",
	}
	for _, origin := range tests {
		r, _ := http.NewRequest("GET", "/ws", nil)
		r.Header.Set("Origin", origin)
		if !upgrader.CheckOrigin(r) {
			t.Errorf("wildcard should allow origin %q", origin)
		}
	}
}

func TestWSUpgrader_ExactMatchAllows(t *testing.T) {
	s := newTestServerWithOrigins([]string{"https://app.example.com", "https://dev.example.com"})
	upgrader := s.wsUpgrader()

	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Origin", "https://app.example.com")
	if !upgrader.CheckOrigin(r) {
		t.Error("exact match should be allowed")
	}

	r2, _ := http.NewRequest("GET", "/ws", nil)
	r2.Header.Set("Origin", "https://dev.example.com")
	if !upgrader.CheckOrigin(r2) {
		t.Error("second origin should be allowed")
	}
}

func TestWSUpgrader_MismatchRejects(t *testing.T) {
	s := newTestServerWithOrigins([]string{"https://app.example.com"})
	upgrader := s.wsUpgrader()

	tests := []string{
		"https://evil.com",
		"http://app.example.com", // scheme mismatch
		"https://app.example.com:8080",
		"",
	}
	for _, origin := range tests {
		r, _ := http.NewRequest("GET", "/ws", nil)
		r.Header.Set("Origin", origin)
		if upgrader.CheckOrigin(r) {
			t.Errorf("origin %q should be rejected", origin)
		}
	}
}

func TestWSUpgrader_EmptyOriginsRejectsAll(t *testing.T) {
	s := newTestServerWithOrigins([]string{})
	upgrader := s.wsUpgrader()

	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Origin", "https://example.com")
	if upgrader.CheckOrigin(r) {
		t.Error("empty origins list should reject all")
	}
}

func TestWSUpgrader_WildcardAmongOthers(t *testing.T) {
	// If wildcard is present alongside specific origins, wildcard wins
	s := newTestServerWithOrigins([]string{"https://specific.com", "*"})
	upgrader := s.wsUpgrader()

	r, _ := http.NewRequest("GET", "/ws", nil)
	r.Header.Set("Origin", "https://random-origin.dev")
	if !upgrader.CheckOrigin(r) {
		t.Error("wildcard among other origins should still allow any origin")
	}
}

func TestWSUpgrader_SubprotocolIsBinary(t *testing.T) {
	s := newTestServerWithOrigins([]string{"*"})
	upgrader := s.wsUpgrader()

	if len(upgrader.Subprotocols) != 1 || upgrader.Subprotocols[0] != "binary" {
		t.Errorf("subprotocols = %v, want [binary]", upgrader.Subprotocols)
	}
}

func TestWSUpgrader_BufferSizes(t *testing.T) {
	s := newTestServerWithOrigins([]string{"*"})
	upgrader := s.wsUpgrader()

	if upgrader.ReadBufferSize != 4096 {
		t.Errorf("ReadBufferSize = %d, want 4096", upgrader.ReadBufferSize)
	}
	if upgrader.WriteBufferSize != 4096 {
		t.Errorf("WriteBufferSize = %d, want 4096", upgrader.WriteBufferSize)
	}
}
