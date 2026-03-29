package ikvm

// Quantization table loading.

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

// Huffman decoding.

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

// Inverse DCT.

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

// Block helpers.

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

// Color conversion.

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

// JPEG decompression.

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

// VQ decompression.

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

// Block index management.

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
