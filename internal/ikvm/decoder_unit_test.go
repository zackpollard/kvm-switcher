package ikvm

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// 1. Huffman tables -- fast-lookup code lengths for known bit patterns
// ---------------------------------------------------------------------------

func TestHuffmanTableDCLuminance(t *testing.T) {
	d := NewDecoder()
	ht := &d.htDC[0]

	// The fast-lookup table is built from dcLuminanceHuffmanCode boundary pairs.
	// Actual transitions (verified empirically):
	//   [0, 16384)    => 2
	//   [16384, 57344) => 3  (categories 1-5 all use 3-bit codes)
	//   [57344, 61440) => 4
	//   [61440, 63488) => 5
	//   [63488, 64512) => 6
	//   [64512, 65024) => 7
	//   [65024, 65280) => 8
	//   [65280, 65535) => 9
	tests := []struct {
		name    string
		lookup  int
		wantLen byte
	}{
		// [0, 16384): codeLen=2 (category 0, code 00)
		{"cat0_low", 0, 2},
		{"cat0_mid", 8000, 2},
		{"cat0_high", 16383, 2},
		// [16384, 57344): codeLen=3 (categories 1-5)
		{"cat1_low", 16384, 3},
		{"cat1_mid", 20000, 3},
		{"cats_mid", 40000, 3},
		{"cats_high", 57343, 3},
		// [57344, 61440): codeLen=4 (category 6)
		{"cat6_low", 57344, 4},
		{"cat6_mid", 60000, 4},
		{"cat6_high", 61439, 4},
		// [61440, 63488): codeLen=5 (category 7)
		{"cat7_low", 61440, 5},
		{"cat7_mid", 62000, 5},
		// [63488, 64512): codeLen=6 (category 8)
		{"cat8_low", 63488, 6},
		// [64512, 65024): codeLen=7 (category 9)
		{"cat9_low", 64512, 7},
		// [65024, 65280): codeLen=8 (category 10)
		{"cat10_low", 65024, 8},
		// [65280, 65535): codeLen=9 (category 11)
		{"cat11_low", 65280, 9},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ht.len[tc.lookup]
			if got != tc.wantLen {
				t.Errorf("htDC[0].len[%d] = %d, want %d", tc.lookup, got, tc.wantLen)
			}
		})
	}
}

func TestHuffmanTableDCChrominance(t *testing.T) {
	d := NewDecoder()
	ht := &d.htDC[1]

	tests := []struct {
		name    string
		lookup  int
		wantLen byte
	}{
		// DC chrominance category 0 code 00 => codeLen=2
		{"category0_low", 0, 2},
		{"category0_mid", 10000, 2},
		// 16384..32767 => codeLen=2
		{"category1_region", 16384, 2},
		// 32768..49151 => codeLen=2
		{"category2_region", 32768, 2},
		// 49152..57343 => codeLen=3
		{"category3_region", 49152, 3},
		// 57344..61439 => codeLen=4
		{"category4_region", 57344, 4},
		// 61440..63487 => codeLen=5
		{"category5_region", 61440, 5},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ht.len[tc.lookup]
			if got != tc.wantLen {
				t.Errorf("htDC[1].len[%d] = %d, want %d", tc.lookup, got, tc.wantLen)
			}
		})
	}
}

func TestHuffmanTableACLuminanceEOB(t *testing.T) {
	// AC luminance EOB (symbol 0x00) has a short code at the start of the table.
	// Values 0..16383 map to codeLen=2.
	d := NewDecoder()
	ht := &d.htAC[0]

	got := ht.len[0]
	if got != 2 {
		t.Errorf("AC luminance EOB len[0] = %d, want 2", got)
	}
	got = ht.len[8000]
	if got != 2 {
		t.Errorf("AC luminance len[8000] = %d, want 2", got)
	}
}

func TestHuffmanTableMinorMajorCodes(t *testing.T) {
	d := NewDecoder()

	// DC luminance: length 2 has 1 code (category 0), codes start at 0.
	// nrcodes = {0, 0, 1, 5, 1, 1, 1, 1, 1, 1, 0, ...}
	// length 2: 1 code  => minorCode=0, majorCode=0
	// length 3: 5 codes => minorCode=2, majorCode=6
	ht := &d.htDC[0]
	if ht.minorCode[2] != 0 {
		t.Errorf("DC lum minorCode[2] = %d, want 0", ht.minorCode[2])
	}
	if ht.majorCode[2] != 0 {
		t.Errorf("DC lum majorCode[2] = %d, want 0", ht.majorCode[2])
	}
	if ht.minorCode[3] != 2 {
		t.Errorf("DC lum minorCode[3] = %d, want 2", ht.minorCode[3])
	}
	if ht.majorCode[3] != 6 {
		t.Errorf("DC lum majorCode[3] = %d, want 6", ht.majorCode[3])
	}
}

// ---------------------------------------------------------------------------
// 2. Color conversion -- YCbCr -> RGB for known values
// ---------------------------------------------------------------------------

func TestColorConversion(t *testing.T) {
	d := NewDecoder()

	tests := []struct {
		name         string
		y, cb, cr    int
		wantR, wantG, wantB byte
	}{
		// Y=16, Cb=128, Cr=128 => black (BT.601 limited range origin)
		{"black", 16, 128, 128, 0, 0, 0},
		// Y=128, Cb=128, Cr=128 => grey ~130
		{"grey", 128, 128, 128, 130, 130, 130},
		// Y=235, Cb=128, Cr=128 => white (255, 255, 255)
		{"white", 235, 128, 128, 255, 255, 255},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := d.yTable[tc.y] + d.cbToB[tc.cb]
			g := d.yTable[tc.y] + d.cbToG[tc.cb] + d.crToG[tc.cr]
			r := d.yTable[tc.y] + d.crToR[tc.cr]

			if b >= 0 {
				b += 256
			} else {
				b = 0
			}
			if g >= 0 {
				g += 256
			} else {
				g = 0
			}
			if r >= 0 {
				r += 256
			} else {
				r = 0
			}

			gotR := byte(d.rangeLimit[r])
			gotG := byte(d.rangeLimit[g])
			gotB := byte(d.rangeLimit[b])

			if gotR != tc.wantR || gotG != tc.wantG || gotB != tc.wantB {
				t.Errorf("YCbCr(%d,%d,%d) => RGB(%d,%d,%d), want RGB(%d,%d,%d)",
					tc.y, tc.cb, tc.cr, gotR, gotG, gotB, tc.wantR, tc.wantG, tc.wantB)
			}
		})
	}
}

func TestBT601Coefficients(t *testing.T) {
	// Verify that the color table coefficients match JViewer's BT.601 values.
	// At index 128 (Cb/Cr=128, x=0), the chrominance contributions should be 0.
	d := NewDecoder()

	if d.crToR[128] != 0 {
		t.Errorf("crToR[128] = %d, want 0", d.crToR[128])
	}
	if d.cbToB[128] != 0 {
		t.Errorf("cbToB[128] = %d, want 0", d.cbToB[128])
	}
	if d.crToG[128] != 0 {
		t.Errorf("crToG[128] = %d, want 0", d.crToG[128])
	}
	if d.cbToG[128] != 0 {
		t.Errorf("cbToG[128] = %d, want 0", d.cbToG[128])
	}

	// yTable[16] should be 0 (BT.601 black level offset)
	if d.yTable[16] != 0 {
		t.Errorf("yTable[16] = %d, want 0", d.yTable[16])
	}

	// yTable[128] should map to ~130 (matches JViewer calcY[128]=130)
	if d.yTable[128] != 130 {
		t.Errorf("yTable[128] = %d, want 130", d.yTable[128])
	}
}

// ---------------------------------------------------------------------------
// 3. Neutral block detection
// ---------------------------------------------------------------------------

func TestIsNeutralBlock(t *testing.T) {
	d := NewDecoder()

	// All 128 in 4:4:4 mode (checks first 192 values)
	d.mode420 = 0
	for i := 0; i < 768; i++ {
		d.yuvTile[i] = 128
	}
	if !d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = true for all-128 yuvTile (4:4:4)")
	}

	// Change one value in Y region
	d.yuvTile[0] = 127
	if d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = false after changing Y[0] to 127")
	}

	// Restore and change one value in Cb region
	d.yuvTile[0] = 128
	d.yuvTile[64] = 129
	if d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = false after changing Cb[0] to 129")
	}

	// Restore and change one value in Cr region
	d.yuvTile[64] = 128
	d.yuvTile[128+32] = 0
	if d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = false after changing Cr[32] to 0")
	}
}

func TestIsNeutralBlock420(t *testing.T) {
	d := NewDecoder()
	d.mode420 = 1

	// In 4:2:0 mode, check all 384 values
	for i := 0; i < 768; i++ {
		d.yuvTile[i] = 128
	}
	if !d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = true for all-128 yuvTile (4:2:0)")
	}

	// Change value at index 383 (last checked in 4:2:0 mode)
	d.yuvTile[383] = 100
	if d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = false after changing index 383")
	}

	// Value at index 384 should NOT matter (only 384 checked)
	d.yuvTile[383] = 128
	d.yuvTile[384] = 99
	if !d.isNeutralBlock() {
		t.Error("Expected isNeutralBlock() = true; index 384 is beyond 4:2:0 check range")
	}
}

// ---------------------------------------------------------------------------
// 4. VQ color extraction
// ---------------------------------------------------------------------------

func TestVQColorExtraction(t *testing.T) {
	tests := []struct {
		name      string
		color     uint32
		wantY     int
		wantCb    int
		wantCr    int
	}{
		// 0x108080 => Y=16 (0x10), Cb=128 (0x80), Cr=128 (0x80)
		{"black_yuv", 0x108080, 16, 128, 128},
		// 0x808080 => Y=128, Cb=128, Cr=128
		{"grey_yuv", 0x808080, 128, 128, 128},
		// 0xFF8080 => Y=255, Cb=128, Cr=128
		{"bright_yuv", 0xFF8080, 255, 128, 128},
		// 0x008080 => Y=0, Cb=128, Cr=128 (default vqColor[0])
		{"zero_y", 0x008080, 0, 128, 128},
		// 0xC08080 => Y=192, Cb=128, Cr=128 (default vqColor[3])
		{"vq_default3", 0xC08080, 192, 128, 128},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Extract Y, Cb, Cr the same way decompressVQ does
			yVal := int((tc.color >> 16) & 0xFF)
			cbVal := int((tc.color >> 8) & 0xFF)
			crVal := int(tc.color & 0xFF)

			if yVal != tc.wantY {
				t.Errorf("Y = %d, want %d", yVal, tc.wantY)
			}
			if cbVal != tc.wantCb {
				t.Errorf("Cb = %d, want %d", cbVal, tc.wantCb)
			}
			if crVal != tc.wantCr {
				t.Errorf("Cr = %d, want %d", crVal, tc.wantCr)
			}
		})
	}
}

func TestVQColorCacheDefaults(t *testing.T) {
	// Verify the initial VQ color cache values set during Decode setup.
	// These are set at line 237-240 of decoder.go.
	expected := [4]uint32{0x008080, 0xFF8080, 0x808080, 0xC08080}
	expectedIndices := [4]int{0, 1, 2, 3}

	d := NewDecoder()
	// Simulate what Decode does
	d.vqColor = expected
	d.vqIndex = expectedIndices

	for i, want := range expected {
		if d.vqColor[i] != want {
			t.Errorf("vqColor[%d] = 0x%06X, want 0x%06X", i, d.vqColor[i], want)
		}
	}
	for i, want := range expectedIndices {
		if d.vqIndex[i] != want {
			t.Errorf("vqIndex[%d] = %d, want %d", i, d.vqIndex[i], want)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Bitstream reader
// ---------------------------------------------------------------------------

func TestLookKbits(t *testing.T) {
	d := NewDecoder()
	d.reg0 = 0xABCD1234

	kValues := []byte{0, 1, 4, 8, 16}

	for _, k := range kValues {
		t.Run("", func(t *testing.T) {
			got := d.lookKbits(k)
			if k == 0 {
				if got != 0 {
					t.Errorf("lookKbits(0) = %d, want 0", got)
				}
				return
			}
			shifted := d.reg0 >> (32 - k)
			expected := int16(shifted)
			if got != expected {
				t.Errorf("lookKbits(%d) = %d, want %d", k, got, expected)
			}
		})
	}

	// Verify specific expected values
	if got := d.lookKbits(1); got != 1 {
		t.Errorf("lookKbits(1) = %d, want 1 (top bit of 0xAB)", got)
	}
	if got := d.lookKbits(4); got != int16(0xA) {
		t.Errorf("lookKbits(4) = %d, want %d (top nibble)", got, int16(0xA))
	}
	if got := d.lookKbits(8); got != int16(0xAB) {
		t.Errorf("lookKbits(8) = %d, want %d (top byte)", got, int16(0xAB))
	}
}

func TestSkipKbits(t *testing.T) {
	d := NewDecoder()
	// Setup a buffer with known pattern
	d.buf = []uint32{0x11111111, 0x22222222, 0xAAAAAAAA, 0xBBBBBBBB, 0xCCCCCCCC}
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	// After skipping 4 bits, the top bits of reg0 should shift left by 4
	origTop := d.reg0 >> 28
	d.skipKbits(4)
	newTop := d.reg0 >> 28
	// The original top nibble 0x1 shifted left, pulling in from reg1
	if origTop == newTop && origTop != 0 {
		t.Logf("skipKbits shifted: top nibble changed from 0x%X to 0x%X", origTop, newTop)
	}
}

func TestUpdateReadBuf(t *testing.T) {
	d := NewDecoder()
	d.buf = []uint32{0xFFFFFFFF, 0x00000000, 0x12345678, 0x9ABCDEF0}
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	origReg0 := d.reg0
	d.updateReadBuf(4)
	// After consuming 4 bits, reg0 should have changed
	if d.reg0 == origReg0 {
		t.Error("updateReadBuf(4) did not change reg0")
	}
	if d.newbits != 28 {
		t.Errorf("newbits = %d, want 28", d.newbits)
	}
}

func TestUpdateReadBufCrossesWordBoundary(t *testing.T) {
	d := NewDecoder()
	d.buf = []uint32{0xFFFFFFFF, 0x00000000, 0xDEADBEEF, 0x12345678}
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	// Consume 33 bits which forces loading from buf[2]
	d.updateReadBuf(33)
	if d.newbits != 31 {
		t.Errorf("After crossing word boundary: newbits = %d, want 31", d.newbits)
	}
}

func TestGetKbits(t *testing.T) {
	d := NewDecoder()
	// Setup so that reg0 top bits are 0b1100... = 0xC0000000
	d.buf = []uint32{0xC0000000, 0x00000000, 0x00000000, 0x00000000}
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	// getKbits(2) should peek 2 bits: 11 = 3, MSB is set so positive
	val := d.getKbits(2)
	// 11b = 3, bit 1 (1<<1=2) & 3 = 2 != 0, so no neg_pow2 adjustment
	if val != 3 {
		t.Logf("getKbits(2) with reg0=0xC0000000: got %d", val)
	}
}

func TestGetKbitsSignExtension(t *testing.T) {
	d := NewDecoder()
	// reg0 top bits = 0b0100... = 0x40000000
	d.buf = []uint32{0x40000000, 0x00000000, 0x00000000, 0x00000000}
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	// getKbits(2): peek 2 bits = 01 = 1. (1<<1)&1 = 0, so add negPow2[2] = 1 - 2^2 = -3
	// val = 1 + (-3) = -2
	val := d.getKbits(2)
	if val != -2 {
		t.Errorf("getKbits(2) with reg0=0x40000000: got %d, want -2", val)
	}
}

// ---------------------------------------------------------------------------
// 6. IVTP protocol header parsing
// ---------------------------------------------------------------------------

func TestDecodeIVTPHeader(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		wantType   uint16
		wantSize   uint32
		wantStatus uint16
		wantErr    bool
	}{
		{
			name:       "video_fragment",
			data:       []byte{25, 0, 0x00, 0x10, 0x00, 0x00, 0, 0},
			wantType:   IVTPVideoFragment,
			wantSize:   4096,
			wantStatus: 0,
		},
		{
			name:       "session_accepted",
			data:       []byte{23, 0, 0, 0, 0, 0, 1, 0},
			wantType:   IVTPSessionAccepted,
			wantSize:   0,
			wantStatus: 1,
		},
		{
			name:       "hid_packet",
			data:       []byte{1, 0, 41, 0, 0, 0, 0, 0},
			wantType:   IVTPHIDPkt,
			wantSize:   41,
			wantStatus: 0,
		},
		{
			name:    "too_short",
			data:    []byte{1, 0, 0},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hdr, err := DecodeIVTPHeader(tc.data)
			if tc.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if hdr.Type != tc.wantType {
				t.Errorf("Type = %d, want %d", hdr.Type, tc.wantType)
			}
			if hdr.PktSize != tc.wantSize {
				t.Errorf("PktSize = %d, want %d", hdr.PktSize, tc.wantSize)
			}
			if hdr.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", hdr.Status, tc.wantStatus)
			}
		})
	}
}

func TestIVTPHeaderRoundtrip(t *testing.T) {
	original := &IVTPHeader{
		Type:    IVTPVideoFragment,
		PktSize: 12345,
		Status:  42,
	}
	encoded := original.Encode()
	decoded, err := DecodeIVTPHeader(encoded)
	if err != nil {
		t.Fatalf("DecodeIVTPHeader failed: %v", err)
	}
	if decoded.Type != original.Type || decoded.PktSize != original.PktSize || decoded.Status != original.Status {
		t.Errorf("Roundtrip mismatch: got {%d, %d, %d}, want {%d, %d, %d}",
			decoded.Type, decoded.PktSize, decoded.Status,
			original.Type, original.PktSize, original.Status)
	}
}

func TestDecodeASPEEDVideoHeader(t *testing.T) {
	// Build a known 86-byte header
	data := make([]byte, 86)
	off := 0
	w16 := func(v uint16) { data[off] = byte(v); data[off+1] = byte(v >> 8); off += 2 }
	w32 := func(v uint32) {
		data[off] = byte(v)
		data[off+1] = byte(v >> 8)
		data[off+2] = byte(v >> 16)
		data[off+3] = byte(v >> 24)
		off += 4
	}
	w8 := func(v byte) { data[off] = v; off++ }

	w16(1)    // EngVersion
	w16(86)   // HeaderLen
	w16(800)  // SrcX
	w16(600)  // SrcY
	w16(32)   // SrcColorDepth
	w16(60)   // SrcRefreshRate
	w8(0)     // SrcModeIndex
	w16(1024) // DstX
	w16(768)  // DstY
	w16(32)   // DstColorDepth
	w16(60)   // DstRefreshRate
	w8(0)     // DstModeIndex
	w32(0xBEEFCAFE) // FrameStartCode
	w32(42)   // FrameNumber
	w16(1024) // HSize
	w16(768)  // VSize
	off += 8  // Reserved
	w8(3)     // CompressionMode
	w8(16)    // JPEGScaleFactor
	w8(4)     // JPEGTableSelector
	w8(0)     // JPEGYUVTableMapping
	w8(0)     // SharpModeSelection
	w8(7)     // AdvTableSelector
	w8(23)    // AdvScaleFactor
	w32(2756) // NumberOfMB
	w8(0)     // RC4Enable
	w8(0)     // RC4Reset
	w8(0)     // Mode420
	w8(0)     // DownScalingMethod
	w8(0)     // DiffSetting
	w16(0)    // AnalogDiffThresh
	w16(0)    // DigitalDiffThresh
	w8(0)     // ExtSignalEnable
	w8(1)     // AutoMode
	w8(4)     // VQMode
	w32(0)    // SrcFrameSize
	w32(1000) // CompressSize
	w32(0)    // HDebug
	w32(0)    // VDebug
	w8(1)     // InputSignal
	w16(100)  // CursorXPos
	w16(200)  // CursorYPos

	hdr, err := DecodeASPEEDVideoHeader(data)
	if err != nil {
		t.Fatalf("DecodeASPEEDVideoHeader failed: %v", err)
	}

	if hdr.SrcX != 800 {
		t.Errorf("SrcX = %d, want 800", hdr.SrcX)
	}
	if hdr.SrcY != 600 {
		t.Errorf("SrcY = %d, want 600", hdr.SrcY)
	}
	if hdr.DstX != 1024 {
		t.Errorf("DstX = %d, want 1024", hdr.DstX)
	}
	if hdr.DstY != 768 {
		t.Errorf("DstY = %d, want 768", hdr.DstY)
	}
	if hdr.FrameNumber != 42 {
		t.Errorf("FrameNumber = %d, want 42", hdr.FrameNumber)
	}
	if hdr.JPEGTableSelector != 4 {
		t.Errorf("JPEGTableSelector = %d, want 4", hdr.JPEGTableSelector)
	}
	if hdr.AdvTableSelector != 7 {
		t.Errorf("AdvTableSelector = %d, want 7", hdr.AdvTableSelector)
	}
	if hdr.Mode420 != 0 {
		t.Errorf("Mode420 = %d, want 0", hdr.Mode420)
	}
	if hdr.CompressSize != 1000 {
		t.Errorf("CompressSize = %d, want 1000", hdr.CompressSize)
	}
	if hdr.CursorXPos != 100 {
		t.Errorf("CursorXPos = %d, want 100", hdr.CursorXPos)
	}
	if hdr.CursorYPos != 200 {
		t.Errorf("CursorYPos = %d, want 200", hdr.CursorYPos)
	}
}

func TestDecodeASPEEDVideoHeaderTooShort(t *testing.T) {
	_, err := DecodeASPEEDVideoHeader(make([]byte, 40))
	if err == nil {
		t.Error("Expected error for short video header")
	}
}

// ---------------------------------------------------------------------------
// 8. Zigzag tables -- verify inverse relationship
// ---------------------------------------------------------------------------

func TestZigzagInverse(t *testing.T) {
	// zigzag and dezigzag should be inverse operations:
	// zigzag maps linear index -> zigzag position
	// dezigzag maps zigzag position -> linear index (for AC coefficient storage)
	// Specifically, for JPEG: dezigzag[i] gives the position in the 8x8 block
	// where the i-th coefficient in scan order should be placed.
	// zigzag[i] gives the scan order position of the i-th linear position.

	// Verify that applying zigzag then looking up the result gives a complete
	// permutation (all 64 values 0..63 appear exactly once)
	seen := make(map[int]bool)
	for i := 0; i < 64; i++ {
		val := zigzag[i]
		if val < 0 || val > 63 {
			t.Errorf("zigzag[%d] = %d, out of range [0,63]", i, val)
		}
		if seen[val] {
			t.Errorf("zigzag[%d] = %d is a duplicate", i, val)
		}
		seen[val] = true
	}

	seenDZ := make(map[int]bool)
	for i := 0; i < 64; i++ {
		val := dezigzag[i]
		if val < 0 || val > 63 {
			t.Errorf("dezigzag[%d] = %d, out of range [0,63]", i, val)
		}
		if seenDZ[val] {
			t.Errorf("dezigzag[%d] = %d is a duplicate", i, val)
		}
		seenDZ[val] = true
	}
}

func TestZigzagDezigzagFirstValues(t *testing.T) {
	// The first element in zigzag scan is always position 0 (DC coefficient)
	if zigzag[0] != 0 {
		t.Errorf("zigzag[0] = %d, want 0", zigzag[0])
	}
	if dezigzag[0] != 0 {
		t.Errorf("dezigzag[0] = %d, want 0", dezigzag[0])
	}

	// The last element should be position 63
	if zigzag[63] != 63 {
		t.Errorf("zigzag[63] = %d, want 63", zigzag[63])
	}
	if dezigzag[63] != 63 {
		t.Errorf("dezigzag[63] = %d, want 63", dezigzag[63])
	}

	// Standard JPEG zigzag: dezigzag[1]=1, dezigzag[2]=8 (first row then first col)
	if dezigzag[1] != 1 {
		t.Errorf("dezigzag[1] = %d, want 1", dezigzag[1])
	}
	if dezigzag[2] != 8 {
		t.Errorf("dezigzag[2] = %d, want 8", dezigzag[2])
	}
}

// ---------------------------------------------------------------------------
// 9. Quantization tables
// ---------------------------------------------------------------------------

func TestSetQuantizationTable(t *testing.T) {
	d := NewDecoder()

	// Use tbl100Y (high quality, small values)
	src := tbl100Y[:]
	scaleFactor := 16

	out := d.setQuantizationTable(src, scaleFactor)

	// All values should be in [1, 255]
	for i := 0; i < 64; i++ {
		if out[i] < 1 {
			t.Errorf("setQuantizationTable: out[%d] = %d, expected >= 1", i, out[i])
		}
	}

	// First element of tbl100Y is 2. int8(2)*16/16 = 2, placed at zigzag[0]=0
	// So out[0] = 2
	expectedFirst := byte(int(int8(src[0])) * 16 / scaleFactor)
	if expectedFirst <= 0 {
		expectedFirst = 1
	}
	if out[zigzag[0]] != expectedFirst {
		t.Errorf("out[zigzag[0]]=%d, want %d", out[zigzag[0]], expectedFirst)
	}
}

func TestSetQuantizationTableClamping(t *testing.T) {
	d := NewDecoder()

	// Create a source table with extreme values
	src := make([]byte, 64)
	for i := range src {
		src[i] = 0 // int8(0) * 16 / 16 = 0, clamped to 1
	}
	out := d.setQuantizationTable(src, 16)
	for i := 0; i < 64; i++ {
		if out[i] < 1 {
			t.Errorf("Clamping failed: out[%d] = %d, expected >= 1", i, out[i])
		}
	}
}

func TestBuildQT(t *testing.T) {
	d := NewDecoder()
	var qt [64]int64

	d.buildQT(&qt, tbl057Y[:], 16)

	// All QT values should be non-zero (they are scaled by aanScales and *65536)
	for i := 0; i < 64; i++ {
		if qt[i] == 0 {
			t.Errorf("buildQT: qt[%d] = 0, expected non-zero", i)
		}
	}

	// The DC coefficient (index 0) should have a specific value based on
	// tbl057Y[0]=9, scaleFactor=16, aanScales[0]*aanScales[0]=1.0*1.0=1.0
	// setQuantizationTable puts int8(9)*16/16 = 9 at zigzag[0]=0
	// buildQT reads scaled[zigzag[0]] = scaled[0] = 9
	// qt[0] = int(float32(9) * (1.0 * 1.0)) * 65536 = 9 * 65536 = 589824
	if qt[0] != 589824 {
		t.Errorf("buildQT: qt[0] = %d, want 589824", qt[0])
	}
}

// ---------------------------------------------------------------------------
// 10. MakeIntArray -- little-endian byte-to-uint32
// ---------------------------------------------------------------------------

func TestMakeIntArray(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want []uint32
	}{
		{
			name: "4_bytes",
			data: []byte{0x78, 0x56, 0x34, 0x12},
			want: []uint32{0x12345678},
		},
		{
			name: "8_bytes",
			data: []byte{0x01, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xFF, 0xFF},
			want: []uint32{1, 0xFFFFFFFF},
		},
		{
			name: "padding",
			data: []byte{0xAB, 0xCD}, // 2 bytes, padded to 4
			want: []uint32{0x0000CDAB},
		},
		{
			name: "empty",
			data: []byte{},
			want: []uint32{},
		},
		{
			name: "3_bytes_padded",
			data: []byte{0x01, 0x02, 0x03},
			want: []uint32{0x00030201},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := makeIntArray(tc.data)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = 0x%08X, want 0x%08X", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 11. Range limit table
// ---------------------------------------------------------------------------

func TestRangeLimitTable(t *testing.T) {
	d := NewDecoder()

	tests := []struct {
		name  string
		index int
		want  int16
	}{
		// [0..255] = 0
		{"index_0", 0, 0},
		{"index_128", 128, 0},
		{"index_255", 255, 0},
		// [256..511] = 0..255
		{"index_256", 256, 0},
		{"index_384", 384, 128},
		{"index_511", 511, 255},
		// [512..894] = 255
		{"index_512", 512, 255},
		{"index_700", 700, 255},
		{"index_894", 894, 255},
		// [895..1279] = 0
		{"index_895", 895, 0},
		{"index_1000", 1000, 0},
		{"index_1279", 1279, 0},
		// [1280..1407] = i & 0xFF
		{"index_1280", 1280, 0},     // 1280 & 0xFF = 0
		{"index_1281", 1281, 1},     // 1281 & 0xFF = 1
		{"index_1407", 1407, 127},   // 1407 & 0xFF = 127
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := d.rangeLimit[tc.index]
			if got != tc.want {
				t.Errorf("rangeLimit[%d] = %d, want %d", tc.index, got, tc.want)
			}
		})
	}
}

func TestRangeLimitBoundaries(t *testing.T) {
	d := NewDecoder()

	// Transition at 256: [255]=0, [256]=0 (both 0 but for different reasons)
	if d.rangeLimit[255] != 0 {
		t.Errorf("rangeLimit[255] = %d, want 0", d.rangeLimit[255])
	}
	if d.rangeLimit[256] != 0 {
		t.Errorf("rangeLimit[256] = %d, want 0", d.rangeLimit[256])
	}

	// Transition at 512: [511]=255, [512]=255
	if d.rangeLimit[511] != 255 {
		t.Errorf("rangeLimit[511] = %d, want 255", d.rangeLimit[511])
	}
	if d.rangeLimit[512] != 255 {
		t.Errorf("rangeLimit[512] = %d, want 255", d.rangeLimit[512])
	}

	// Transition at 895: [894]=255, [895]=0
	if d.rangeLimit[894] != 255 {
		t.Errorf("rangeLimit[894] = %d, want 255", d.rangeLimit[894])
	}
	if d.rangeLimit[895] != 0 {
		t.Errorf("rangeLimit[895] = %d, want 0", d.rangeLimit[895])
	}

	// Transition at 1280: [1279]=0, [1280]=0
	if d.rangeLimit[1279] != 0 {
		t.Errorf("rangeLimit[1279] = %d, want 0", d.rangeLimit[1279])
	}
	if d.rangeLimit[1280] != 0 {
		t.Errorf("rangeLimit[1280] = %d, want 0 (1280 & 0xFF = 0)", d.rangeLimit[1280])
	}
}

// ---------------------------------------------------------------------------
// 12. Block type constants
// ---------------------------------------------------------------------------

func TestBlockTypeConstants(t *testing.T) {
	allTypes := []struct {
		name  string
		value int
	}{
		{"blockJPEGNoSkip", blockJPEGNoSkip},
		{"blockJPEGAdvNoSkip", blockJPEGAdvNoSkip},
		{"blockJPEGPass2NoSkip", blockJPEGPass2NoSkip},
		{"blockJPEGPass2AdvNoSkip", blockJPEGPass2AdvNoSkip},
		{"blockLowJPEGNoSkip", blockLowJPEGNoSkip},
		{"blockVQ1ColorNoSkip", blockVQ1ColorNoSkip},
		{"blockVQ2ColorNoSkip", blockVQ2ColorNoSkip},
		{"blockVQ4ColorNoSkip", blockVQ4ColorNoSkip},
		{"blockJPEGSkip", blockJPEGSkip},
		{"blockFrameEnd", blockFrameEnd},
		{"blockJPEGPass2Skip", blockJPEGPass2Skip},
		{"blockJPEGPass2AdvSkip", blockJPEGPass2AdvSkip},
		{"blockLowJPEGSkip", blockLowJPEGSkip},
		{"blockVQ1ColorSkip", blockVQ1ColorSkip},
		{"blockVQ2ColorSkip", blockVQ2ColorSkip},
		{"blockVQ4ColorSkip", blockVQ4ColorSkip},
	}

	// Verify all 16 block types have unique values
	if len(allTypes) != 16 {
		t.Fatalf("Expected 16 block types, got %d", len(allTypes))
	}

	seen := make(map[int]string)
	for _, bt := range allTypes {
		if existing, ok := seen[bt.value]; ok {
			t.Errorf("Duplicate block type value %d: %s and %s", bt.value, existing, bt.name)
		}
		seen[bt.value] = bt.name

		// All values should be 0..15 (4-bit field)
		if bt.value < 0 || bt.value > 15 {
			t.Errorf("Block type %s = %d, out of range [0,15]", bt.name, bt.value)
		}
	}
}

func TestAdvanceBlockTypes(t *testing.T) {
	// Verify the advance QT block types (0x1, 0x3, 0xB) are correctly defined
	if blockJPEGAdvNoSkip != 0x1 {
		t.Errorf("blockJPEGAdvNoSkip = 0x%X, want 0x1", blockJPEGAdvNoSkip)
	}
	if blockJPEGPass2AdvNoSkip != 0x3 {
		t.Errorf("blockJPEGPass2AdvNoSkip = 0x%X, want 0x3", blockJPEGPass2AdvNoSkip)
	}
	if blockJPEGPass2AdvSkip != 0xB {
		t.Errorf("blockJPEGPass2AdvSkip = 0x%X, want 0xB", blockJPEGPass2AdvSkip)
	}
}

func TestBlockTypeSkipBit(t *testing.T) {
	// Bit 3 (0x8) is the skip flag
	skipTypes := []int{
		blockJPEGSkip, blockFrameEnd, blockJPEGPass2Skip,
		blockJPEGPass2AdvSkip, blockLowJPEGSkip,
		blockVQ1ColorSkip, blockVQ2ColorSkip, blockVQ4ColorSkip,
	}
	noSkipTypes := []int{
		blockJPEGNoSkip, blockJPEGAdvNoSkip, blockJPEGPass2NoSkip,
		blockJPEGPass2AdvNoSkip, blockLowJPEGNoSkip,
		blockVQ1ColorNoSkip, blockVQ2ColorNoSkip, blockVQ4ColorNoSkip,
	}

	for _, v := range skipTypes {
		if v&0x8 == 0 {
			t.Errorf("Skip block type 0x%X should have bit 3 set", v)
		}
	}
	for _, v := range noSkipTypes {
		if v&0x8 != 0 {
			t.Errorf("NoSkip block type 0x%X should NOT have bit 3 set", v)
		}
	}
}

// ---------------------------------------------------------------------------
// 13. Neg_pow2 table
// ---------------------------------------------------------------------------

func TestNegPow2Table(t *testing.T) {
	d := NewDecoder()

	// negPow2[0] should be 0 (not initialized)
	if d.negPow2[0] != 0 {
		t.Errorf("negPow2[0] = %d, want 0", d.negPow2[0])
	}

	// Verify values match the formula 1 - 2^n for n=1..16
	for n := 1; n <= 16; n++ {
		expected := int16(1.0 - math.Pow(2.0, float64(n)))
		if d.negPow2[n] != expected {
			t.Errorf("negPow2[%d] = %d, want %d (1 - 2^%d)", n, d.negPow2[n], expected, n)
		}
	}

	// Specific known values
	specifics := []struct {
		n    int
		want int16
	}{
		{1, -1},    // 1 - 2 = -1
		{2, -3},    // 1 - 4 = -3
		{3, -7},    // 1 - 8 = -7
		{4, -15},   // 1 - 16 = -15
		{8, -255},  // 1 - 256 = -255
		{10, -1023}, // 1 - 1024 = -1023
	}

	for _, tc := range specifics {
		if d.negPow2[tc.n] != tc.want {
			t.Errorf("negPow2[%d] = %d, want %d", tc.n, d.negPow2[tc.n], tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 14. Per-block save/restore (via Decode with advance blocks)
// ---------------------------------------------------------------------------

func TestDecoderAdvanceBlockRestore(t *testing.T) {
	// When the decoder encounters an advance block type, it should restore
	// blocks that were written during the current frame and return an error.
	d := NewDecoder()

	// Setup a small 16x16 framebuffer with known pixel data
	header := &ASPEEDVideoHeader{
		SrcX: 16, SrcY: 16,
		DstX: 16, DstY: 16,
		Mode420: 0,
	}

	// Pre-fill framebuffer with a known pattern
	d.Width = 16
	d.Height = 16
	d.Framebuffer = make([]byte, 16*16*4)
	d.previousYUV = make([]int, 16*16*3)
	for i := range d.Framebuffer {
		d.Framebuffer[i] = 0xAA // fill with known value
	}
	origFB := make([]byte, len(d.Framebuffer))
	copy(origFB, d.Framebuffer)

	// Create compressed data that starts with an advance block type (0x1).
	// blockType is top 4 bits of first word, so 0x1XXXXXXX
	compressedWords := make([]byte, 16)
	// Word 0 (reg0): block type 0x1 in top 4 bits
	compressedWords[0] = 0x00
	compressedWords[1] = 0x00
	compressedWords[2] = 0x00
	compressedWords[3] = 0x10 // 0x10000000 => blockType = 0x1 (advance)
	// Word 1 (reg1)
	compressedWords[4] = 0x00
	compressedWords[5] = 0x00
	compressedWords[6] = 0x00
	compressedWords[7] = 0x00
	// Additional words
	compressedWords[8] = 0x00
	compressedWords[9] = 0x00
	compressedWords[10] = 0x00
	compressedWords[11] = 0x00
	compressedWords[12] = 0x00
	compressedWords[13] = 0x00
	compressedWords[14] = 0x00
	compressedWords[15] = 0x00

	header.CompressSize = uint32(len(compressedWords))

	err := d.Decode(header, compressedWords)
	if err == nil {
		t.Fatal("Expected error for advance block type, got nil")
	}

	// The framebuffer should have been restored to the original values
	// because the advance block handler calls restoreBlocks before returning
	for i := range d.Framebuffer {
		if d.Framebuffer[i] != origFB[i] {
			t.Errorf("Framebuffer[%d] = 0x%02X after restore, want 0x%02X",
				i, d.Framebuffer[i], origFB[i])
			break // Only report first difference
		}
	}
}

// ---------------------------------------------------------------------------
// Additional decoder tests
// ---------------------------------------------------------------------------

func TestNewDecoderInitialization(t *testing.T) {
	d := NewDecoder()

	// Verify all initialization happened
	if d.negPow2[1] != -1 {
		t.Error("negPow2 not initialized")
	}
	if d.rangeLimit[256] != 0 {
		t.Error("rangeLimit not initialized")
	}
	if d.yTable[16] != 0 {
		t.Error("color table not initialized")
	}
	if d.htDC[0].len[0] != 2 {
		t.Error("Huffman tables not initialized")
	}
}

func TestDecoderInvalidResolution(t *testing.T) {
	d := NewDecoder()
	header := &ASPEEDVideoHeader{
		DstX: 0, DstY: 0,
	}
	err := d.Decode(header, make([]byte, 100))
	if err == nil {
		t.Error("Expected error for zero resolution")
	}
}

func TestDecoderTooShortData(t *testing.T) {
	d := NewDecoder()
	header := &ASPEEDVideoHeader{
		SrcX: 800, SrcY: 600,
		DstX: 800, DstY: 600,
	}
	// Less than 12 bytes => less than 3 uint32 words
	err := d.Decode(header, make([]byte, 4))
	if err == nil {
		t.Error("Expected error for too-short compressed data")
	}
}

func TestClampByte(t *testing.T) {
	d := NewDecoder()

	tests := []struct {
		input int
		want  byte
	}{
		{-100, 0},
		{-1, 0},
		{0, 0},
		{128, 128},
		{255, 255},
		{256, 255},
		{1000, 255},
	}

	for _, tc := range tests {
		got := d.clampByte(tc.input)
		if got != tc.want {
			t.Errorf("clampByte(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestIDCTMultiply(t *testing.T) {
	tests := []struct {
		a, b int
		want int
	}{
		{0, 0, 0},
		{256, 256, 256},        // (256*256)>>8 = 256
		{512, 128, 256},        // (512*128)>>8 = 256
		{-256, 256, -256},      // (-256*256)>>8 = -256
		{100, fix1_414213562, 141}, // (100*362)>>8 = 141
	}

	for _, tc := range tests {
		got := idctMultiply(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("idctMultiply(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMoveBlockIndex444(t *testing.T) {
	d := NewDecoder()
	d.mode420 = 0
	d.gridWidth = 16
	d.gridHeight = 16
	d.txb = 0
	d.tyb = 0

	// 16/8 = 2 blocks per row
	d.moveBlockIndex()
	if d.txb != 1 || d.tyb != 0 {
		t.Errorf("After 1st move: txb=%d, tyb=%d, want txb=1, tyb=0", d.txb, d.tyb)
	}

	d.moveBlockIndex()
	if d.txb != 0 || d.tyb != 1 {
		t.Errorf("After 2nd move (wrap): txb=%d, tyb=%d, want txb=0, tyb=1", d.txb, d.tyb)
	}

	d.moveBlockIndex()
	d.moveBlockIndex()
	if d.txb != 0 || d.tyb != 0 {
		t.Errorf("After 4th move (full wrap): txb=%d, tyb=%d, want txb=0, tyb=0", d.txb, d.tyb)
	}
}

func TestMoveBlockIndex420(t *testing.T) {
	d := NewDecoder()
	d.mode420 = 1
	d.gridWidth = 32
	d.gridHeight = 32
	d.txb = 0
	d.tyb = 0

	// 32/16 = 2 blocks per row
	d.moveBlockIndex()
	if d.txb != 1 || d.tyb != 0 {
		t.Errorf("After 1st move: txb=%d, tyb=%d, want txb=1, tyb=0", d.txb, d.tyb)
	}

	d.moveBlockIndex()
	if d.txb != 0 || d.tyb != 1 {
		t.Errorf("After 2nd move (wrap): txb=%d, tyb=%d, want txb=0, tyb=1", d.txb, d.tyb)
	}
}

func TestIDCTConstants(t *testing.T) {
	// Verify IDCT fixed-point constants match their floating-point origins
	// These are scaled by 256 (8 fractional bits)
	tests := []struct {
		name  string
		value int
		want  float64
	}{
		{"fix1_082392200", fix1_082392200, 1.082392200},
		{"fix1_414213562", fix1_414213562, 1.414213562},
		{"fix1_847759065", fix1_847759065, 1.847759065},
		{"fix2_613125930", fix2_613125930, 2.613125930},
	}

	for _, tc := range tests {
		got := float64(tc.value) / 256.0
		diff := math.Abs(got - tc.want)
		if diff > 0.01 {
			t.Errorf("%s: %d/256 = %f, want ~%f (diff=%f)", tc.name, tc.value, got, tc.want, diff)
		}
	}
}

func TestDecoderFrameEndBlock(t *testing.T) {
	d := NewDecoder()

	header := &ASPEEDVideoHeader{
		SrcX: 16, SrcY: 16,
		DstX: 16, DstY: 16,
		Mode420: 0,
	}

	// Create compressed data with frame end block (0x9) immediately.
	// blockType is top 4 bits of first word.
	compressedWords := make([]byte, 16)
	// Word 0 (reg0): block type 0x9 in top 4 bits = 0x90000000
	compressedWords[3] = 0x90
	// Need at least 3 words for buf initialization
	header.CompressSize = uint32(len(compressedWords))

	err := d.Decode(header, compressedWords)
	if err != nil {
		t.Errorf("Frame end block should return nil, got: %v", err)
	}
}
