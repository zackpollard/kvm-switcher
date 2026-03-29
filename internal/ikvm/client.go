package ikvm

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"

	"strings"
	"sync"
	"time"
)

// Client connects to an AMI MegaRAC BMC using the IVTP protocol.
// It handles the single-port HTTP tunnel handshake, IVTP authentication,
// and bidirectional message exchange (video frames + HID input).
type Client struct {
	conn          net.Conn
	reader        *bufio.Reader // buffered reader for the connection
	mu            sync.Mutex    // protects writes to conn
	log           Logger
	host          string
	port          int           // TCP connection port (kvmport, typically 80)
	webSecurePort int           // CONNECT tunnel target port (websecureport, typically 443)
	webCookie     string
	kvmToken      string
	useSSL        bool

	// Fragment assembly for multi-fragment video frames
	frameBuf   []byte
	frameLen   int

	// Callbacks
	OnVideoFrame func(header *ASPEEDVideoHeader, data []byte)
	OnCtrlMsg    func(hdr *IVTPHeader, payload []byte)

	stopCh   chan struct{}
	stopped  bool
}

// ClientConfig holds connection parameters for an IVTP client.
type ClientConfig struct {
	Host          string
	Port          int // TCP connection port (kvmport, typically 80)
	WebSecurePort int // CONNECT tunnel target (websecureport, typically 443)
	WebCookie     string
	KVMToken      string
	UseSSL        bool
	Logger        Logger // optional; defaults to log.Default()
}

// NewClient creates a new IVTP client (does not connect yet).
func NewClient(cfg ClientConfig) *Client {
	webSecPort := cfg.WebSecurePort
	if webSecPort == 0 {
		webSecPort = 443 // default from JNLP
	}
	l := cfg.Logger
	if l == nil {
		l = log.Default()
	}
	return &Client{
		log:           l,
		host:          cfg.Host,
		port:          cfg.Port,
		webSecurePort: webSecPort,
		webCookie:     cfg.WebCookie,
		kvmToken:      cfg.KVMToken,
		useSSL:        cfg.UseSSL,
		frameBuf:      make([]byte, 0, 9216000),
		stopCh:        make(chan struct{}),
	}
}

// Connect establishes the TCP connection, performs the single-port HTTP tunnel
// handshake, and authenticates the IVTP session.
func (c *Client) Connect() error {
	addr := net.JoinHostPort(c.host, fmt.Sprintf("%d", c.port))
	c.log.Printf("iKVM: connecting to %s", addr)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to BMC: %w", err)
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 64*1024)

	// Phase 1: Single-port HTTP tunnel handshake
	if err := c.singlePortHandshake(); err != nil {
		c.conn.Close()
		return fmt.Errorf("single-port handshake: %w", err)
	}

	c.log.Printf("iKVM: single-port handshake complete, waiting for session acceptance")
	return nil
}

// singlePortHandshake performs the HTTP CONNECT tunnel negotiation.
// Based on decompiled SinglePortKVM.java:
//   1. Send: CONNECT <host>:<port> HTTP/1.1\n cookie <webcookie>\r\n\r\n
//   2. Send: JVIEWER VIDEO cookie <webcookie>\r\n\r\n
//   3. Read response (check for ERROR)
func (c *Client) singlePortHandshake() error {
	c.conn.SetDeadline(time.Now().Add(15 * time.Second))
	defer c.conn.SetDeadline(time.Time{})

	// Message 1: HTTP CONNECT
	// JViewer connects TCP to kvmPort but the CONNECT message targets webSecurePort.
	// FormHttpRequest: "CONNECT <host>:<secWebPort> HTTP/1.1\n cookie <webcookie>\r\n\r\n"
	proto := "HTTP/1.1"
	if c.useSSL {
		proto = "HTTPS/1.1"
	}
	connectMsg := fmt.Sprintf("CONNECT %s:%d %s\n cookie %s\r\n\r\n",
		c.host, c.webSecurePort, proto, c.webCookie)
	if _, err := c.conn.Write([]byte(connectMsg)); err != nil {
		return fmt.Errorf("sending CONNECT: %w", err)
	}

	// Message 2: JVIEWER VIDEO service request
	serviceMsg := fmt.Sprintf("JVIEWER VIDEO cookie %s\r\n\r\n", c.webCookie)
	if _, err := c.conn.Write([]byte(serviceMsg)); err != nil {
		return fmt.Errorf("sending JVIEWER VIDEO: %w", err)
	}

	// Read response
	resp, err := c.readTunnelResponse()
	if err != nil {
		return fmt.Errorf("reading tunnel response: %w", err)
	}
	if strings.Contains(resp, "ERROR") {
		return fmt.Errorf("tunnel handshake rejected: %s", resp)
	}

	c.log.Printf("iKVM: tunnel response: %q", resp)
	return nil
}

// readTunnelResponse reads the HTTP response from the tunnel handshake.
// The BMC responds with an HTTP status line (e.g. "HTTP/1.0 200 OK\r\n\r\n").
// We read byte-by-byte through the bufio.Reader looking for the end of
// HTTP headers (\r\n\r\n), then stop. Any subsequent bytes (IVTP protocol)
// remain in the bufio.Reader's buffer for readMessage to consume.
func (c *Client) readTunnelResponse() (string, error) {
	var sb strings.Builder
	// Track the last 4 bytes to detect \r\n\r\n
	var trail [4]byte

	for i := 0; i < 4096; i++ {
		b, err := c.reader.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(b)

		// Shift trail window
		trail[0] = trail[1]
		trail[1] = trail[2]
		trail[2] = trail[3]
		trail[3] = b

		if trail == [4]byte{'\r', '\n', '\r', '\n'} {
			return sb.String(), nil
		}
	}
	return sb.String(), fmt.Errorf("HTTP response too long (no \\r\\n\\r\\n found)")
}

// RunSession runs the IVTP session: waits for SESSION_ACCEPTED, sends auth,
// and starts the read loop. Blocks until the session ends or Stop is called.
func (c *Client) RunSession() error {
	// Read the first IVTP message — should be SESSION_ACCEPTED (23)
	hdr, payload, err := c.readMessage()
	if err != nil {
		return fmt.Errorf("reading initial message: %w", err)
	}

	if hdr.Type != IVTPSessionAccepted {
		return fmt.Errorf("expected SESSION_ACCEPTED (23), got type %d", hdr.Type)
	}
	c.log.Printf("iKVM: session accepted (status=%d)", hdr.Status)

	// Send web token: IVTP type 21 (GET_WEB_TOKEN) with the web cookie as payload.
	// JViewer sends getM_webSession_token() which is the -webcookie arg (= SessionCookie).
	tokenBytes := []byte(c.webCookie)
	if err := c.sendMessageWithPayload(IVTPGetWebToken, 0, tokenBytes); err != nil {
		return fmt.Errorf("sending web token: %w", err)
	}
	c.log.Printf("iKVM: sent web token (%d bytes)", len(tokenBytes))

	// Send session validation: IVTP type 18 (VALIDATE_VIDEO_SESSION)
	// Payload (324 bytes): [tokenType:1][token:129][clientIP:65][username:129]
	// The 130-byte token field includes tokenType(1) + token(129).
	validPayload := make([]byte, 130+65+129) // 324 bytes
	validPayload[0] = 0 // WEB_SESSION_TOKEN
	copy(validPayload[1:130], []byte(c.kvmToken))
	localAddr := c.conn.LocalAddr().(*net.TCPAddr).IP.String()
	copy(validPayload[130:195], []byte(localAddr))
	copy(validPayload[195:324], []byte("admin"))

	if err := c.sendMessageWithPayload(IVTPValidateVideoSession, 0, validPayload); err != nil {
		return fmt.Errorf("sending session validation: %w", err)
	}
	c.log.Printf("iKVM: sent session validation (%d byte payload, kvmToken=%s, ip=%s)",
		len(validPayload), c.kvmToken, localAddr)

	// Send RESUME_REDIRECTION to start receiving video
	if err := c.SendHeader(IVTPResumeRedirection, 0, 0); err != nil {
		return fmt.Errorf("sending resume redirection: %w", err)
	}
	c.log.Printf("iKVM: sent resume redirection, starting read loop")

	// Handle the VALIDATE_VIDEO_SESSION_RESPONSE and enter main loop
	_ = payload // unused from SESSION_ACCEPTED
	return c.readLoop()
}

// readLoop is the main message processing loop.
func (c *Client) readLoop() error {
	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		hdr, payload, err := c.readMessage()
		if err != nil {
			if c.stopped {
				return nil
			}
			return fmt.Errorf("read error: %w", err)
		}

		switch hdr.Type {
		case IVTPVideoFragment:
			c.handleVideoFragment(hdr, payload)

		case IVTPValidateVideoSessionRsp:
			if len(payload) > 0 && payload[0] == 0 {
				return fmt.Errorf("session validation failed: invalid session")
			}
			c.log.Printf("iKVM: session validated (result=%d)", payload[0])
			// Send exactly what JViewer sends in OnValidVideoSession():
			// 1. Power status request (type 34, no payload)
			c.SendHeader(IVTPPowerStatus, 0, 0)
			// 2. Lock screen query
			c.sendMessageWithPayload(IVTPDisplayLock, 0, []byte{DisplayQuery})
			// 3. Get user macro
			c.SendHeader(IVTPGetUserMacro, 0, 0)
			c.log.Printf("iKVM: sent post-validation requests (power, lockscreen, macro)")

		case IVTPBlankScreen:
			c.log.Printf("iKVM: blank screen (no signal)")

		case IVTPStopSessionImmediate:
			c.log.Printf("iKVM: stop session (status=%d)", hdr.Status)
			return fmt.Errorf("session stopped by BMC (status=%d)", hdr.Status)

		case IVTPGetUSBMouseMode:
			if len(payload) > 0 {
				c.log.Printf("iKVM: mouse mode = %d", payload[0])
			}

		case IVTPGetKeybdLED:
			// Keyboard LED status update, informational only

		case IVTPPowerStatus:
			c.log.Printf("iKVM: power status = %d", hdr.Status)

		case IVTPEncryptionStatus, IVTPInitialEncryptionStatus:
			c.log.Printf("iKVM: encryption status (type=%d)", hdr.Type)

		case IVTPBWDetectResp:
			// Bandwidth detection response, discard

		case IVTPMaxSessionClosing:
			c.log.Printf("iKVM: max session reached (status=%d)", hdr.Status)
			return fmt.Errorf("max KVM sessions reached")

		case IVTPKVMSharing:
			c.log.Printf("iKVM: KVM sharing event (status=%d)", hdr.Status)

		case IVTPSetNextMaster:
			c.log.Printf("iKVM: set next master (status=%d)", hdr.Status)

		case IVTPSOCVideoEngConfig:
			// Informational — BMC reports current video engine settings

		case IVTPConfServiceStatus:
			c.log.Printf("iKVM: conf service status (%d bytes)", hdr.PktSize)

		case IVTPGetActiveClients:
			c.log.Printf("iKVM: active clients info, sending full screen request")
			c.SendHeader(IVTPGetFullScreen, 0, 0)

		case IVTPGetUserMacro:
			c.log.Printf("iKVM: user macro data (%d bytes)", hdr.PktSize)

		case IVTPMediaLicenseStatus:
			c.log.Printf("iKVM: media license status=%d", hdr.Status)

		case IVTPMediaFreeInstance:
			c.log.Printf("iKVM: media free instance status")

		default:
			if c.OnCtrlMsg != nil {
				c.OnCtrlMsg(hdr, payload)
			} else {
				c.log.Printf("iKVM: unhandled message type=%d size=%d status=%d", hdr.Type, hdr.PktSize, hdr.Status)
			}
		}
	}
}

// handleVideoFragment processes an IVTP_VIDEO_FRAGMENT message.
// Video fragments have an additional 2-byte fragment number after the header,
// included in PktSize. Fragments accumulate until the last fragment (bit 15 set).
// Fragment number (low 15 bits): 0 = first, bit 15 set = last.
func (c *Client) handleVideoFragment(hdr *IVTPHeader, payload []byte) {
	if len(payload) < 2 {
		return
	}
	fragNum := binary.LittleEndian.Uint16(payload[0:2])
	fragData := payload[2:]
	isFirst := (fragNum & 0x7FFF) == 0
	isLast := fragNum&FragmentLastMask != 0

	if isFirst {
		// Reset frame buffer for new frame
		c.frameBuf = c.frameBuf[:0]
	}
	c.frameBuf = append(c.frameBuf, fragData...)

	if isLast && c.OnVideoFrame != nil {
		if len(c.frameBuf) >= VideoHeaderSize {
			videoHdr, err := DecodeASPEEDVideoHeader(c.frameBuf)
			if err == nil {
				c.OnVideoFrame(videoHdr, c.frameBuf[VideoHeaderSize:])
			}
		}
	}
}

// readMessage reads one complete IVTP message (header + payload).
func (c *Client) readMessage() (*IVTPHeader, []byte, error) {
	hdrBuf := make([]byte, IVTPHeaderSize)
	if _, err := io.ReadFull(c.reader, hdrBuf); err != nil {
		return nil, nil, fmt.Errorf("reading IVTP header: %w", err)
	}

	hdr, err := DecodeIVTPHeader(hdrBuf)
	if err != nil {
		return nil, nil, err
	}

	var payload []byte
	if hdr.PktSize > 0 {
		if hdr.PktSize > 10*1024*1024 {
			return nil, nil, fmt.Errorf("IVTP payload too large: %d bytes", hdr.PktSize)
		}
		payload = make([]byte, hdr.PktSize)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			return nil, nil, fmt.Errorf("reading IVTP payload (%d bytes): %w", hdr.PktSize, err)
		}
	}

	return hdr, payload, nil
}

// SendHeader sends an IVTP header-only message (no payload).
func (c *Client) SendHeader(msgType uint16, pktSize uint32, status uint16) error {
	hdr := &IVTPHeader{Type: msgType, PktSize: pktSize, Status: status}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Write(hdr.Encode())
	return err
}

// sendMessageWithPayload sends an IVTP header + payload.
func (c *Client) sendMessageWithPayload(msgType uint16, status uint16, payload []byte) error {
	hdr := &IVTPHeader{
		Type:    msgType,
		PktSize: uint32(len(payload)),
		Status:  status,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write(hdr.Encode()); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// SendKeyEvent sends a keyboard HID event to the BMC.
func (c *Client) SendKeyEvent(modifiers byte, keycode byte, pressed bool) error {
	report := BuildKeyboardReport(modifiers, keycode, pressed)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Write(report)
	return err
}

// SendMouseEvent sends a mouse HID event to the BMC.
// x, y are absolute coordinates (0-32767 range), buttons is a bitmask.
func (c *Client) SendMouseEvent(buttons byte, x, y uint16, wheel int8) error {
	report := BuildAbsMouseReport(buttons, x, y, wheel)
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.conn.Write(report)
	return err
}

// Stop gracefully shuts down the IVTP session.
func (c *Client) Stop() {
	c.stopped = true
	close(c.stopCh)
	if c.conn != nil {
		// Send disconnect
		disconnectMsg := fmt.Sprintf("JVIEWER DISCONNECT Cookie %s\r\n\r\n", c.webCookie)
		c.conn.Write([]byte(disconnectMsg))
		c.conn.Close()
	}
}
