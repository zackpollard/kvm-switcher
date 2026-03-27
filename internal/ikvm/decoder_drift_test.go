package ikvm

import (
	"os"
	"testing"
)

// TestDecoderDrift checks if decoding the same frame twice produces
// identical output. Any difference would indicate state leaking between
// frames, which causes progressive pixel corruption.
func TestDecoderDrift(t *testing.T) {
	data, err := os.ReadFile("/tmp/ikvm-frame-data.bin")
	if err != nil {
		t.Skip("No captured frame data; run ikvm-test first")
	}

	header := &ASPEEDVideoHeader{
		SrcX: 800, SrcY: 600,
		DstX: 800, DstY: 600,
		CompressionMode:     3,
		JPEGScaleFactor:     16,
		JPEGTableSelector:   4,
		JPEGYUVTableMapping: 0,
		AdvTableSelector:    7,
		AdvScaleFactor:      23,
		NumberOfMB:          2756,
		CompressSize:        uint32(len(data)),
		Mode420:             0,
		VQMode:              4,
		AutoMode:            1,
	}

	// Decode frame 1
	dec := NewDecoder()
	if err := dec.Decode(header, data); err != nil {
		t.Fatalf("Decode 1 failed: %v", err)
	}
	frame1 := make([]byte, len(dec.Framebuffer))
	copy(frame1, dec.Framebuffer)
	prev1 := make([]int, len(dec.previousYUV))
	copy(prev1, dec.previousYUV)

	// Decode same frame again (simulates receiving the same frame)
	if err := dec.Decode(header, data); err != nil {
		t.Fatalf("Decode 2 failed: %v", err)
	}

	// Check framebuffer drift
	diffs := 0
	for i := 0; i < len(frame1) && i < len(dec.Framebuffer); i++ {
		if frame1[i] != dec.Framebuffer[i] {
			if diffs < 10 {
				px := (i / 4) % int(dec.Width)
				py := (i / 4) / int(dec.Width)
				ch := i % 4
				t.Errorf("Framebuffer drift at pixel (%d,%d) channel %d: %d -> %d",
					px, py, ch, frame1[i], dec.Framebuffer[i])
			}
			diffs++
		}
	}
	if diffs > 0 {
		t.Errorf("Total framebuffer diffs: %d pixels", diffs/4)
	}

	// Check previousYUV drift
	yuvDiffs := 0
	for i := 0; i < len(prev1) && i < len(dec.previousYUV); i++ {
		if prev1[i] != dec.previousYUV[i] {
			if yuvDiffs < 10 {
				t.Errorf("previousYUV drift at index %d: %d -> %d", i, prev1[i], dec.previousYUV[i])
			}
			yuvDiffs++
		}
	}
	if yuvDiffs > 0 {
		t.Errorf("Total previousYUV diffs: %d values", yuvDiffs)
	}

	if diffs == 0 && yuvDiffs == 0 {
		t.Log("No drift detected — decoding same frame twice produces identical output")
	}
}
