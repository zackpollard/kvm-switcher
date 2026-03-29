// Package ikvm implements the AMI MegaRAC iKVM protocol for remote KVM
// access to servers with ASPEED AST2400/2500 BMCs.
//
// The package provides three layers:
//
//   - Client: low-level IVTP (iKVM Video Transfer Protocol) client that
//     handles TCP connection, HTTP tunnel handshake, session authentication,
//     and bidirectional message exchange.
//
//   - Decoder: ASPEED video frame decompressor supporting JPEG DCT,
//     VQ (vector quantization), and differential (Pass2) encoding modes
//     in both YUV 4:4:4 and 4:2:0 color formats.
//
//   - Bridge: high-level integration that connects noVNC WebSocket clients
//     to a BMC. It runs a background IVTP session, decodes video frames
//     into a framebuffer, and translates VNC input to USB HID reports.
//
// Typical usage via Bridge:
//
//	bridge := ikvm.NewBridge(ikvm.ClientConfig{
//	    Host:      "192.168.1.100",
//	    Port:      80,
//	    WebCookie: cookie,
//	    KVMToken:  token,
//	})
//	bridge.Start(ctx)
//	defer bridge.Stop()
//	bridge.ServeWebSocket(ws) // blocks until client disconnects
//
// For direct protocol access, use Client with an OnVideoFrame callback.
package ikvm
