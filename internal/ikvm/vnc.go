package ikvm

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

// vncHandshake performs the RFB protocol handshake over WebSocket.
func (b *Bridge) vncHandshake(ws *websocket.Conn) error {
	ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer ws.SetReadDeadline(time.Time{})

	// 1. Server -> Client: ProtocolVersion
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("sending version: %w", err)
	}

	// 2. Client -> Server: ProtocolVersion
	if _, _, err := ws.ReadMessage(); err != nil {
		return fmt.Errorf("reading client version: %w", err)
	}

	// 3. Server -> Client: Security types (None only)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte{1, 1}); err != nil {
		return fmt.Errorf("sending security types: %w", err)
	}

	// 4. Client -> Server: Security type selection
	if _, _, err := ws.ReadMessage(); err != nil {
		return fmt.Errorf("reading security selection: %w", err)
	}

	// 5. Server -> Client: SecurityResult (0 = OK)
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 4)); err != nil {
		return fmt.Errorf("sending security result: %w", err)
	}

	// 6. Client -> Server: ClientInit
	if _, _, err := ws.ReadMessage(); err != nil {
		return fmt.Errorf("reading ClientInit: %w", err)
	}

	// 7. Server -> Client: ServerInit
	if err := ws.WriteMessage(websocket.BinaryMessage, b.buildServerInit()); err != nil {
		return fmt.Errorf("sending ServerInit: %w", err)
	}

	b.log.Printf("iKVM bridge: VNC handshake complete (%dx%d)", b.width, b.height)
	return nil
}

// buildServerInit creates the VNC ServerInit message.
func (b *Bridge) buildServerInit() []byte {
	name := []byte("iKVM")
	buf := make([]byte, 24+len(name))

	binary.BigEndian.PutUint16(buf[0:2], b.width)
	binary.BigEndian.PutUint16(buf[2:4], b.height)

	// Pixel format: 32bpp RGBX (matches what noVNC expects by default)
	buf[4] = 32  // bits-per-pixel
	buf[5] = 24  // depth
	buf[6] = 0   // big-endian-flag (little-endian)
	buf[7] = 1   // true-colour-flag
	binary.BigEndian.PutUint16(buf[8:10], 255)  // red-max
	binary.BigEndian.PutUint16(buf[10:12], 255) // green-max
	binary.BigEndian.PutUint16(buf[12:14], 255) // blue-max
	buf[14] = 0  // red-shift (noVNC expects RGBA byte order)
	buf[15] = 8  // green-shift
	buf[16] = 16 // blue-shift

	binary.BigEndian.PutUint32(buf[20:24], uint32(len(name)))
	copy(buf[24:], name)
	return buf
}

// readVNCInput reads VNC client messages (keyboard/mouse) and forwards to BMC.
func (b *Bridge) readVNCInput(ctx context.Context, ws *websocket.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// No read deadline -- the connection stays open as long as the browser tab is open.
		// noVNC sends FramebufferUpdateRequests and mouse/key events; idle periods are normal.
		_, data, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			return fmt.Errorf("WS read: %w", err)
		}
		if len(data) == 0 {
			continue
		}

		switch data[0] {
		case 0: // SetPixelFormat -- accept but ignore, we always send our format
		case 2: // SetEncodings -- accept but ignore, we always use Raw
		case 3: // FramebufferUpdateRequest -- acknowledged, frames sent on arrival
		case 4: // KeyEvent
			if len(data) >= 8 {
				b.handleKeyEvent(data)
			}
		case 5: // PointerEvent
			if len(data) >= 6 {
				b.handlePointerEvent(data)
			}
		}
	}
}

// handleKeyEvent translates a VNC KeyEvent to IVTP keyboard HID.
// USB HID keyboard reports use cumulative modifier state -- modifiers are
// set on press and cleared on release. Regular keys send the keycode on
// press and 0 on release.
func (b *Bridge) handleKeyEvent(data []byte) {
	if b.client == nil {
		return
	}
	pressed := data[1] != 0
	keysym := binary.BigEndian.Uint32(data[4:8])
	keycode, modBit := keysymToUSBHID(keysym)

	// Handle modifier keys -- update cumulative state
	if modBit != 0 && keycode == 0 {
		if pressed {
			b.kbdModifiers |= modBit
		} else {
			b.kbdModifiers &^= modBit
		}
		// Send modifier-only report (no keycode)
		b.client.SendKeyEvent(b.kbdModifiers, 0, pressed)
		return
	}

	if keycode == 0 {
		return
	}

	if pressed {
		b.client.SendKeyEvent(b.kbdModifiers, keycode, true)
	} else {
		// Release: send current modifiers with keycode=0
		b.client.SendKeyEvent(b.kbdModifiers, 0, false)
	}
}

// handlePointerEvent translates a VNC PointerEvent to IVTP mouse HID.
func (b *Bridge) handlePointerEvent(data []byte) {
	if b.client == nil {
		return
	}
	buttons := data[1]
	x := binary.BigEndian.Uint16(data[2:4])
	y := binary.BigEndian.Uint16(data[4:6])

	b.fbMu.Lock()
	w, h := b.width, b.height
	b.fbMu.Unlock()
	if w == 0 || h == 0 {
		return
	}

	absX := uint16(uint32(x) * 32767 / uint32(w))
	absY := uint16(uint32(y) * 32767 / uint32(h))

	// VNC button mask -> USB HID buttons
	usbButtons := byte(0)
	if buttons&1 != 0 { usbButtons |= 1 } // left
	if buttons&2 != 0 { usbButtons |= 4 } // middle
	if buttons&4 != 0 { usbButtons |= 2 } // right

	var wheel int8
	if buttons&8 != 0 { wheel = 1 }
	if buttons&16 != 0 { wheel = -1 }

	b.client.SendMouseEvent(usbButtons, absX, absY, wheel)
}

// sendVNCFrames sends VNC FramebufferUpdate messages when new frames arrive.
func (b *Bridge) sendVNCFrames(ctx context.Context, ws *websocket.Conn) error {
	lastW := b.width
	lastH := b.height

	for {
		// Wait for a frame, then coalesce if more arrive quickly. During
		// rapid bursts (initial connect, resolution changes), multiple frames
		// arrive within milliseconds — some with transient decode artifacts
		// that self-correct in subsequent frames. We wait briefly for the
		// burst to settle. During normal operation (slow 5s updates), the
		// short timeout expires immediately and frames are sent with minimal
		// latency. This matches JViewer's Swing EDT repaint batching.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.frameReady:
		}
		// Brief wait to coalesce rapid frames (5ms max additional latency)
		coalesce := time.NewTimer(5 * time.Millisecond)
		for coalescing := true; coalescing; {
			select {
			case <-b.frameReady:
				// More frames arriving — reset the timer
				coalesce.Reset(5 * time.Millisecond)
			case <-coalesce.C:
				coalescing = false
			case <-ctx.Done():
				coalesce.Stop()
				return ctx.Err()
			}
		}

		b.fbMu.Lock()
		w := b.decoder.Width
		h := b.decoder.Height
		fb := b.decoder.Framebuffer
		dirty := b.fbDirty
		b.fbDirty = false

		if w == 0 || h == 0 || len(fb) < int(w)*int(h)*4 {
			b.fbMu.Unlock()
			continue
		}

		// Check for resolution change
		resChanged := w != lastW || h != lastH
		if resChanged {
			lastW = w
			lastH = h
			// Request a full refresh for the new resolution -- the BMC's first
			// frame after a resolution change is often a small diff that produces
			// garbage because previousYUV was just reset to zeros.
			if b.client != nil {
				go b.client.SendHeader(IVTPRefreshVideoScreen, 0, 0)
			}
		}

		// Copy framebuffer while holding lock, swapping B↔R channels.
		// The decoder writes BGRA but noVNC expects RGBA (it sends a
		// SetPixelFormat with red-shift=0 which we accept).
		size := int(w) * int(h) * 4
		pixelData := make([]byte, size)
		for i := 0; i < size; i += 4 {
			pixelData[i] = fb[i+2]   // R ← byte 2
			pixelData[i+1] = fb[i+1] // G ← byte 1
			pixelData[i+2] = fb[i]   // B ← byte 0
			pixelData[i+3] = fb[i+3] // A ← byte 3
		}
		b.fbMu.Unlock()

		if !dirty && !resChanged {
			continue
		}

		// Send DesktopSize if resolution changed
		if resChanged {
			resizeMsg := buildDesktopResize(w, h)
			if err := ws.WriteMessage(websocket.BinaryMessage, resizeMsg); err != nil {
				return fmt.Errorf("WS write resize: %w", err)
			}
			b.log.Printf("iKVM bridge: resolution changed to %dx%d", w, h)
			continue // noVNC will request a new frame after processing the resize
		}

		// Send FramebufferUpdate with Raw encoding
		msg := buildFramebufferUpdate(0, 0, w, h, pixelData)
		if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return fmt.Errorf("WS write frame: %w", err)
		}
	}
}

// buildFramebufferUpdate creates a VNC FramebufferUpdate message with Raw encoding.
func buildFramebufferUpdate(x, y, w, h uint16, pixels []byte) []byte {
	hdrSize := 4 + 12 // msg header + 1 rectangle header
	msg := make([]byte, hdrSize+len(pixels))
	msg[0] = 0 // FramebufferUpdate type
	binary.BigEndian.PutUint16(msg[2:4], 1) // 1 rectangle
	binary.BigEndian.PutUint16(msg[4:6], x)
	binary.BigEndian.PutUint16(msg[6:8], y)
	binary.BigEndian.PutUint16(msg[8:10], w)
	binary.BigEndian.PutUint16(msg[10:12], h)
	binary.BigEndian.PutUint32(msg[12:16], 0) // Raw encoding
	copy(msg[16:], pixels)
	return msg
}

// buildDesktopResize creates a VNC FramebufferUpdate with DesktopSize pseudo-encoding.
func buildDesktopResize(w, h uint16) []byte {
	msg := make([]byte, 16)
	msg[0] = 0 // FramebufferUpdate
	binary.BigEndian.PutUint16(msg[2:4], 1) // 1 rectangle
	binary.BigEndian.PutUint16(msg[8:10], w)
	binary.BigEndian.PutUint16(msg[10:12], h)
	binary.BigEndian.PutUint32(msg[12:16], 0xFFFFFF21) // DesktopSize
	return msg
}

// keysymToUSBHID converts an X11 keysym to USB HID keycode and modifier byte.
func keysymToUSBHID(keysym uint32) (keycode byte, modifiers byte) {
	switch keysym {
	case 0xffe1, 0xffe2: return 0, 0x02 // Shift
	case 0xffe3, 0xffe4: return 0, 0x01 // Control
	case 0xffe9, 0xffea: return 0, 0x04 // Alt
	case 0xffeb, 0xffec: return 0, 0x08 // Super
	}
	if keysym >= 0xffbe && keysym <= 0xffc9 { return byte(0x3A + (keysym - 0xffbe)), 0 } // F1-F12
	switch keysym {
	case 0xff0d: return 0x28, 0 // Return
	case 0xff1b: return 0x29, 0 // Escape
	case 0xff08: return 0x2A, 0 // BackSpace
	case 0xff09: return 0x2B, 0 // Tab
	case 0x0020: return 0x2C, 0 // Space
	case 0xff50: return 0x4A, 0 // Home
	case 0xff51: return 0x50, 0 // Left
	case 0xff52: return 0x52, 0 // Up
	case 0xff53: return 0x4F, 0 // Right
	case 0xff54: return 0x51, 0 // Down
	case 0xff55: return 0x4B, 0 // Page_Up
	case 0xff56: return 0x4E, 0 // Page_Down
	case 0xff57: return 0x4D, 0 // End
	case 0xffff: return 0x4C, 0 // Delete
	case 0xff63: return 0x49, 0 // Insert
	case 0xff14: return 0x47, 0 // Scroll_Lock
	case 0xff7f: return 0x48, 0 // Pause
	case 0xff61: return 0x46, 0 // Print
	}
	if keysym >= 0x61 && keysym <= 0x7a { return byte(0x04 + (keysym - 0x61)), 0 }       // a-z
	if keysym >= 0x41 && keysym <= 0x5a { return byte(0x04 + (keysym - 0x41)), 0x02 }    // A-Z
	if keysym >= 0x31 && keysym <= 0x39 { return byte(0x1E + (keysym - 0x31)), 0 }       // 1-9
	if keysym == 0x30 { return 0x27, 0 }                                                  // 0
	switch keysym {
	case 0x2d: return 0x2D, 0 // minus
	case 0x3d: return 0x2E, 0 // equals
	case 0x5b: return 0x2F, 0 // [
	case 0x5d: return 0x30, 0 // ]
	case 0x5c: return 0x31, 0 // backslash
	case 0x3b: return 0x33, 0 // semicolon
	case 0x27: return 0x34, 0 // apostrophe
	case 0x60: return 0x35, 0 // grave
	case 0x2c: return 0x36, 0 // comma
	case 0x2e: return 0x37, 0 // period
	case 0x2f: return 0x38, 0 // slash
	}
	return 0, 0
}
