package ikvm

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
)

// Block type codes (top 4 bits of header word).
const (
	blockJPEGNoSkip       = 0x0
	blockJPEGPass2NoSkip  = 0x2
	blockLowJPEGNoSkip    = 0x4
	blockVQ1ColorNoSkip   = 0x5
	blockVQ2ColorNoSkip   = 0x6
	blockVQ4ColorNoSkip   = 0x7
	blockJPEGSkip         = 0x8
	blockFrameEnd         = 0x9
	blockJPEGPass2Skip    = 0xA
	blockLowJPEGSkip      = 0xC
	blockVQ1ColorSkip     = 0xD
	blockVQ2ColorSkip     = 0xE
	blockVQ4ColorSkip     = 0xF
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

	// Setup quantization tables from header parameters.
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

	// Process macroblocks.
	for d.index < compressWords {
		blockType := d.reg0 >> 28

		switch blockType {
		case blockJPEGNoSkip:
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()

		case blockJPEGSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 0)
			d.moveBlockIndex()

		case blockJPEGPass2NoSkip:
			d.updateReadBuf(4)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockJPEGPass2Skip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.decompressJPEGPass2(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockLowJPEGNoSkip:
			d.updateReadBuf(4)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockLowJPEGSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.decompressJPEG(d.txb, d.tyb, 2)
			d.moveBlockIndex()

		case blockVQ1ColorNoSkip:
			d.updateReadBuf(4)
			d.vqBitmapBits = 0
			d.decodeVQHeader(1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ1ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.vqBitmapBits = 0
			d.decodeVQHeader(1)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ2ColorNoSkip:
			d.updateReadBuf(4)
			d.vqBitmapBits = 1
			d.decodeVQHeader(2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ2ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.vqBitmapBits = 1
			d.decodeVQHeader(2)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ4ColorNoSkip:
			d.updateReadBuf(4)
			d.vqBitmapBits = 2
			d.decodeVQHeader(4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockVQ4ColorSkip:
			d.txb = int((d.reg0 & 0x0FF00000) >> 20)
			d.tyb = int((d.reg0 & 0x000FF000) >> 12)
			d.updateReadBuf(20)
			d.vqBitmapBits = 2
			d.decodeVQHeader(4)
			d.decompressVQ(d.txb, d.tyb)
			d.moveBlockIndex()

		case blockFrameEnd:
			return nil

		default:
			// Unknown block type -- skip 3 bits and advance.
			d.updateReadBuf(3)
			d.moveBlockIndex()
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
// Uses the exact same coefficients as JViewer's precalculateCrCbTables:
//   R = Y + 1.402*(Cr-128)
//   G = Y - 0.34414*(Cb-128) - 0.71414*(Cr-128)
//   B = Y + 1.772*(Cb-128)
func (d *Decoder) initColorTable() {
	for i := 0; i < 256; i++ {
		d.crToR[i] = int(float64(i-128) * 1.402)
		d.cbToB[i] = int(float64(i-128) * 1.772)
		d.crToG[i] = int(-0.71414 * float64(i-128))
		d.cbToG[i] = int(-0.34414 * float64(i-128))
	}
	// yTable: identity (Y value passed through directly)
	for i := 0; i < 256; i++ {
		d.yTable[i] = i
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
		v := int(srcTable[i]) * 16 / scaleFactor
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

				// Clamp YUV indices for table lookup.
				if yVal < 0 { yVal = 0 } else if yVal > 255 { yVal = 255 }
				if cbVal < 0 { cbVal = 0 } else if cbVal > 255 { cbVal = 255 }
				if crVal < 0 { crVal = 0 } else if crVal > 255 { crVal = 255 }

				// Java: R = Y + cr_tab[Cr], G = Y + green_tab, B = Y + cb_tab[Cb]
				r := d.yTable[yVal] + d.crToR[crVal]
				g := d.yTable[yVal] + d.crToG[crVal] + d.cbToG[cbVal]
				b := d.yTable[yVal] + d.cbToB[cbVal]

				// Clamp to 0-255
				if r < 0 { r = 0 } else if r > 255 { r = 255 }
				if g < 0 { g = 0 } else if g > 255 { g = 255 }
				if b < 0 { b = 0 } else if b > 255 { b = 255 }

				fbIdx := (py*fbW + px) * 4
				if fbIdx+3 < len(d.Framebuffer) {
					d.Framebuffer[fbIdx] = byte(b)     // B
					d.Framebuffer[fbIdx+1] = byte(g)   // G
					d.Framebuffer[fbIdx+2] = byte(r)   // R
					d.Framebuffer[fbIdx+3] = 255       // A
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

				r := d.yTable[yVal] + d.crToR[crVal]
				g := d.yTable[yVal] + d.crToG[crVal] + d.cbToG[cbVal]
				b := d.yTable[yVal] + d.cbToB[cbVal]

				if r < 0 { r = 0 } else if r > 255 { r = 255 }
				if g < 0 { g = 0 } else if g > 255 { g = 255 }
				if b < 0 { b = 0 } else if b > 255 { b = 255 }

				fbIdx := (py*fbW + px) * 4
				if fbIdx+3 < len(d.Framebuffer) {
					d.Framebuffer[fbIdx] = byte(b)
					d.Framebuffer[fbIdx+1] = byte(g)
					d.Framebuffer[fbIdx+2] = byte(r)
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

			r := d.yTable[yVal] + d.crToR[crVal]
			g := d.yTable[yVal] + d.crToG[crVal] + d.cbToG[cbVal]
			b := d.yTable[yVal] + d.cbToB[cbVal]

			if r < 0 { r = 0 } else if r > 255 { r = 255 }
			if g < 0 { g = 0 } else if g > 255 { g = 255 }
			if b < 0 { b = 0 } else if b > 255 { b = 255 }

			fbIdx := (py*fbW + px) * 4
			if fbIdx+3 < len(d.Framebuffer) {
				d.Framebuffer[fbIdx] = byte(b)
				d.Framebuffer[fbIdx+1] = byte(g)
				d.Framebuffer[fbIdx+2] = byte(r)
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
var dcLuminanceHuffmanCode = [...]int{
	0, 2,
	2, 3,
	3, 3,
	4, 3,
	5, 3,
	6, 3,
	14, 4,
	30, 5,
	62, 6,
	126, 7,
	254, 8,
	510, 9,
	65535, 9,
}

// DC chrominance Huffman fast-lookup code table.
var dcChrominanceHuffmanCode = [...]int{
	0, 2,
	1, 2,
	2, 2,
	6, 3,
	14, 4,
	30, 5,
	62, 6,
	126, 7,
	254, 8,
	510, 9,
	1022, 10,
	2046, 11,
	65535, 11,
}

// AC luminance Huffman fast-lookup code table.
var acLuminanceHuffmanCode = [...]int{
	0, 2,
	1, 2,
	4, 3,
	10, 4,
	26, 5,
	58, 6,
	120, 7,
	248, 8,
	500, 9,
	1014, 10,
	2040, 11,
	4084, 12,
	8180, 13,
	16369, 14,
	32752, 15,
	65504, 16,
	65535, 16,
}

// AC chrominance Huffman fast-lookup code table.
var acChrominanceHuffmanCode = [...]int{
	0, 2,
	1, 2,
	4, 3,
	10, 4,
	24, 5,
	56, 6,
	120, 7,
	248, 8,
	504, 9,
	1016, 10,
	2040, 11,
	4088, 12,
	8184, 13,
	16376, 14,
	32764, 15,
	65534, 16,
	65535, 16,
}

// ============================================================================
// ASPEED quantization tables (from JTables)
// These are the 8 quality levels for luminance (Y) and chrominance (UV).
// ============================================================================

// Table 0 (quality 0%) - Luminance.
var tbl000Y = [64]byte{
	13, 9, 10, 11, 10, 8, 13, 11,
	10, 11, 14, 14, 13, 15, 19, 32,
	21, 19, 18, 18, 19, 39, 28, 30,
	23, 32, 46, 41, 49, 48, 46, 41,
	45, 44, 51, 58, 74, 62, 51, 54,
	70, 55, 44, 45, 64, 87, 65, 70,
	76, 78, 82, 83, 82, 50, 62, 90,
	97, 90, 80, 96, 74, 81, 82, 79,
}

// Table 1 (quality 14%) - Luminance.
var tbl014Y = [64]byte{
	9, 6, 7, 8, 7, 6, 9, 8,
	7, 8, 10, 10, 9, 11, 14, 22,
	15, 14, 13, 13, 14, 27, 20, 21,
	17, 22, 33, 29, 34, 34, 33, 29,
	32, 31, 36, 41, 52, 44, 36, 38,
	49, 39, 31, 32, 45, 61, 46, 49,
	53, 55, 58, 58, 58, 35, 44, 64,
	68, 63, 56, 67, 52, 57, 58, 56,
}

// Table 2 (quality 29%) - Luminance.
var tbl029Y = [64]byte{
	6, 4, 5, 5, 5, 4, 6, 5,
	5, 5, 7, 7, 6, 7, 9, 15,
	10, 9, 8, 8, 9, 18, 13, 14,
	11, 15, 22, 20, 23, 23, 22, 19,
	22, 21, 25, 28, 35, 30, 25, 26,
	34, 27, 21, 22, 31, 41, 31, 34,
	36, 37, 39, 39, 39, 24, 30, 43,
	46, 43, 38, 46, 36, 39, 39, 38,
}

// Table 3 (quality 43%) - Luminance.
var tbl043Y = [64]byte{
	4, 3, 3, 4, 3, 3, 4, 4,
	3, 4, 5, 5, 4, 5, 6, 10,
	7, 6, 6, 6, 6, 13, 9, 10,
	8, 10, 15, 13, 16, 16, 15, 13,
	15, 14, 17, 19, 24, 20, 17, 18,
	23, 18, 14, 15, 21, 28, 21, 23,
	24, 25, 26, 27, 27, 16, 20, 29,
	31, 29, 26, 31, 24, 26, 26, 25,
}

// Table 4 (quality 57%) - Luminance.
var tbl057Y = [64]byte{
	3, 2, 2, 3, 2, 2, 3, 3,
	2, 3, 3, 3, 3, 3, 5, 7,
	5, 5, 4, 4, 5, 9, 6, 7,
	5, 7, 10, 9, 11, 11, 10, 9,
	10, 10, 11, 13, 16, 14, 11, 12,
	15, 12, 10, 10, 14, 19, 14, 15,
	16, 17, 18, 18, 18, 11, 14, 20,
	21, 20, 17, 21, 16, 18, 18, 17,
}

// Table 5 (quality 71%) - Luminance.
var tbl071Y = [64]byte{
	2, 1, 2, 2, 2, 1, 2, 2,
	2, 2, 2, 2, 2, 3, 3, 5,
	3, 3, 3, 3, 3, 6, 4, 5,
	4, 5, 7, 6, 7, 7, 7, 6,
	7, 7, 8, 9, 11, 9, 8, 8,
	10, 8, 7, 7, 10, 13, 10, 10,
	11, 12, 12, 12, 12, 8, 9, 13,
	14, 13, 12, 14, 11, 12, 12, 12,
}

// Table 6 (quality 86%) - Luminance.
var tbl086Y = [64]byte{
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 2, 3,
	2, 2, 2, 2, 2, 4, 3, 3,
	2, 3, 4, 4, 5, 5, 4, 4,
	4, 4, 5, 6, 7, 6, 5, 5,
	7, 5, 4, 4, 7, 8, 7, 7,
	7, 8, 8, 8, 8, 5, 6, 9,
	9, 9, 8, 9, 7, 8, 8, 8,
}

// Table 7 (quality 100%) - Luminance.
var tbl100Y = [64]byte{
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
}

// Table 0 (quality 0%) - Chrominance.
var tbl000UV = [64]byte{
	14, 14, 14, 19, 17, 19, 38, 21,
	21, 38, 79, 53, 45, 53, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
	79, 79, 79, 79, 79, 79, 79, 79,
}

// Table 1 (quality 14%) - Chrominance.
var tbl014UV = [64]byte{
	10, 10, 10, 14, 12, 14, 27, 15,
	15, 27, 56, 37, 32, 37, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
	56, 56, 56, 56, 56, 56, 56, 56,
}

// Table 2 (quality 29%) - Chrominance.
var tbl029UV = [64]byte{
	7, 7, 7, 9, 8, 9, 18, 10,
	10, 18, 38, 25, 22, 25, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
	38, 38, 38, 38, 38, 38, 38, 38,
}

// Table 3 (quality 43%) - Chrominance.
var tbl043UV = [64]byte{
	5, 5, 5, 6, 6, 6, 12, 7,
	7, 12, 25, 17, 14, 17, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
	25, 25, 25, 25, 25, 25, 25, 25,
}

// Table 4 (quality 57%) - Chrominance.
var tbl057UV = [64]byte{
	3, 3, 3, 4, 4, 4, 8, 5,
	5, 8, 17, 11, 10, 11, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
	17, 17, 17, 17, 17, 17, 17, 17,
}

// Table 5 (quality 71%) - Chrominance.
var tbl071UV = [64]byte{
	2, 2, 2, 3, 3, 3, 6, 3,
	3, 6, 12, 8, 7, 8, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
	12, 12, 12, 12, 12, 12, 12, 12,
}

// Table 6 (quality 86%) - Chrominance.
var tbl086UV = [64]byte{
	1, 1, 1, 2, 2, 2, 4, 2,
	2, 4, 8, 5, 4, 5, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
	8, 8, 8, 8, 8, 8, 8, 8,
}

// Table 7 (quality 100%) - Chrominance.
var tbl100UV = [64]byte{
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
	1, 1, 1, 1, 1, 1, 1, 1,
}
