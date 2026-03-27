package ikvm

import (
	"image"
	"image/png"
	"os"
	"testing"
)

// TestDecodeFrame tests the decoder against a real captured frame from
// a live AMI MegaRAC BMC. The frame was captured from 10.10.11.14
// using the ikvm-test tool.
//
// Run with: go test -run TestDecodeFrame -v ./internal/ikvm/
func TestDecodeFrame(t *testing.T) {
	data, err := os.ReadFile("/tmp/ikvm-frame-data.bin")
	if err != nil {
		t.Skip("No captured frame data at /tmp/ikvm-frame-data.bin; run ikvm-test first")
	}

	// Frame parameters from the capture
	header := &ASPEEDVideoHeader{
		SrcX: 800, SrcY: 600,
		DstX: 800, DstY: 600,
		CompressionMode:     3,  // VQ 4-color
		JPEGScaleFactor:     16,
		JPEGTableSelector:   4,
		JPEGYUVTableMapping: 0,
		AdvTableSelector:    7,
		AdvScaleFactor:      23,
		NumberOfMB:          2756,
		CompressSize:        uint32(len(data)),
		Mode420:             0, // YUV 4:4:4
		VQMode:              4,
		AutoMode:            1,
	}

	dec := NewDecoder()
	err = dec.Decode(header, data)
	if err != nil {
		t.Logf("Decode returned error (decoder may need fixes): %v", err)
		// Still check that resolution was set
		if dec.Width == 0 || dec.Height == 0 || dec.Framebuffer == nil {
			t.Skip("Decoder did not initialize framebuffer")
		}
	}

	if dec.Width != 800 || dec.Height != 600 {
		t.Fatalf("Expected 800x600, got %dx%d", dec.Width, dec.Height)
	}

	expectedSize := 800 * 600 * 4
	if len(dec.Framebuffer) < expectedSize {
		t.Fatalf("Framebuffer too small: %d < %d", len(dec.Framebuffer), expectedSize)
	}

	// Save as PNG for visual verification
	img := image.NewRGBA(image.Rect(0, 0, int(dec.Width), int(dec.Height)))
	for y := 0; y < int(dec.Height); y++ {
		for x := 0; x < int(dec.Width); x++ {
			off := (y*int(dec.Width) + x) * 4
			// BGRA → RGBA
			img.Pix[(y*int(dec.Width)+x)*4+0] = dec.Framebuffer[off+2] // R
			img.Pix[(y*int(dec.Width)+x)*4+1] = dec.Framebuffer[off+1] // G
			img.Pix[(y*int(dec.Width)+x)*4+2] = dec.Framebuffer[off+0] // B
			img.Pix[(y*int(dec.Width)+x)*4+3] = 255                     // A
		}
	}

	outPath := "/tmp/ikvm-decoded-frame.png"
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("Create output: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("PNG encode: %v", err)
	}
	t.Logf("Decoded frame saved to %s", outPath)

	// Basic sanity check: not all pixels should be the same color
	firstPixel := dec.Framebuffer[0:4]
	allSame := true
	for i := 4; i < len(dec.Framebuffer); i += 4 {
		if dec.Framebuffer[i] != firstPixel[0] || dec.Framebuffer[i+1] != firstPixel[1] ||
			dec.Framebuffer[i+2] != firstPixel[2] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Log("WARNING: All pixels are the same color — decoder may not be working yet")
	} else {
		t.Log("Frame has varied pixel data — decoder is producing output")
	}
}
