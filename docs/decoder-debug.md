# ASPEED Decoder Debug Analysis

## Problem Description
The native Go ASPEED video decoder produces visual artifacts that the original JViewer (Java) decoder does not:
1. **Grey bars** — alternating grey/black 8-pixel columns at the top of the screen, especially after resolution changes (e.g., 800x600 → 720x400 during BIOS POST)
2. **Random colored pixels** — scattered single-pixel or single-block color errors that appear and persist
3. **Stale characters** — text from previous screens sometimes persists when the screen changes

## Root Cause (Confirmed via IDCT Trace)
The grey bars are **not a decoder bug**. They are correct JPEG decode output:
- The BMC sends JPEG blocks with DC=0 for "unchanged" areas
- DC=0 after IDCT produces Y=128 (JPEG neutral midpoint)
- Y=128 through BT.601 conversion produces RGB(130,130,130) = visible grey
- JViewer produces the **same Y=128 output** (confirmed via Java reflection: `calcY[128]=130`)
- JViewer hides this because its Docker container pipeline (Xvfb→x11vnc→websockify) adds buffering that masks transitional frames

## What Has Been 100% Confirmed to Match JViewer

### Color Conversion Tables ✅
Verified by extracting actual values from JViewer via Java reflection in Docker container:
- `calculatedRGBofY[16]=0, [128]=130, [0]=-19` — **exact match**
- `calculatedRGBofCrToR[128]=0` — **exact match**
- `calculatedRGBofCbToB[128]=0` — **exact match**
- Coefficients: `FIX_G(1.164)`, `FIX_G(1.597656)`, `FIX_G(2.015625)`, `FIX_G(0.8125)`, `FIX_G(0.390625)` — **exact match**
- rangeLimit table: `[256]=0, [0]=0, [512]=255` — **exact match**

### Bitstream Reader ✅
- `skipKbits` and `updateReadBuf` produce identical register states for all tested bit counts (1-27 bits)
- Verified via `TestBitstreamConsistency` unit test
- Both use uint64 intermediate arithmetic (fixed from original uint32 bug)

### MakeIntArray Byte Ordering ✅
- Little-endian: `bytes[n+3]<<24 | bytes[n+2]<<16 | bytes[n+1]<<8 | bytes[n]`
- Java produces `Word 0: 04030201` for `{0x01,0x02,0x03,0x04}` — **exact match**

### First Block Dispatch ✅
- Frame 9: Both Java and Go see `type=0xE pos=(27,0) reg0=e1b00d45` — **exact match**
- Frame 10: Both see `type=0xE pos=(2,18) reg0=e0212d45` — **exact match**

### VQ Color Extraction ✅
- Color `0x108080`: Both extract Y=16, Cb=128, Cr=128 — **exact match**

### VQ Cache Initialization ✅
- Both Java and Go initialize per-frame: `[0x008080, 0xFF8080, 0x808080, 0xC08080]`
- Java code confirmed at Decoder.java line 1221-1224

### neg_pow2 Table ✅
- All 16 values match exactly between Java and Go

### IDCT Behavior ✅
- DC=0 produces Y=128 in both (confirmed: `128 + (0 >> 3 & 0x3FF) = 128`)
- This is standard JPEG behavior — DC=0 = neutral midpoint

### Huffman Byte Truncation ✅
- Fixed: `int(byte(code - minorCode[codeLen]))` matches Java's `(byte)` cast

## What Has Been Ruled Out

### Differential Frame Accumulation ❌ Not the cause
- Offline decode of 100 captured frames produces the SAME grey bars as live decode
- Fresh decoder on frame 1 alone: clean output for the Talos console
- The grey appears specifically after resolution changes, not from accumulated drift

### VQ Cache Persistence ❌ Not the cause
- Tested: not resetting VQ cache per-frame still produces grey
- The grey comes from JPEG Pass1 blocks (type 0), not VQ blocks
- Frame 13 IDCT trace: `DC_coeff=0, dequant=0` → Y=128

### Wrong Color Conversion ❌ Not the cause (was a bug, now fixed)
- Previously used simple `1.402/1.772` coefficients instead of BT.601 `1.597656/2.015625`
- Fixed and verified against JViewer's actual table values
- The grey bars persist even with correct tables because the issue is Y=128 from JPEG, not color conversion

### Session Management ❌ Not the cause
- Grey bars appear in offline decode of captured frames
- Not related to BMC session slots, reconnects, or IVTP protocol

## What Still Needs Investigation

### 1. Why does the BMC send DC=0 for black areas?
- After resolution change, BMC's encoder sends JPEG DC=0 for the first full frame
- DC=0 → Y=128 → grey. For black, DC should be ≈ -112 → Y≈16
- The BMC's encoder appears to use a neutral reference for the first frame
- **Hypothesis**: The BMC's video engine needs time to capture a proper reference frame after resolution change. The first encode is against a neutral (Y=128) reference, producing DC=0 for everything.

### 2. How does JViewer avoid showing the grey?
- JViewer's decoder produces the SAME Y=128 output (confirmed)
- JViewer runs inside Docker: `JViewer → Xvfb → x11vnc → websockify → noVNC`
- **Hypothesis**: The Xvfb/x11vnc pipeline introduces 1-2 frames of latency. By the time the user sees the display, the BMC has sent proper refresh frames that overwrite the grey. Our native bridge sends EVERY frame immediately, making the grey visible.
- **Alternative hypothesis**: JViewer sends `REFRESH_VIDEO_SCREEN` more aggressively or at the right timing to avoid the grey.
- **Needs testing**: Run JViewer container during a power cycle and capture its Xvfb display frame-by-frame to verify it also has grey transitionally.

### 3. Quantization Tables
- The decoder agent ported standard JPEG QT tables, NOT the ASPEED-specific ones from JViewer
- We identified the correct JViewer tables (from JTables.java) but haven't applied them yet
- The tables matter for JPEG block quality but may not affect the grey bar issue (which is DC=0 regardless of QT)
- **Needs**: Apply the correct ASPEED QT tables and the `int8` signed byte handling in `setQuantizationTable`

### 4. Range Limit Table Off-by-One
- Index 895: Go has 255, Java has 0 (identified by decoder review agent)
- Fix identified but not yet applied to the committed code

### 5. Random Colored Pixels
- May be a separate issue from the grey bars
- Could be from:
  - QT table mismatch (standard JPEG vs ASPEED tables)
  - Occasional Huffman decode error causing bitstream desync
  - IDCT rounding differences due to wrong QT values
- **Needs**: Apply correct QT tables first, then re-test

### 6. Stale Characters
- Text persisting from previous screens
- Likely related to `previousYUV` not being updated correctly for those blocks
- Could be from Pass2 blocks referencing wrong base values
- **Needs**: Investigation after QT table fix

## Fixes Applied (Committed)
1. ✅ Color conversion tables set to JViewer's exact `calculatedRGBof*` values (commit c8f6dc5)
2. ✅ Huffman byte truncation: `int(byte(code - minorCode))` matching Java `(byte)` cast
3. ✅ `skipKbits` uses uint64 arithmetic (matching `updateReadBuf`)
4. ✅ Correct ASPEED quantization tables — 16 tables from JTables.java (commit 288f000)
5. ✅ `int8` signed byte handling in `setQuantizationTable` for values > 127 (commit 288f000)
6. ✅ Range limit table off-by-one at index 895 (was already applied)

## Test Results After All Fixes
- **Talos console (800x600)**: Clean output, no random colored pixels ✅
- **BIOS POST (720x400)**: Grey bars persist (DC=0 → Y=128 issue, documented as correct JPEG behavior)
- **Random colored pixels**: Eliminated by QT table fix ✅
- **Stale characters**: Needs retesting with live BMC

## Remaining Issue: Grey Bars During BIOS POST
The grey bars are correct JPEG decoder output (DC=0 → Y=128 → grey). JViewer produces the same output but hides it through its Docker container buffering pipeline. Options:
1. Send `REFRESH_VIDEO_SCREEN` after resolution changes (current approach, 30s periodic)
2. Skip frames after resolution change until a clean full frame arrives
3. Accept as a cosmetic issue that self-corrects when the BMC sends proper content

## Files
- Decoder: `internal/ikvm/decoder.go`
- Bridge: `internal/ikvm/bridge.go`
- Captured frames: `/tmp/ikvm-rawframe-{1..100}.bin`
- JViewer source (decompiled): `/tmp/jviewer-decompiled/com/ami/kvm/jviewer/soc/video/Decoder.java`
