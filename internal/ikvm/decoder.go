package ikvm

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
)

// Block type codes (top 4 bits of header word).
// Bit 3 = skip flag, bit 0 = advance QT flag for JPEG types.
const (
	blockJPEGNoSkip           = 0x0
	blockJPEGAdvNoSkip        = 0x1 // JPEG with advance QT tables
	blockJPEGPass2NoSkip      = 0x2
	blockJPEGPass2AdvNoSkip   = 0x3 // JPEG Pass2 with advance QT tables
	blockLowJPEGNoSkip        = 0x4
	blockVQ1ColorNoSkip       = 0x5
	blockVQ2ColorNoSkip       = 0x6
	blockVQ4ColorNoSkip       = 0x7
	blockJPEGSkip             = 0x8
	blockFrameEnd             = 0x9
	blockJPEGPass2Skip        = 0xA
	blockJPEGPass2AdvSkip     = 0xB // JPEG Pass2 Skip with advance QT tables
	blockLowJPEGSkip          = 0xC
	blockVQ1ColorSkip         = 0xD
	blockVQ2ColorSkip         = 0xE
	blockVQ4ColorSkip         = 0xF
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
	yTable   [256]int
	crToR    [256]int
	cbToB    [256]int
	crToG    [256]int
	cbToG    [256]int

	// VQ color cache.
	vqColor    [4]uint32
	vqIndex    [4]int
	vqBitmapBits byte

	// YUV tile data for 4:2:0 mode.
	yTile420  [4][64]int
	cbTile    [64]int
	crTile    [64]int

	// neg_pow2 table for Huffman sign extension.
	negPow2 [17]int16

	// Quantization table selector state.
	selector        int
	advanceSelector int
	mapping         int
	scaleFactor     int
	scaleFactorUV   int
	advScaleFactor  int
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

// makeIntArray converts bytes to uint32 words using the same byte order as the
// Java MakeIntArray: bytes[n+3]<<24 | bytes[n+2]<<16 | bytes[n+1]<<8 | bytes[n].
// This is standard little-endian uint32.
func makeIntArray(data []byte) []uint32 {
	// Pad to 4-byte boundary.
	padded := data
	if rem := len(data) % 4; rem != 0 {
		padded = make([]byte, len(data)+4-rem)
		copy(padded, data)
	}
	n := len(padded) / 4
	result := make([]uint32, n)
	for i := 0; i < n; i++ {
		result[i] = binary.LittleEndian.Uint32(padded[i*4:])
	}
	return result
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
// Verified by extracting actual table values from JViewer via reflection:
//   calcY[16]=0, calcY[128]=130, calcCrToR[128]=0, calcCbToB[128]=0
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

// --- Bitstream reader ---

// lookKbits peeks at the top k bits from the bitstream register.
func (d *Decoder) lookKbits(k byte) int16 {
	if k == 0 {
		return 0
	}
	return int16(d.reg0 >> (32 - k))
}

// skipKbits consumes k bits from the bitstream.
func (d *Decoder) skipKbits(k byte) {
	n := int(k)
	if d.newbits-n <= 0 {
		if d.index >= len(d.buf) {
			d.index = len(d.buf) - 1
		}
		nextWord := d.buf[d.index]
		d.index++
		d.reg0 = (d.reg0 << n) | uint32((uint64(d.reg1)|uint64(nextWord)>>uint(d.newbits))>>uint(32-n))
		d.reg1 = nextWord << uint(n-d.newbits)
		d.newbits = 32 + d.newbits - n
	} else {
		d.reg0 = (d.reg0 << n) | (d.reg1 >> uint(32-n))
		d.reg1 = d.reg1 << n
		d.newbits -= n
	}
}

// getKbits reads k bits and applies sign extension for JPEG coefficient decoding.
func (d *Decoder) getKbits(k byte) int16 {
	val := d.lookKbits(k)
	if (1<<(k-1))&val == 0 {
		val = val + d.negPow2[k]
	}
	d.skipKbits(k)
	return val
}

// updateReadBuf consumes n bits from the bitstream (same as skipKbits but
// operates on unsigned values matching the Java updateReadBuf).
func (d *Decoder) updateReadBuf(n int) {
	if d.newbits-n <= 0 {
		if d.index >= len(d.buf) {
			d.index = len(d.buf) - 1
		}
		nextWord := uint64(d.buf[d.index])
		d.index++
		d.reg0 = uint32(uint64(d.reg0)<<uint(n)) | uint32((uint64(d.reg1)|nextWord>>uint(d.newbits))>>uint(32-n))
		d.reg1 = uint32(nextWord << uint(n-d.newbits))
		d.newbits = 32 + d.newbits - n
	} else {
		d.reg0 = uint32(uint64(d.reg0)<<uint(n)) | uint32(uint64(d.reg1)>>uint(32-n))
		d.reg1 = uint32(uint64(d.reg1) << uint(n))
		d.newbits -= n
	}
}

// --- Quantization table loading ---

func (d *Decoder) setQuantizationTable(srcTable []byte, scaleFactor int) [64]byte {
	var out [64]byte
	for i := 0; i < 64; i++ {
		// Java uses signed byte: values > 127 are negative. Match that behavior.
		v := int(int8(srcTable[i])) * 16 / scaleFactor
		if v <= 0 {
			v = 1
		}
		if v > 255 {
			v = 255
		}
		out[zigzag[i]] = byte(v)
	}
	return out
}

func (d *Decoder) buildQT(qt *[64]int64, table []byte, scaleFactor int) {
	aanScales := [8]float32{1.0, 1.3870399, 1.306563, 1.1758755, 1.0, 0.78569496, 0.5411961, 0.27589938}
	scaled := d.setQuantizationTable(table, scaleFactor)

	for i := 0; i < 64; i++ {
		qt[i] = int64(scaled[zigzag[i]]) & 0xFF
	}

	n := 0
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			v := int(float32(qt[n]) * (aanScales[row] * aanScales[col]))
			qt[n] = int64(v) * 65536
			n++
		}
	}
}

func (d *Decoder) selectLuminanceTable(sel int) []byte {
	switch sel {
	case 0:
		return tbl000Y[:]
	case 1:
		return tbl014Y[:]
	case 2:
		return tbl029Y[:]
	case 3:
		return tbl043Y[:]
	case 4:
		return tbl057Y[:]
	case 5:
		return tbl071Y[:]
	case 6:
		return tbl086Y[:]
	case 7:
		return tbl100Y[:]
	default:
		return tbl000Y[:]
	}
}

func (d *Decoder) selectChrominanceTable(sel int, mapping int) []byte {
	if mapping == 1 {
		return d.selectLuminanceTable(sel)
	}
	switch sel {
	case 0:
		return tbl000UV[:]
	case 1:
		return tbl014UV[:]
	case 2:
		return tbl029UV[:]
	case 3:
		return tbl043UV[:]
	case 4:
		return tbl057UV[:]
	case 5:
		return tbl071UV[:]
	case 6:
		return tbl086UV[:]
	case 7:
		return tbl100UV[:]
	default:
		return tbl000UV[:]
	}
}

func (d *Decoder) loadLuminanceQT(qtIdx int) {
	tbl := d.selectLuminanceTable(d.selector)
	d.buildQT(&d.qt[qtIdx], tbl, d.scaleFactor)
}

func (d *Decoder) loadChrominanceQT(qtIdx int) {
	tbl := d.selectChrominanceTable(d.selector, d.mapping)
	d.buildQT(&d.qt[qtIdx], tbl, d.scaleFactorUV)
}

func (d *Decoder) loadPass2LuminanceQT(qtIdx int) {
	tbl := d.selectLuminanceTable(d.advanceSelector)
	d.buildQT(&d.qt[qtIdx], tbl, d.advScaleFactor)
}

func (d *Decoder) loadPass2ChrominanceQT(qtIdx int) {
	tbl := d.selectChrominanceTable(d.advanceSelector, d.mapping)
	d.buildQT(&d.qt[qtIdx], tbl, d.advScaleFactorUV)
}

// --- Huffman decoding ---

func (d *Decoder) decodeHuffmanDataUnit(dcTableIdx, acTableIdx byte, dcPred *int16, coeffOffset int) {
	// Clear DCT coefficients for this block.
	for i := coeffOffset; i < coeffOffset+64; i++ {
		d.dctCoeff[i] = 0
	}

	// Decode DC coefficient.
	htDC := &d.htDC[dcTableIdx]
	lookup := int(d.reg0>>16) & 0xFFFF
	codeLen := htDC.len[lookup]
	if codeLen == 0 {
		codeLen = 1
	}
	code := d.lookKbits(codeLen)
	d.skipKbits(codeLen)
	// Java: (byte)(s2 - min_code[by7]) — truncate to 8 bits, then WORD_hi_lo
	idx := int(codeLen)<<8 | int(byte(code-htDC.minorCode[codeLen]))
	category := byte(htDC.v[idx])

	if category == 0 {
		d.dctCoeff[coeffOffset] = int(*dcPred)
	} else {
		diff := d.getKbits(category)
		dc := int(*dcPred) + int(diff)
		d.dctCoeff[coeffOffset] = dc
		*dcPred = int16(dc)
	}

	// Decode AC coefficients.
	htAC := &d.htAC[acTableIdx]
	k := 1
	for k < 64 {
		lookup = int(d.reg0>>16) & 0xFFFF
		codeLen = htAC.len[lookup]
		if codeLen == 0 {
			codeLen = 1
		}
		code = d.lookKbits(codeLen)
		d.skipKbits(codeLen)
		idx = int(codeLen)<<8 | int(byte(code-htAC.minorCode[codeLen]))
		sym := byte(htAC.v[idx])

		size := sym & 0x0F
		run := (sym >> 4) & 0x0F

		if size == 0 {
			if run != 15 {
				break // EOB
			}
			k += 16
			continue
		}
		k += int(run)
		if k < 64 {
			d.dctCoeff[coeffOffset+int(dezigzag[k])] = int(d.getKbits(size))
		}
		k++
	}
}

// --- Inverse DCT ---

func idctMultiply(a, b int) int {
	return (a * b) >> 8
}

func (d *Decoder) inverseDCT(offset int, qtIdx byte) {
	src := offset
	ws := 0
	qi := 0

	// Column pass.
	for col := 0; col < 8; col++ {
		if (d.dctCoeff[src+8] | d.dctCoeff[src+16] | d.dctCoeff[src+24] |
			d.dctCoeff[src+32] | d.dctCoeff[src+40] | d.dctCoeff[src+48] |
			d.dctCoeff[src+56]) == 0 {
			dc := int(int64(d.dctCoeff[src]) * d.qt[qtIdx][qi]) >> 16
			d.workspace[ws] = dc
			d.workspace[ws+8] = dc
			d.workspace[ws+16] = dc
			d.workspace[ws+24] = dc
			d.workspace[ws+32] = dc
			d.workspace[ws+40] = dc
			d.workspace[ws+48] = dc
			d.workspace[ws+56] = dc
		} else {
			tmp0 := int(int64(d.dctCoeff[src]) * d.qt[qtIdx][qi]) >> 16
			tmp1 := int(int64(d.dctCoeff[src+16]) * d.qt[qtIdx][qi+16]) >> 16
			tmp2 := int(int64(d.dctCoeff[src+32]) * d.qt[qtIdx][qi+32]) >> 16
			tmp3 := int(int64(d.dctCoeff[src+48]) * d.qt[qtIdx][qi+48]) >> 16

			tmp10 := tmp0 + tmp2
			tmp11 := tmp0 - tmp2
			tmp12 := tmp1 + tmp3
			tmp13 := idctMultiply(tmp1-tmp3, fix1_414213562) - tmp12

			tmp0 = tmp10 + tmp12
			tmp3 = tmp10 - tmp12
			tmp1 = tmp11 + tmp13
			tmp2 = tmp11 - tmp13

			tmp4 := int(int64(d.dctCoeff[src+8]) * d.qt[qtIdx][qi+8]) >> 16
			tmp5 := int(int64(d.dctCoeff[src+24]) * d.qt[qtIdx][qi+24]) >> 16
			tmp6 := int(int64(d.dctCoeff[src+40]) * d.qt[qtIdx][qi+40]) >> 16
			tmp7 := int(int64(d.dctCoeff[src+56]) * d.qt[qtIdx][qi+56]) >> 16

			z13 := tmp6 + tmp5
			z10 := tmp6 - tmp5
			z11 := tmp4 + tmp7
			z12 := tmp4 - tmp7

			tmp7 = z11 + z13
			tmp11 = idctMultiply(z11-z13, fix1_414213562)
			z5 := idctMultiply(z10+z12, fix1_847759065)
			tmp10 = idctMultiply(z12, fix1_082392200) - z5
			tmp12 = idctMultiply(z10, -fix2_613125930) + z5

			tmp6 = tmp12 - tmp7
			tmp5 = tmp11 - tmp6
			tmp4 = tmp10 + tmp5

			d.workspace[ws] = tmp0 + tmp7
			d.workspace[ws+56] = tmp0 - tmp7
			d.workspace[ws+8] = tmp1 + tmp6
			d.workspace[ws+48] = tmp1 - tmp6
			d.workspace[ws+16] = tmp2 + tmp5
			d.workspace[ws+40] = tmp2 - tmp5
			d.workspace[ws+32] = tmp3 + tmp4
			d.workspace[ws+24] = tmp3 - tmp4
		}
		src++
		qi++
		ws++
	}

	// Row pass.
	ws = 0
	for row := 0; row < 8; row++ {
		outBase := offset + row*8

		tmp10 := d.workspace[ws] + d.workspace[ws+4]
		tmp11 := d.workspace[ws] - d.workspace[ws+4]
		tmp12 := d.workspace[ws+2] + d.workspace[ws+6]
		tmp13 := idctMultiply(d.workspace[ws+2]-d.workspace[ws+6], fix1_414213562) - tmp12

		tmp0 := tmp10 + tmp12
		tmp3 := tmp10 - tmp12
		tmp1 := tmp11 + tmp13
		tmp2 := tmp11 - tmp13

		z13 := d.workspace[ws+5] + d.workspace[ws+3]
		z10 := d.workspace[ws+5] - d.workspace[ws+3]
		z11 := d.workspace[ws+1] + d.workspace[ws+7]
		z12 := d.workspace[ws+1] - d.workspace[ws+7]

		tmp7 := z11 + z13
		tmp11 = idctMultiply(z11-z13, fix1_414213562)
		z5 := idctMultiply(z10+z12, fix1_847759065)
		tmp10 = idctMultiply(z12, fix1_082392200) - z5
		tmp12 = idctMultiply(z10, -fix2_613125930) + z5

		tmp6 := tmp12 - tmp7
		tmp5 := tmp11 - tmp6
		tmp4 := tmp10 + tmp5

		val := 128 + ((tmp0 + tmp7) >> 3 & 0x3FF)
		d.yuvTile[outBase] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp0 - tmp7) >> 3 & 0x3FF)
		d.yuvTile[outBase+7] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp1 + tmp6) >> 3 & 0x3FF)
		d.yuvTile[outBase+1] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp1 - tmp6) >> 3 & 0x3FF)
		d.yuvTile[outBase+6] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp2 + tmp5) >> 3 & 0x3FF)
		d.yuvTile[outBase+2] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp2 - tmp5) >> 3 & 0x3FF)
		d.yuvTile[outBase+5] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp3 + tmp4) >> 3 & 0x3FF)
		d.yuvTile[outBase+4] = int(d.rangeLimit[val+256])
		val = 128 + ((tmp3 - tmp4) >> 3 & 0x3FF)
		d.yuvTile[outBase+3] = int(d.rangeLimit[val+256])

		ws += 8
	}
}

// isNeutralBlock checks if the current yuvTile contains only the JPEG neutral
// midpoint value (128) for all Y, Cb, and Cr samples. This indicates the ASPEED
// encoder had no real data for this block (DC=0, no AC coefficients).
func (d *Decoder) isNeutralBlock() bool {
	// Check Y (0..63), Cb (64..127), Cr (128..191) in 4:4:4 mode.
	// In 4:2:0 mode, check all 384 values (4Y + Cb + Cr).
	count := 192
	if d.mode420 == 1 {
		count = 384
	}
	for i := 0; i < count; i++ {
		if d.yuvTile[i] != 128 {
			return false
		}
	}
	return true
}

// updatePreviousYUV stores the current yuvTile values into previousYUV without
// writing to the framebuffer. This keeps previousYUV in sync for Pass2 blocks
// even when we skip rendering neutral blocks.
func (d *Decoder) updatePreviousYUV(tileX, tileY int) {
	if d.mode420 == 0 {
		pixX := tileX * 8
		pixY := tileY * 8
		for row := 0; row < 8; row++ {
			py := pixY + row
			if py >= d.realHeight {
				break
			}
			for col := 0; col < 8; col++ {
				px := pixX + col
				if px >= d.realWidth {
					break
				}
				tileIdx := row*8 + col
				yuvIdx := (py*d.realWidth + px) * 3
				if yuvIdx+2 < len(d.previousYUV) {
					d.previousYUV[yuvIdx] = d.yuvTile[tileIdx]
					d.previousYUV[yuvIdx+1] = d.yuvTile[64+tileIdx]
					d.previousYUV[yuvIdx+2] = d.yuvTile[128+tileIdx]
				}
			}
		}
	}
	// 4:2:0 mode: previousYUV is not used for pass2 in 16x16 blocks,
	// so no update needed.
}

// --- Color conversion ---

func (d *Decoder) clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func (d *Decoder) convertYUVtoRGB(tileX, tileY int) {
	fbW := int(d.Width)
	fbH := int(d.Height)

	if d.mode420 == 0 {
		// YUV 4:4:4 mode: 8x8 pixel block.
		pixX := tileX * 8
		pixY := tileY * 8

		maxCol := 8
		if fbW-pixX < maxCol && fbW-pixX > 0 {
			maxCol = fbW - pixX
		}

		for row := 0; row < 8; row++ {
			py := pixY + row
			if py >= fbH {
				break
			}
			for col := 0; col < maxCol; col++ {
				px := pixX + col
				if px >= fbW {
					break
				}

				tileIdx := row*8 + col
				yVal := d.yuvTile[tileIdx]
				cbVal := d.yuvTile[64+tileIdx]
				crVal := d.yuvTile[128+tileIdx]

				// Store previous YUV for pass2.
				yuvIdx := (py*d.realWidth + px) * 3
				if yuvIdx+2 < len(d.previousYUV) {
					d.previousYUV[yuvIdx] = yVal
					d.previousYUV[yuvIdx+1] = cbVal
					d.previousYUV[yuvIdx+2] = crVal
				}

				// Clamp indices.
				if yVal < 0 { yVal = 0 } else if yVal > 255 { yVal = 255 }
				if cbVal < 0 { cbVal = 0 } else if cbVal > 255 { cbVal = 255 }
				if crVal < 0 { crVal = 0 } else if crVal > 255 { crVal = 255 }

				b := d.yTable[yVal] + d.cbToB[cbVal]
				g := d.yTable[yVal] + d.cbToG[cbVal] + d.crToG[crVal]
				r := d.yTable[yVal] + d.crToR[crVal]

				if b >= 0 { b += 256 } else { b = 0 }
				if g >= 0 { g += 256 } else { g = 0 }
				if r >= 0 { r += 256 } else { r = 0 }

				fbIdx := (py*fbW + px) * 4
				if fbIdx+3 < len(d.Framebuffer) {
					d.Framebuffer[fbIdx] = byte(d.rangeLimit[b])   // B
					d.Framebuffer[fbIdx+1] = byte(d.rangeLimit[g]) // G
					d.Framebuffer[fbIdx+2] = byte(d.rangeLimit[r]) // R
					d.Framebuffer[fbIdx+3] = 255                   // A
				}
			}
		}
	} else {
		// YUV 4:2:0 mode: 16x16 pixel block (4 Y blocks + 1 Cb + 1 Cr).
		// Extract Y, Cb, Cr from yuvTile.
		idx := 0
		for blk := 0; blk < 4; blk++ {
			for i := 0; i < 64; i++ {
				d.yTile420[blk][i] = d.yuvTile[idx]
				idx++
			}
		}
		for i := 0; i < 64; i++ {
			d.cbTile[i] = d.yuvTile[idx]
			d.crTile[i] = d.yuvTile[idx+64]
			idx++
		}

		pixX := tileX * 16
		pixY := tileY * 16
		lineOffset := pixY*d.realWidth + pixX

		maxRow := 16
		// Handle partial last row of macroblocks.
		if d.height == 608 && tileY == 37 {
			maxRow = 8
		}

		var yBlockIdx [4]int

		for row := 0; row < maxRow; row++ {
			blockRow := (row >> 3) * 2
			chromaRow := (row >> 1) * 8

			for col := 0; col < 16; col++ {
				blockIdx := blockRow + (col >> 3)

				yIdx := yBlockIdx[blockIdx]
				yBlockIdx[blockIdx]++

				py := pixY + row
				px := pixX + col
				if py >= fbH || px >= fbW {
					continue
				}

				yVal := d.yTile420[blockIdx][yIdx]
				chromaIdx := chromaRow + (col >> 1)
				cbVal := d.cbTile[chromaIdx]
				crVal := d.crTile[chromaIdx]

				if yVal < 0 { yVal = 0 } else if yVal > 255 { yVal = 255 }
				if cbVal < 0 { cbVal = 0 } else if cbVal > 255 { cbVal = 255 }
				if crVal < 0 { crVal = 0 } else if crVal > 255 { crVal = 255 }

				b := d.yTable[yVal] + d.cbToB[cbVal]
				g := d.yTable[yVal] + d.cbToG[cbVal] + d.crToG[crVal]
				r := d.yTable[yVal] + d.crToR[crVal]

				if b >= 0 { b += 256 } else { b = 0 }
				if g >= 0 { g += 256 } else { g = 0 }
				if r >= 0 { r += 256 } else { r = 0 }

				fbIdx := (py*fbW + px) * 4
				if fbIdx+3 < len(d.Framebuffer) {
					d.Framebuffer[fbIdx] = byte(d.rangeLimit[b])
					d.Framebuffer[fbIdx+1] = byte(d.rangeLimit[g])
					d.Framebuffer[fbIdx+2] = byte(d.rangeLimit[r])
					d.Framebuffer[fbIdx+3] = 255
				}
			}

			lineOffset += d.realWidth
		}
	}
}

func (d *Decoder) convertYUVtoRGBPass2(tileX, tileY int) {
	fbW := int(d.Width)
	fbH := int(d.Height)

	if d.mode420 != 0 {
		// Pass2 in 4:2:0 mode is not used in practice.
		return
	}

	// YUV 4:4:4 pass2: differential from previous YUV.
	pixX := tileX * 8
	pixY := tileY * 8

	maxCol := 8
	if fbW-pixX < maxCol && fbW-pixX > 0 {
		maxCol = fbW - pixX
	}

	for row := 0; row < 8; row++ {
		py := pixY + row
		if py >= fbH {
			break
		}
		for col := 0; col < maxCol; col++ {
			px := pixX + col
			if px >= fbW {
				break
			}

			tileIdx := row*8 + col
			yuvIdx := (py*d.realWidth + px) * 3

			yVal := 0
			cbVal := 0
			crVal := 0

			if yuvIdx+2 < len(d.previousYUV) {
				yVal = d.previousYUV[yuvIdx] + (d.yuvTile[tileIdx] - 128)
				cbVal = d.previousYUV[yuvIdx+1] + (d.yuvTile[64+tileIdx] - 128)
				crVal = d.previousYUV[yuvIdx+2] + (d.yuvTile[128+tileIdx] - 128)
			}

			if yVal < 0 { yVal = 0 } else if yVal > 255 { yVal = 255 }
			if cbVal < 0 { cbVal = 0 } else if cbVal > 255 { cbVal = 255 }
			if crVal < 0 { crVal = 0 } else if crVal > 255 { crVal = 255 }

			b := d.yTable[yVal] + d.cbToB[cbVal]
			g := d.yTable[yVal] + d.cbToG[cbVal] + d.crToG[crVal]
			r := d.yTable[yVal] + d.crToR[crVal]

			if b >= 0 { b += 256 } else { b = 0 }
			if g >= 0 { g += 256 } else { g = 0 }
			if r >= 0 { r += 256 } else { r = 0 }

			fbIdx := (py*fbW + px) * 4
			if fbIdx+3 < len(d.Framebuffer) {
				d.Framebuffer[fbIdx] = byte(d.rangeLimit[b])
				d.Framebuffer[fbIdx+1] = byte(d.rangeLimit[g])
				d.Framebuffer[fbIdx+2] = byte(d.rangeLimit[r])
				d.Framebuffer[fbIdx+3] = 255
			}
		}
	}
}

// --- JPEG decompression ---

func (d *Decoder) decompressJPEG(tileX, tileY int, qtOffset byte) {
	// Y DC/AC table indices.
	var yDCnr byte = 0
	var yACnr byte = 0
	var cbDCnr byte = 1
	var cbACnr byte = 1
	var crDCnr byte = 1
	var crACnr byte = 1

	d.decodeHuffmanDataUnit(yDCnr, yACnr, &d.dcY, 0)
	d.inverseDCT(0, qtOffset)

	if d.mode420 == 1 {
		// 4:2:0: decode 4 Y blocks, 1 Cb, 1 Cr.
		d.decodeHuffmanDataUnit(yDCnr, yACnr, &d.dcY, 64)
		d.inverseDCT(64, qtOffset)
		d.decodeHuffmanDataUnit(yDCnr, yACnr, &d.dcY, 128)
		d.inverseDCT(128, qtOffset)
		d.decodeHuffmanDataUnit(yDCnr, yACnr, &d.dcY, 192)
		d.inverseDCT(192, qtOffset)
		d.decodeHuffmanDataUnit(cbDCnr, cbACnr, &d.dcCb, 256)
		d.inverseDCT(256, qtOffset+1)
		d.decodeHuffmanDataUnit(crDCnr, crACnr, &d.dcCr, 320)
		d.inverseDCT(320, qtOffset+1)
	} else {
		// 4:4:4: decode 1 Y, 1 Cb, 1 Cr.
		d.decodeHuffmanDataUnit(cbDCnr, cbACnr, &d.dcCb, 64)
		d.inverseDCT(64, qtOffset+1)
		d.decodeHuffmanDataUnit(crDCnr, crACnr, &d.dcCr, 128)
		d.inverseDCT(128, qtOffset+1)
	}

	// Skip writing neutral blocks to the framebuffer. When the ASPEED encoder
	// has no real data for a block, it sends DC=0 which IDCT decodes to Y=128,
	// Cb=128, Cr=128. We skip the framebuffer write to avoid visible grey, but
	// still update previousYUV so Pass2 differential blocks stay in sync.
	if d.isNeutralBlock() {
		d.updatePreviousYUV(tileX, tileY)
		return
	}

	d.convertYUVtoRGB(tileX, tileY)
}

func (d *Decoder) decompressJPEGPass2(tileX, tileY int, qtOffset byte) {
	var yDCnr byte = 0
	var yACnr byte = 0
	var cbDCnr byte = 1
	var cbACnr byte = 1
	var crDCnr byte = 1
	var crACnr byte = 1

	d.decodeHuffmanDataUnit(yDCnr, yACnr, &d.dcY, 0)
	d.inverseDCT(0, qtOffset)
	d.decodeHuffmanDataUnit(cbDCnr, cbACnr, &d.dcCb, 64)
	d.inverseDCT(64, qtOffset+1)
	d.decodeHuffmanDataUnit(crDCnr, crACnr, &d.dcCr, 128)
	d.inverseDCT(128, qtOffset+1)
	if d.isNeutralBlock() {
		d.updatePreviousYUV(tileX, tileY)
		return
	}
	d.convertYUVtoRGBPass2(tileX, tileY)
}

// --- VQ decompression ---

func (d *Decoder) decodeVQHeader(numColors int) {
	for i := 0; i < numColors; i++ {
		d.vqIndex[i] = int(d.reg0>>29) & 3
		updateFlag := (d.reg0 >> 31) & 1
		if updateFlag == 0 {
			d.updateReadBuf(3)
		} else {
			d.vqColor[d.vqIndex[i]] = (d.reg0 >> 5) & 0xFFFFFF
			d.updateReadBuf(27)
		}
	}
}

func (d *Decoder) decompressVQ(tileX, tileY int) {
	n := 0
	if d.vqBitmapBits == 0 {
		// Single color fill.
		color := d.vqColor[d.vqIndex[0]]
		yVal := int((color >> 16) & 0xFF)
		cbVal := int((color >> 8) & 0xFF)
		crVal := int(color & 0xFF)
		for i := 0; i < 64; i++ {
			d.yuvTile[n] = yVal
			d.yuvTile[n+64] = cbVal
			d.yuvTile[n+128] = crVal
			n++
		}
	} else {
		// Multiple colors with bitmap selection.
		for i := 0; i < 64; i++ {
			sel := d.lookKbits(d.vqBitmapBits)
			color := d.vqColor[d.vqIndex[sel]]
			d.yuvTile[n] = int((color >> 16) & 0xFF)
			d.yuvTile[n+64] = int((color >> 8) & 0xFF)
			d.yuvTile[n+128] = int(color & 0xFF)
			n++
			d.skipKbits(d.vqBitmapBits)
		}
	}
	if d.isNeutralBlock() {
		d.updatePreviousYUV(tileX, tileY)
		return
	}
	d.convertYUVtoRGB(tileX, tileY)
}

// --- Block index management ---

func (d *Decoder) moveBlockIndex() {
	d.txb++
	if d.mode420 == 0 {
		blocksPerRow := d.gridWidth / 8
		if blocksPerRow <= 0 {
			blocksPerRow = 1
		}
		if d.txb >= blocksPerRow {
			d.tyb++
			blocksPerCol := d.gridHeight / 8
			if blocksPerCol <= 0 {
				blocksPerCol = 1
			}
			if d.tyb >= blocksPerCol {
				d.tyb = 0
			}
			d.txb = 0
		}
	} else {
		blocksPerRow := d.gridWidth / 16
		if blocksPerRow <= 0 {
			blocksPerRow = 1
		}
		if d.txb >= blocksPerRow {
			d.tyb++
			blocksPerCol := d.gridHeight / 16
			if blocksPerCol <= 0 {
				blocksPerCol = 1
			}
			if d.tyb >= blocksPerCol {
				d.tyb = 0
			}
			d.txb = 0
		}
	}
}

// ============================================================================
// Standard JPEG tables
// ============================================================================

// Zigzag scan order.
var zigzag = [64]int{
	0, 1, 5, 6, 14, 15, 27, 28,
	2, 4, 7, 13, 16, 26, 29, 42,
	3, 8, 12, 17, 25, 30, 41, 43,
	9, 11, 18, 24, 31, 40, 44, 53,
	10, 19, 23, 32, 39, 45, 52, 54,
	20, 22, 33, 38, 46, 51, 55, 60,
	21, 34, 37, 47, 50, 56, 59, 61,
	35, 36, 48, 49, 57, 58, 62, 63,
}

// Inverse zigzag scan order.
var dezigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10,
	17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34,
	27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36,
	29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46,
	53, 60, 61, 54, 47, 55, 62, 63,
}

// Standard DC luminance Huffman table.
var stdDCLuminanceNRCodes = [17]byte{0, 0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}
var stdDCLuminanceValues = [12]int16{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}

// Standard DC chrominance Huffman table.
var stdDCChrominanceNRCodes = [17]byte{0, 0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
var stdDCChrominanceValues = [12]int16{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}

// Standard AC luminance Huffman table.
var stdACLuminanceNRCodes = [17]byte{0, 0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7D}
var stdACLuminanceValues = [162]int16{
	0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12,
	0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
	0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xA1, 0x08,
	0x23, 0x42, 0xB1, 0xC1, 0x15, 0x52, 0xD1, 0xF0,
	0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0A, 0x16,
	0x17, 0x18, 0x19, 0x1A, 0x25, 0x26, 0x27, 0x28,
	0x29, 0x2A, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39,
	0x3A, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
	0x4A, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59,
	0x5A, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
	0x6A, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79,
	0x7A, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
	0x8A, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98,
	0x99, 0x9A, 0xA2, 0xA3, 0xA4, 0xA5, 0xA6, 0xA7,
	0xA8, 0xA9, 0xAA, 0xB2, 0xB3, 0xB4, 0xB5, 0xB6,
	0xB7, 0xB8, 0xB9, 0xBA, 0xC2, 0xC3, 0xC4, 0xC5,
	0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xD2, 0xD3, 0xD4,
	0xD5, 0xD6, 0xD7, 0xD8, 0xD9, 0xDA, 0xE1, 0xE2,
	0xE3, 0xE4, 0xE5, 0xE6, 0xE7, 0xE8, 0xE9, 0xEA,
	0xF1, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8,
	0xF9, 0xFA,
}

// Standard AC chrominance Huffman table.
var stdACChrominanceNRCodes = [17]byte{0, 0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}
var stdACChrominanceValues = [162]int16{
	0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21,
	0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
	0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91,
	0xA1, 0xB1, 0xC1, 0x09, 0x23, 0x33, 0x52, 0xF0,
	0x15, 0x62, 0x72, 0xD1, 0x0A, 0x16, 0x24, 0x34,
	0xE1, 0x25, 0xF1, 0x17, 0x18, 0x19, 0x1A, 0x26,
	0x27, 0x28, 0x29, 0x2A, 0x35, 0x36, 0x37, 0x38,
	0x39, 0x3A, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
	0x49, 0x4A, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58,
	0x59, 0x5A, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
	0x69, 0x6A, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78,
	0x79, 0x7A, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
	0x88, 0x89, 0x8A, 0x92, 0x93, 0x94, 0x95, 0x96,
	0x97, 0x98, 0x99, 0x9A, 0xA2, 0xA3, 0xA4, 0xA5,
	0xA6, 0xA7, 0xA8, 0xA9, 0xAA, 0xB2, 0xB3, 0xB4,
	0xB5, 0xB6, 0xB7, 0xB8, 0xB9, 0xBA, 0xC2, 0xC3,
	0xC4, 0xC5, 0xC6, 0xC7, 0xC8, 0xC9, 0xCA, 0xD2,
	0xD3, 0xD4, 0xD5, 0xD6, 0xD7, 0xD8, 0xD9, 0xDA,
	0xE2, 0xE3, 0xE4, 0xE5, 0xE6, 0xE7, 0xE8, 0xE9,
	0xEA, 0xF2, 0xF3, 0xF4, 0xF5, 0xF6, 0xF7, 0xF8,
	0xF9, 0xFA,
}

// DC luminance Huffman fast-lookup code table.
// Format: pairs of (code_boundary, bit_length).
// DC luminance Huffman fast-lookup code table (left-aligned 16-bit thresholds).
var dcLuminanceHuffmanCode = [...]int{
	0, 0, 16384, 2, 24576, 3, 32768, 3, 40960, 3, 49152, 3, 57344, 3,
	61440, 4, 63488, 5, 64512, 6, 65024, 7, 65280, 8, 65535, 9,
}

// DC chrominance Huffman fast-lookup code table (left-aligned 16-bit thresholds).
var dcChrominanceHuffmanCode = [...]int{
	0, 0, 16384, 2, 32768, 2, 49152, 2, 57344, 3, 61440, 4, 63488, 5,
	64512, 6, 65024, 7, 65280, 8, 65408, 9, 65472, 10, 65535, 11,
}

// AC luminance Huffman fast-lookup code table (left-aligned 16-bit thresholds).
var acLuminanceHuffmanCode = [...]int{
	0, 0, 16384, 2, 32768, 2, 40960, 3, 45056, 4, 49152, 4, 53248, 4,
	55296, 5, 57344, 5, 59392, 5, 60416, 6, 61440, 6, 61952, 7, 62464, 7,
	62976, 7, 63488, 7, 63744, 8, 64000, 8, 64256, 8, 64384, 9, 64512, 9,
	64640, 9, 64768, 9, 64896, 9, 64960, 10, 65024, 10, 65088, 10, 65152, 10,
	65216, 10, 65248, 11, 65280, 11, 65312, 11, 65344, 11, 65360, 12, 65376, 12,
	65392, 12, 65408, 12, 65410, 15, 65535, 16,
}

// AC chrominance Huffman fast-lookup code table (left-aligned 16-bit thresholds).
var acChrominanceHuffmanCode = [...]int{
	0, 0, 16384, 2, 32768, 2, 40960, 3, 45056, 4, 49152, 4, 51200, 5,
	53248, 5, 55296, 5, 57344, 5, 58368, 6, 59392, 6, 60416, 6, 61440, 6,
	61952, 7, 62464, 7, 62976, 7, 63232, 8, 63488, 8, 63744, 8, 64000, 8,
	64128, 9, 64256, 9, 64384, 9, 64512, 9, 64640, 9, 64768, 9, 64896, 9,
	64960, 10, 65024, 10, 65088, 10, 65152, 10, 65216, 10, 65248, 11, 65280, 11,
	65312, 11, 65344, 11, 65360, 12, 65376, 12, 65392, 12, 65408, 12, 65412, 14,
	65414, 15, 65416, 15, 65535, 16,
}

// ============================================================================
// ASPEED quantization tables (from JTables)
// These are the 8 quality levels for luminance (Y) and chrominance (UV).
// ============================================================================

// ASPEED quantization tables from JViewer (AMI MegaRAC JTables.java).
// Values > 127 are stored as unsigned Go bytes matching the Java signed byte values.
// The setQuantizationTable function treats them as signed (via int8 cast) to match Java.

var tbl000Y = [64]byte{
	20, 13, 12, 20, 30, 50, 63, 76, 15, 15, 17, 23, 32, 72, 75, 68,
	17, 16, 20, 30, 50, 71, 86, 70, 17, 21, 27, 36, 63, 108, 100, 77,
	22, 27, 46, 70, 85, 136, 128, 96, 30, 43, 68, 80, 101, 130, 141, 115,
	61, 80, 97, 108, 128, 151, 150, 126, 90, 115, 118, 122, 140, 125, 128, 123,
}
var tbl014Y = [64]byte{
	17, 12, 10, 17, 26, 43, 55, 66, 13, 13, 15, 20, 28, 63, 65, 60,
	15, 14, 17, 26, 43, 62, 75, 61, 15, 18, 24, 31, 55, 95, 87, 67,
	19, 24, 40, 61, 74, 119, 112, 84, 26, 38, 60, 70, 88, 113, 123, 100,
	53, 70, 85, 95, 112, 132, 131, 110, 78, 100, 103, 107, 122, 109, 112, 108,
}
var tbl029Y = [64]byte{
	14, 9, 9, 14, 21, 36, 46, 55, 10, 10, 12, 17, 23, 52, 54, 49,
	12, 11, 14, 21, 36, 51, 62, 50, 12, 15, 19, 26, 46, 78, 72, 56,
	16, 19, 33, 50, 61, 98, 93, 69, 21, 31, 49, 58, 73, 94, 102, 83,
	44, 58, 70, 78, 93, 109, 108, 91, 65, 83, 86, 88, 101, 90, 93, 89,
}
var tbl043Y = [64]byte{
	11, 7, 7, 11, 17, 28, 36, 43, 8, 8, 10, 13, 18, 41, 43, 39,
	10, 9, 11, 17, 28, 40, 49, 40, 10, 12, 15, 20, 36, 62, 57, 44,
	12, 15, 26, 40, 48, 78, 74, 55, 17, 25, 39, 46, 58, 74, 81, 66,
	35, 46, 56, 62, 74, 86, 86, 72, 51, 66, 68, 70, 80, 71, 74, 71,
}
var tbl057Y = [64]byte{
	9, 6, 5, 9, 13, 22, 28, 34, 6, 6, 7, 10, 14, 32, 33, 30,
	7, 7, 9, 13, 22, 32, 38, 31, 7, 9, 12, 16, 28, 48, 45, 34,
	10, 12, 20, 31, 38, 61, 57, 43, 13, 19, 30, 36, 45, 58, 63, 51,
	27, 36, 43, 48, 57, 68, 67, 56, 40, 51, 53, 55, 63, 56, 57, 55,
}
var tbl071Y = [64]byte{
	6, 4, 3, 6, 9, 15, 19, 22, 4, 4, 5, 7, 9, 21, 22, 20,
	5, 4, 6, 9, 15, 21, 25, 21, 5, 6, 8, 10, 19, 32, 30, 23,
	6, 8, 13, 21, 25, 40, 38, 28, 9, 13, 20, 24, 30, 39, 42, 34,
	18, 24, 29, 32, 38, 45, 45, 37, 27, 34, 35, 36, 42, 37, 38, 37,
}
var tbl086Y = [64]byte{
	3, 2, 1, 3, 4, 7, 9, 11, 2, 2, 2, 3, 4, 10, 11, 10,
	2, 2, 3, 4, 7, 10, 12, 10, 2, 3, 4, 5, 9, 16, 15, 11,
	3, 4, 6, 10, 12, 20, 19, 14, 4, 6, 10, 12, 15, 19, 21, 17,
	9, 12, 14, 16, 19, 22, 22, 18, 13, 17, 17, 18, 21, 18, 19, 18,
}
var tbl100Y = [64]byte{
	2, 1, 1, 2, 3, 5, 6, 7, 1, 1, 1, 2, 3, 7, 7, 6,
	1, 1, 2, 3, 5, 7, 8, 7, 1, 2, 2, 3, 6, 10, 10, 7,
	2, 2, 4, 7, 8, 13, 12, 9, 3, 4, 6, 8, 10, 13, 14, 11,
	6, 8, 9, 10, 12, 15, 15, 12, 9, 11, 11, 12, 14, 12, 12, 12,
}
var tbl000UV = [64]byte{
	31, 33, 45, 88, 185, 185, 185, 185, 33, 39, 48, 123, 185, 185, 185, 185,
	45, 48, 105, 185, 185, 185, 185, 185, 88, 123, 185, 185, 185, 185, 185, 185,
	185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185,
	185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185, 185,
}
var tbl014UV = [64]byte{
	27, 29, 39, 76, 160, 160, 160, 160, 29, 34, 42, 107, 160, 160, 160, 160,
	39, 42, 91, 160, 160, 160, 160, 160, 76, 107, 160, 160, 160, 160, 160, 160,
	160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160,
	160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160, 160,
}
var tbl029UV = [64]byte{
	22, 24, 32, 63, 133, 133, 133, 133, 24, 28, 34, 88, 133, 133, 133, 133,
	32, 34, 75, 133, 133, 133, 133, 133, 63, 88, 133, 133, 133, 133, 133, 133,
	133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133,
	133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133, 133,
}
var tbl043UV = [64]byte{
	18, 19, 26, 51, 108, 108, 108, 108, 19, 22, 28, 72, 108, 108, 108, 108,
	26, 28, 61, 108, 108, 108, 108, 108, 51, 72, 108, 108, 108, 108, 108, 108,
	108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108,
	108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108, 108,
}
var tbl057UV = [64]byte{
	13, 14, 19, 38, 80, 80, 80, 80, 14, 17, 21, 53, 80, 80, 80, 80,
	19, 21, 45, 80, 80, 80, 80, 80, 38, 53, 80, 80, 80, 80, 80, 80,
	80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80,
	80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80, 80,
}
var tbl071UV = [64]byte{
	9, 10, 13, 26, 55, 55, 55, 55, 10, 11, 14, 37, 55, 55, 55, 55,
	13, 14, 31, 55, 55, 55, 55, 55, 26, 37, 55, 55, 55, 55, 55, 55,
	55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55,
	55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55, 55,
}
var tbl086UV = [64]byte{
	4, 5, 6, 13, 27, 27, 27, 27, 5, 5, 7, 18, 27, 27, 27, 27,
	6, 7, 15, 27, 27, 27, 27, 27, 13, 18, 27, 27, 27, 27, 27, 27,
	27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
	27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27, 27,
}
var tbl100UV = [64]byte{
	3, 3, 4, 8, 18, 18, 18, 18, 3, 3, 4, 12, 18, 18, 18, 18,
	4, 4, 10, 18, 18, 18, 18, 18, 8, 12, 18, 18, 18, 18, 18, 18,
	18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18,
	18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18, 18,
}
