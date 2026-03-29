package ikvm

import "encoding/binary"

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
