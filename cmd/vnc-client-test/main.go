// vnc-client-test simulates a noVNC client connecting to the kvm-switcher
// server via WebSocket. It performs the VNC handshake, requests framebuffer
// updates, and saves received frames as PNG files.
//
// Usage: go run ./cmd/vnc-client-test <session-id>
package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: vnc-client-test <session-id>")
	}
	sessionID := os.Args[1]
	wsURL := fmt.Sprintf("ws://localhost:8080/ws/kvm/%s", sessionID)
	log.Printf("Connecting to %s", wsURL)

	dialer := websocket.Dialer{Subprotocols: []string{"binary"}}
	ws, _, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		log.Fatalf("Dial: %v", err)
	}
	defer ws.Close()

	// === VNC Handshake ===
	_, data, err := ws.ReadMessage()
	if err != nil {
		log.Fatalf("Read version: %v", err)
	}
	log.Printf("Server version: %q", string(data))

	ws.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n"))

	_, data, _ = ws.ReadMessage()
	log.Printf("Security types: %v", data)

	ws.WriteMessage(websocket.BinaryMessage, []byte{1}) // None

	_, data, _ = ws.ReadMessage()
	log.Printf("Security result: %v (0=OK)", data)

	ws.WriteMessage(websocket.BinaryMessage, []byte{1}) // SharedFlag=true

	_, data, _ = ws.ReadMessage()
	if len(data) < 24 {
		log.Fatalf("ServerInit too short: %d bytes", len(data))
	}
	w := binary.BigEndian.Uint16(data[0:2])
	h := binary.BigEndian.Uint16(data[2:4])
	log.Printf("ServerInit: %dx%d bpp=%d depth=%d", w, h, data[4], data[5])

	// === Request frames ===
	fbReq := make([]byte, 10)
	fbReq[0] = 3 // FramebufferUpdateRequest
	fbReq[1] = 0 // incremental=0 (full)
	binary.BigEndian.PutUint16(fbReq[6:8], w)
	binary.BigEndian.PutUint16(fbReq[8:10], h)
	ws.WriteMessage(websocket.BinaryMessage, fbReq)

	log.Println("Waiting for frames...")
	for i := 0; i < 10; i++ {
		ws.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, data, err = ws.ReadMessage()
		if err != nil {
			log.Printf("Read error: %v", err)
			break
		}

		if len(data) < 4 || data[0] != 0 {
			log.Printf("Non-update message: type=%d len=%d", data[0], len(data))
			continue
		}

		numRects := binary.BigEndian.Uint16(data[2:4])
		if numRects == 0 || len(data) < 16 {
			continue
		}

		rw := binary.BigEndian.Uint16(data[8:10])
		rh := binary.BigEndian.Uint16(data[10:12])
		enc := binary.BigEndian.Uint32(data[12:16])
		pixelBytes := len(data) - 16
		log.Printf("Frame %d: %dx%d enc=%d (%d bytes pixels)", i, rw, rh, enc, pixelBytes)

		if enc == 0 && pixelBytes >= int(rw)*int(rh)*4 {
			pix := data[16:]
			img := image.NewRGBA(image.Rect(0, 0, int(rw), int(rh)))
			for y := 0; y < int(rh); y++ {
				for x := 0; x < int(rw); x++ {
					off := (y*int(rw) + x) * 4
					// Server sends BGRA (blue at offset 0), convert to RGBA
					img.Pix[(y*int(rw)+x)*4+0] = pix[off+2] // R
					img.Pix[(y*int(rw)+x)*4+1] = pix[off+1] // G
					img.Pix[(y*int(rw)+x)*4+2] = pix[off+0] // B
					img.Pix[(y*int(rw)+x)*4+3] = 255
				}
			}
			path := fmt.Sprintf("/tmp/ikvm-e2e-frame-%d.png", i)
			f, _ := os.Create(path)
			png.Encode(f, img)
			f.Close()
			log.Printf("Saved %s", path)
		} else if enc == 0xFFFFFF21 {
			log.Printf("DesktopSize pseudo-encoding: %dx%d", rw, rh)
			w, h = rw, rh
			binary.BigEndian.PutUint16(fbReq[6:8], w)
			binary.BigEndian.PutUint16(fbReq[8:10], h)
		}

		// Request next frame (incremental)
		fbReq[1] = 1
		ws.WriteMessage(websocket.BinaryMessage, fbReq)
	}
	log.Println("Test complete")
}
