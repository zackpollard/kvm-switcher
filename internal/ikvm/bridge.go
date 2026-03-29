package ikvm

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Bridge connects noVNC WebSocket clients to a BMC via the native IVTP protocol.
// The bridge runs independently of WebSocket clients: Start() connects to the BMC
// and decodes frames into a framebuffer, and ServeWebSocket() attaches a VNC client
// to read that framebuffer and forward input. Multiple clients can attach/detach
// over the bridge's lifetime.
type Bridge struct {
	cfg     ClientConfig
	client  *Client
	decoder *Decoder
	log     Logger

	// VNC state (protected by fbMu)
	width  uint16
	height uint16
	fbMu   sync.Mutex
	// fbDirty is set when a new frame has been decoded and not yet consumed
	// by any WebSocket client. Each client tracks its own dirty state via
	// the frameReady broadcast channel.
	fbDirty    bool
	frameReady chan struct{} // signals from video frame callback

	// Keyboard state -- USB HID uses cumulative modifier tracking
	kbdModifiers byte

	// Resolution change tracking -- discard transitional frames
	resChangeCountdown int

	// Frame capture for debugging
	frameCount int

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	// running is true between Start() returning nil and Stop() completing.
	running bool
	// ready is closed when the first frame has been decoded, signalling that
	// width/height are valid and ServeWebSocket can send ServerInit.
	ready   chan struct{}
	runMu   sync.Mutex // protects running, ready
	stopWg  sync.WaitGroup
}

// --- Public command methods (called from API handlers) ---

// SendPowerCommand sends a power control command to the BMC.
// action: 0=off, 1=on, 2=cycle, 3=hard reset, 5=soft reset
func (b *Bridge) SendPowerCommand(action byte) error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	return b.client.SendHeader(IVTPPowerControlReq, 0, uint16(action))
}

// SendDisplayLock sends a display lock/unlock command.
// lock: true=lock, false=unlock
func (b *Bridge) SendDisplayLock(lock bool) error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	cmd := DisplayUnlock
	if lock {
		cmd = DisplayLock
	}
	return b.client.SendMessageWithPayload(IVTPDisplayLock, 0, []byte{cmd})
}

// ResetVideo sends a pause/resume cycle to reset the BMC's video capture engine.
// This forces the ASPEED video engine to re-detect the host display resolution
// and start a fresh capture. JViewer uses the same approach (Video -> Pause/Resume).
func (b *Bridge) ResetVideo() error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	// Pause redirection (type 4) -- stops video capture
	if err := b.client.SendHeader(IVTPPauseRedirection, 0, 0); err != nil {
		return fmt.Errorf("sending pause: %w", err)
	}
	// Resume redirection (type 6) -- restarts video capture with fresh mode detection
	if err := b.client.SendHeader(IVTPResumeRedirection, 0, 0); err != nil {
		return fmt.Errorf("sending resume: %w", err)
	}
	// Request full screen refresh
	b.client.SendHeader(IVTPRefreshVideoScreen, 0, 0)
	return nil
}

// SetMouseMode sets the mouse input mode.
// mode: 1=relative, 2=absolute
func (b *Bridge) SetMouseMode(mode byte) error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	return b.client.SendHeader(IVTPSetMouseMode, 0, uint16(mode))
}

// SetKeyboardLayout sends a keyboard layout change.
// layout is the JViewer layout string (e.g. "AD"=English, "FR"=French, "DE"=German).
func (b *Bridge) SetKeyboardLayout(layout string) error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	return b.client.SendMessageWithPayload(IVTPSetKbdLang, 0, []byte(layout))
}

// SendIPMICommand sends a raw IPMI command through the KVM tunnel.
func (b *Bridge) SendIPMICommand(data []byte) error {
	if b.client == nil {
		return fmt.Errorf("not connected")
	}
	return b.client.SendMessageWithPayload(IVTPIPMIRequestPkt, 0, data)
}

// NewBridge creates a bridge between noVNC (WebSocket) and BMC (IVTP).
func NewBridge(cfg ClientConfig) *Bridge {
	l := cfg.Logger
	if l == nil {
		l = log.Default()
	}
	return &Bridge{
		cfg:        cfg,
		log:        l,
		decoder:    NewDecoder(),
		width:      800,
		height:     600,
		frameReady: make(chan struct{}, 4),
		ready:      make(chan struct{}),
	}
}

// Running returns whether the bridge background loop is active.
func (b *Bridge) Running() bool {
	b.runMu.Lock()
	defer b.runMu.Unlock()
	return b.running
}

// Start connects to the BMC via IVTP and starts the background read loop
// and periodic refresh. It runs independently of any WebSocket client.
// Returns nil once the BMC connection is established and the session loop
// is running. The bridge keeps running until Stop() is called or the BMC
// connection drops.
func (b *Bridge) Start(ctx context.Context) error {
	b.runMu.Lock()
	if b.running {
		b.runMu.Unlock()
		return nil // already running
	}

	b.ctx, b.cancel = context.WithCancel(ctx)
	b.runMu.Unlock()

	// Connect to BMC via IVTP
	b.client = NewClient(b.cfg)
	b.client.OnVideoFrame = b.onVideoFrame

	if err := b.client.Connect(); err != nil {
		return fmt.Errorf("IVTP connect: %w", err)
	}

	b.runMu.Lock()
	b.running = true
	b.runMu.Unlock()

	// IVTP session (BMC reader loop)
	b.stopWg.Add(1)
	go func() {
		defer b.stopWg.Done()
		err := b.client.RunSession()
		b.log.Printf("iKVM bridge: IVTP session ended: %v", err)
		// If the BMC session dies, cancel the bridge context so all
		// goroutines (refresh ticker, attached WS clients) stop.
		b.cancel()
	}()

	// Periodic refresh: request a full video frame every 30 seconds.
	// This serves as both a keepalive AND resets any accumulated differential
	// decoding drift (the ASPEED encoder and our decoder must stay perfectly
	// in sync for Pass2 frames; periodic full refreshes re-baseline both sides).
	b.stopWg.Add(1)
	go func() {
		defer b.stopWg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-b.ctx.Done():
				return
			case <-ticker.C:
				if b.client != nil {
					b.client.SendHeader(IVTPRefreshVideoScreen, 0, 0)
				}
			}
		}
	}()

	b.log.Printf("iKVM bridge: background session started (host=%s)", b.cfg.Host)
	return nil
}

// Stop shuts down the bridge: cancels the context, stops the IVTP client,
// and waits for background goroutines to finish.
func (b *Bridge) Stop() {
	b.runMu.Lock()
	if !b.running {
		b.runMu.Unlock()
		return
	}
	b.runMu.Unlock()

	b.log.Printf("iKVM bridge: stopping background session")
	b.cancel()
	if b.client != nil {
		b.client.Stop()
	}
	b.stopWg.Wait()

	b.runMu.Lock()
	b.running = false
	// Reset ready channel for potential re-start
	b.ready = make(chan struct{})
	b.runMu.Unlock()

	b.log.Printf("iKVM bridge: background session stopped")
}

// ServeWebSocket handles a noVNC WebSocket client. It performs the VNC
// handshake, then loops sending framebuffer updates and reading input.
// Blocks until the WebSocket closes or the bridge context is cancelled.
// Multiple clients can be served over the bridge's lifetime (sequentially
// or concurrently).
func (b *Bridge) ServeWebSocket(ws *websocket.Conn) error {
	if !b.Running() {
		return fmt.Errorf("bridge not running")
	}

	// Wait for the bridge to be ready (first frame decoded) so we can
	// send the correct resolution in ServerInit. Time out after 30s.
	select {
	case <-b.ready:
	case <-b.ctx.Done():
		return fmt.Errorf("bridge stopped before ready")
	case <-time.After(30 * time.Second):
		// Proceed with default 800x600 -- noVNC will get a DesktopSize
		// update once the first real frame arrives.
		b.log.Printf("iKVM bridge: timed out waiting for first frame, proceeding with default resolution")
	}

	// VNC handshake with noVNC
	if err := b.vncHandshake(ws); err != nil {
		return fmt.Errorf("VNC handshake: %w", err)
	}

	// Send the current framebuffer immediately so reconnecting clients don't
	// have to wait for the next BMC frame (which could be 5+ seconds away).
	b.fbMu.Lock()
	if b.decoder.Width > 0 && b.decoder.Height > 0 && len(b.decoder.Framebuffer) >= int(b.decoder.Width)*int(b.decoder.Height)*4 {
		w, h := b.decoder.Width, b.decoder.Height
		size := int(w) * int(h) * 4
		fb := b.decoder.Framebuffer
		pixelData := make([]byte, size)
		for i := 0; i < size; i += 4 {
			pixelData[i] = fb[i+2]
			pixelData[i+1] = fb[i+1]
			pixelData[i+2] = fb[i]
			pixelData[i+3] = fb[i+3]
		}
		b.fbMu.Unlock()
		msg := buildFramebufferUpdate(0, 0, w, h, pixelData)
		ws.WriteMessage(websocket.BinaryMessage, msg)
	} else {
		b.fbMu.Unlock()
	}

	// Run input reader and frame sender in parallel, scoped to this WS client.
	errCh := make(chan error, 2)

	// VNC client -> BMC (keyboard/mouse input reader)
	go func() {
		errCh <- b.readVNCInput(b.ctx, ws)
	}()

	// BMC -> VNC client (video frame sender)
	go func() {
		errCh <- b.sendVNCFrames(b.ctx, ws)
	}()

	// Wait for first error (WS close or bridge shutdown)
	err := <-errCh
	if err != nil {
		b.log.Printf("iKVM bridge: WebSocket client disconnected: %v", err)
	}
	return err
}

// onVideoFrame is called by the IVTP client when a complete video frame arrives.
func (b *Bridge) onVideoFrame(header *ASPEEDVideoHeader, data []byte) {
	b.fbMu.Lock()
	defer b.fbMu.Unlock()

	b.frameCount++

	prevW, prevH := b.width, b.height

	if err := b.decoder.Decode(header, data); err != nil {
		// Frame had advance block types that stopped processing. Request a
		// refresh so the BMC sends a full frame to fill in any stale blocks.
		if b.client != nil {
			go b.client.SendHeader(IVTPRefreshVideoScreen, 0, 0)
		}
		return
	}

	b.width = b.decoder.Width
	b.height = b.decoder.Height

	// On resolution change, discard the first few transitional frames. The BMC's
	// video encoder sends garbage during mode switches: DC=0 grey blocks, partial
	// captures with random chrominance, and advance-QT JPEG blocks. JViewer hides
	// this via its Xvfb pipeline latency and exception-based frame discard.
	// We clear the framebuffer for a few frames after each resolution change while
	// still decoding (to keep the decoder's internal state current for subsequent
	// differential frames).
	// Discard transitional frames on resolution changes, but NOT on the initial
	// frame (frameCount==1). The first frames after connect have valid content
	// that must not be discarded. Only subsequent resolution changes (e.g. BIOS
	// POST switching modes) produce transitional garbage.
	if b.width != prevW || b.height != prevH {
		if b.frameCount > 1 {
			b.resChangeCountdown = 3
		}
		if b.client != nil {
			go b.client.SendHeader(IVTPRefreshVideoScreen, 0, 0)
		}
	}
	if b.resChangeCountdown > 0 {
		for i := range b.decoder.Framebuffer {
			b.decoder.Framebuffer[i] = 0
		}
		b.resChangeCountdown--
	}

	b.fbDirty = true
	select {
	case b.frameReady <- struct{}{}:
	default:
	}

	// Signal readiness on the first successfully decoded frame.
	b.runMu.Lock()
	select {
	case <-b.ready:
		// Already closed
	default:
		close(b.ready)
	}
	b.runMu.Unlock()
}

// Screenshot returns the current framebuffer as PNG bytes.
// Works anytime the bridge is running, not just when a WebSocket is connected.
func (b *Bridge) Screenshot() []byte {
	b.fbMu.Lock()
	defer b.fbMu.Unlock()
	w, h := int(b.decoder.Width), int(b.decoder.Height)
	if w == 0 || h == 0 || len(b.decoder.Framebuffer) < w*h*4 {
		return nil
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	fb := b.decoder.Framebuffer
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			off := (y*w + x) * 4
			img.Pix[(y*w+x)*4+0] = fb[off+2] // R from B
			img.Pix[(y*w+x)*4+1] = fb[off+1] // G
			img.Pix[(y*w+x)*4+2] = fb[off+0] // B from R
			img.Pix[(y*w+x)*4+3] = 255
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
