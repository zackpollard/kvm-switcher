package ikvm

import (
	"fmt"
	"math"
	"sync"
)

// Block type codes (top 4 bits of header word).
// Bit 3 = skip flag, bit 0 = advance QT flag for JPEG types.
const (
	blockJPEGNoSkip         = 0x0
	blockJPEGAdvNoSkip      = 0x1 // JPEG with advance QT tables
	blockJPEGPass2NoSkip    = 0x2
	blockJPEGPass2AdvNoSkip = 0x3 // JPEG Pass2 with advance QT tables
	blockLowJPEGNoSkip      = 0x4
	blockVQ1ColorNoSkip     = 0x5
	blockVQ2ColorNoSkip     = 0x6
	blockVQ4ColorNoSkip     = 0x7
	blockJPEGSkip           = 0x8
	blockFrameEnd           = 0x9
	blockJPEGPass2Skip      = 0xA
	blockJPEGPass2AdvSkip   = 0xB // JPEG Pass2 Skip with advance QT tables
	blockLowJPEGSkip        = 0xC
	blockVQ1ColorSkip       = 0xD
	blockVQ2ColorSkip       = 0xE
	blockVQ4ColorSkip       = 0xF
)

// IDCT fixed-point constants.
const (
	fix1_082392200 = 277
	fix1_414213562 = 362
	fix1_847759065 = 473
	fix2_613125930 = 669
)

// Decoder decodes ASPEED AST2400/2500 compressed video frames into a BGRA framebuffer.
// The framebuffer persists across frames (differential encoding -- only changed
// macroblocks are updated).
type Decoder struct {
	mu sync.Mutex

	Width       uint16
	Height      uint16
	Framebuffer []byte // BGRA, 4 bytes per pixel

	// Internal resolution (padded to macroblock boundaries).
	width  int
	height int

	// Real (unpadded) resolution from source.
	realWidth  int
	realHeight int

	// Block grid dimensions (padded).
	gridWidth  int
	gridHeight int

	// Current macroblock position.
	txb int
	tyb int

	// Mode420 flag (1 = YUV 4:2:0, 0 = YUV 4:4:4).
	mode420 byte

	// Bitstream reader state.
	buf     []uint32 // data words
	reg0    uint32   // current bitstream register
	reg1    uint32   // next bits register
	newbits int      // bits remaining in reg1
	index   int      // read position in buf

	// Huffman tables (0=luminance DC, 1=chrominance DC, etc).
	htDC [4]huffmanTable
	htAC [4]huffmanTable

	// Quantization tables.
	qt [4][64]int64

	// DCT state.
	dctCoeff  [384]int
	yuvTile   [768]int
	workspace [64]int

	// DC prediction values.
	dcY  int16
	dcCb int16
	dcCr int16

	// Previous YUV data (for pass2 differential decoding).
	previousYUV []int

	// Range limit table for clamping.
	rangeLimit [1408]int16

	// Color conversion lookup tables.
	yTable [256]int
	crToR  [256]int
	cbToB  [256]int
	crToG  [256]int
	cbToG  [256]int

	// VQ color cache.
	vqColor    [4]uint32
	vqIndex    [4]int
	vqBitmapBits byte

	// YUV tile data for 4:2:0 mode.
	yTile420 [4][64]int
	cbTile   [64]int
	crTile   [64]int

	// neg_pow2 table for Huffman sign extension.
	negPow2 [17]int16

	// Quantization table selector state.
	selector         int
	advanceSelector  int
	mapping          int
	scaleFactor      int
	scaleFactorUV    int
	advScaleFactor   int
	advScaleFactorUV int
}

type huffmanTable struct {
	length    [17]byte
	v         [65536]int16
	minorCode [17]int16
	majorCode [17]int16
	len       [65536]byte
}

// NewDecoder creates a new ASPEED video decoder.
func NewDecoder() *Decoder {
	d := &Decoder{}
	d.initNegPow2()
	d.initRangeLimitTable()
	d.initColorTable()
	d.initHuffmanTables()
	return d
}

// Decode processes a compressed video frame and updates the framebuffer.
// header contains resolution and compression parameters.
// compressedData is the compressed pixel data (after the 86-byte video header).
func (d *Decoder) Decode(header *ASPEEDVideoHeader, compressedData []byte) (retErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Recovery from decoder panics (Huffman table bugs, index errors, etc.)
	// The decoder is a complex port from Java and may have edge cases.
	// On panic, we preserve the existing framebuffer content rather than crashing.
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("decoder panic: %v", r)
		}
	}()

	w := int(header.DstX)
	h := int(header.DstY)

	if w <= 0 || h <= 0 {
		return fmt.Errorf("invalid resolution: %dx%d", w, h)
	}

	srcW := int(header.SrcX)
	srcH := int(header.SrcY)
	if srcW <= 0 {
		srcW = w
	}
	if srcH <= 0 {
		srcH = h
	}

	d.mode420 = header.Mode420

	// Handle resolution change.
	needRealloc := d.Width != uint16(w) || d.Height != uint16(h) || d.Framebuffer == nil
	if needRealloc {
		d.Width = uint16(w)
		d.Height = uint16(h)
		d.Framebuffer = make([]byte, w*h*4)
		d.previousYUV = make([]int, w*h*3)
	}

	d.realWidth = srcW
	d.realHeight = srcH

	// Compute padded dimensions for macroblock alignment.
	if d.mode420 == 1 {
		d.width = srcW
		if d.width%16 != 0 {
			d.width = d.width + 16 - d.width%16
		}
		d.height = srcH
		if d.height%16 != 0 {
			d.height = d.height + 16 - d.height%16
		}
	} else {
		d.width = srcW
		if d.width%8 != 0 {
			d.width = d.width + 8 - d.width%8
		}
		d.height = srcH
		if d.height%8 != 0 {
			d.height = d.height + 8 - d.height%8
		}
	}

	// Grid dimensions (padded destination resolution).
	d.gridWidth = w
	d.gridHeight = h
	if d.mode420 == 1 {
		if d.gridWidth%16 != 0 {
			d.gridWidth = d.gridWidth + 16 - d.gridWidth%16
		}
		if d.gridHeight%16 != 0 {
			d.gridHeight = d.gridHeight + 16 - d.gridHeight%16
		}
	} else {
		if d.gridWidth%8 != 0 {
			d.gridWidth = d.gridWidth + 8 - d.gridWidth%8
		}
		if d.gridHeight%8 != 0 {
			d.gridHeight = d.gridHeight + 8 - d.gridHeight%8
		}
	}

	// Initialize VQ color cache.
	d.vqIndex[0] = 0
	d.vqIndex[1] = 1
	d.vqIndex[2] = 2
	d.vqIndex[3] = 3
	d.vqColor[0] = 0x008080
	d.vqColor[1] = 0xFF8080
	d.vqColor[2] = 0x808080
	d.vqColor[3] = 0xC08080

	// Setup quantization tables. JViewer hardcodes all scale factors to 16.
	d.scaleFactor = 16
	d.scaleFactorUV = 16
	d.advScaleFactor = 16
	d.advScaleFactorUV = 16
	d.selector = int(header.JPEGTableSelector)
	d.advanceSelector = int(header.AdvTableSelector)
	d.mapping = int(header.JPEGYUVTableMapping)

	d.loadLuminanceQT(0)
	d.loadChrominanceQT(1)
	d.loadPass2LuminanceQT(2)
	d.loadPass2ChrominanceQT(3)

	// Convert compressed bytes to uint32 words (little-endian as in MakeIntArray).
	d.buf = makeIntArray(compressedData)
	if len(d.buf) < 3 {
		return fmt.Errorf("compressed data too short: %d words", len(d.buf))
	}

	// Initialize bitstream reader.
	// reg0 and reg1 are the two accumulator registers.
	// The data words start at index 0 in d.buf.
	// _index starts at 2 (skipping the first two words which seed the accumulators).
	d.reg0 = d.buf[0]
	d.reg1 = d.buf[1]
	d.index = 2
	d.newbits = 32

	// Reset block position and DC prediction.
	d.txb = 0
	d.tyb = 0
	d.dcY = 0
	d.dcCb = 0
	d.dcCr = 0

	compressWords := int(header.CompressSize) / 4

	// Track block positions written during this frame. If we hit an advance
	// block type, restore just these blocks to undo partial writes without
	// affecting the rest of the framebuffer.
	type savedBlock struct {
		offset int
		pixels [8 * 8 * 4]byte
	}
	var savedBlocks []savedBlock

	saveBlock := func(tileX, tileY int) {
		blockSize := 8
		pixX, pixY := tileX*blockSize, tileY*blockSize
		fbW := int(d.Width)
		var sb savedBlock
		sb.offset = (pixY*fbW + pixX) * 4
		for row := 0; row < blockSize; row++ {
			for col := 0; col < blockSize; col++ {
				idx := ((pixY+row)*fbW + pixX + col) * 4
				bIdx := (row*blockSize + col) * 4
				if idx+3 < len(d.Framebuffer) {
					copy(sb.pixels[bIdx:bIdx+4], d.Framebuffer[idx:idx+4])
				}
			}
		}
		savedBlocks = append(savedBlocks, sb)
	}

	restoreBlocks := func() {
		blockSize := 8
		fbW := int(d.Width)
		for _, sb := range savedBlocks {
			pixX := (sb.offset / 4) % fbW
			pixY := (sb.offset / 4) / fbW
			for row := 0; row < blockSize; row++ {
				for col := 0; col < blockSize; col++ {
					idx := ((pixY+row)*fbW + pixX + col) * 4
					bIdx := (row*blockSize + col) * 4
					if idx+3 < len(d.Framebuffer) {
						copy(d.Framebuffer[idx:idx+4], sb.pixels[bIdx:bIdx+4])
					}
				}
			}
		}
	}

	// Process macroblocks.
	for d.index < compressWords {
		blockType := d.reg0 >> 28

		switch blockType {
		case blockJPEGNoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()

		case blockJPEGAdvNoSkip, blockJPEGPass2AdvNoSkip, blockJPEGPass2AdvSkip:
			restoreBlocks()
			return fmt.Errorf("advance block type 0x%X", blockType)

		case blockJPEGSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()

		case blockJPEGPass2NoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockJPEGPass2Skip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockLowJPEGNoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockLowJPEGSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockVQ1ColorNoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.vqBitmapBits = 0
			d.decodeVQHeader(1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ1ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.vqBitmapBits = 0
			d.decodeVQHeader(1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ2ColorNoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.vqBitmapBits = 1
			d.decodeVQHeader(2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ2ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.vqBitmapBits = 1
			d.decodeVQHeader(2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ4ColorNoSkip:
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(4)
			d.vqBitmapBits = 2
			d.decodeVQHeader(4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ4ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			saveBlock(d.txb, d.tyb)
			d.updateReadBuf(20)
			d.vqBitmapBits = 2
			d.decodeVQHeader(4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockFrameEnd:
			return nil

		default:
			// Truly unknown block type — stop processing.
			return fmt.Errorf("unknown block type 0x%X", blockType)
		}
	}

	return nil
}

// initNegPow2 precomputes the neg_pow2 table used for Huffman sign extension.
func (d *Decoder) initNegPow2() {
	for i := 1; i < 17; i++ {
		d.negPow2[i] = int16(1.0 - math.Pow(2.0, float64(i)))
	}
}

// initRangeLimitTable sets up the clamping table.
func (d *Decoder) initRangeLimitTable() {
	// [0..255] = 0
	for i := 0; i < 256; i++ {
		d.rangeLimit[i] = 0
	}
	// [256..511] = 0..255
	for i := 0; i < 256; i++ {
		d.rangeLimit[256+i] = int16(i)
	}
	// [512..894] = 255 (Java: Arrays.fill(rangeLimitTableShort, 512, 895, 255) — toIndex exclusive)
	for i := 512; i < 895; i++ {
		d.rangeLimit[i] = 255
	}
	// [895..1279] = 0
	for i := 895; i < 1280; i++ {
		d.rangeLimit[i] = 0
	}
	// [1280..1407] = i & 0xFF
	for i := 1280; i < 1408; i++ {
		d.rangeLimit[i] = int16(i & 0xFF)
	}
}

// initColorTable builds the YUV-to-RGB conversion lookup tables.
// Uses JViewer's calculatedRGBof* tables (ITU-R BT.601 limited-range YCbCr).
func (d *Decoder) initColorTable() {
	fixG := func(v float64) int {
		return int(v*65536.0 + 0.5)
	}
	half := 65536 >> 1
	x := -128
	for i := 0; i < 256; i++ {
		d.crToR[i] = (fixG(1.597656)*x + half) >> 16
		d.cbToB[i] = (fixG(2.015625)*x + half) >> 16
		d.crToG[i] = (-fixG(0.8125)*x + half) >> 16
		d.cbToG[i] = (-fixG(0.390625)*x + half) >> 16
		x++
	}
	x = -16
	for i := 0; i < 256; i++ {
		d.yTable[i] = (fixG(1.164)*x + half) >> 16
		x++
	}
}

// initHuffmanTables loads the standard JPEG DC and AC Huffman tables.
func (d *Decoder) initHuffmanTables() {
	d.loadHuffmanTable(&d.htDC[0], stdDCLuminanceNRCodes[:], stdDCLuminanceValues[:], dcLuminanceHuffmanCode[:])
	d.loadHuffmanTable(&d.htAC[0], stdACLuminanceNRCodes[:], stdACLuminanceValues[:], acLuminanceHuffmanCode[:])
	d.loadHuffmanTable(&d.htDC[1], stdDCChrominanceNRCodes[:], stdDCChrominanceValues[:], dcChrominanceHuffmanCode[:])
	d.loadHuffmanTable(&d.htAC[1], stdACChrominanceNRCodes[:], stdACChrominanceValues[:], acChrominanceHuffmanCode[:])
}

// loadHuffmanTable populates a Huffman table from nrcodes, values, and the fast-lookup code table.
func (d *Decoder) loadHuffmanTable(ht *huffmanTable, nrcodes []byte, values []int16, huffCode []int) {
	for i := 1; i <= 16; i++ {
		ht.length[i] = nrcodes[i]
	}

	n := 0
	for i := 1; i <= 16; i++ {
		for j := 0; j < int(ht.length[i]); j++ {
			idx := int(i)<<8 | j
			if idx < len(ht.v) {
				ht.v[idx] = values[n]
			}
			n++
		}
	}

	code := 0
	for i := 1; i <= 16; i++ {
		ht.minorCode[i] = int16(code)
		for j := 1; j <= int(ht.length[i]); j++ {
			code++
		}
		ht.majorCode[i] = int16(code - 1)
		code *= 2
		if ht.length[i] == 0 {
			ht.minorCode[i] = -1
			ht.majorCode[i] = 0
		}
	}

	// Build fast lookup table.
	ht.len[0] = 2
	ci := 2
	for i := 1; i < 65535; i++ {
		if i < huffCode[ci] {
			ht.len[i] = byte(huffCode[ci+1] & 0xFF)
		} else {
			ci += 2
			if ci+1 < len(huffCode) {
				ht.len[i] = byte(huffCode[ci+1] & 0xFF)
			}
		}
	}
}
