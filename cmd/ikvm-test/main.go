// ikvm-test is a diagnostic tool that connects to an AMI MegaRAC BMC
// using the native IVTP protocol and dumps received messages.
// Usage: go run ./cmd/ikvm-test
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/zackpollard/kvm-switcher/internal/auth"
	"github.com/zackpollard/kvm-switcher/internal/ikvm"
)

func main() {
	host := "10.10.11.14"
	port := 80
	username := "admin"
	password := "admin"

	log.Printf("Authenticating with BMC at %s:%d...", host, port)

	// Use existing MegaRAC authenticator to get KVM tokens
	authenticator, ok := auth.Get("ami_megarac")
	if !ok {
		log.Fatal("MegaRAC authenticator not registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Clean up stale sessions if requested (helps when running standalone tests)
	if os.Getenv("CLEANUP") == "1" {
		log.Printf("Cleaning up stale BMC sessions...")
		for i := 0; i < 5; i++ {
			creds, err := authenticator.CreateWebSession(ctx, host, port, username, password)
			if err != nil {
				break
			}
			authenticator.Logout(ctx, host, port, creds)
			time.Sleep(300 * time.Millisecond)
		}
		time.Sleep(3 * time.Second)
	}

	creds, connectInfo, err := authenticator.Authenticate(ctx, host, port, username, password)
	if err != nil {
		log.Fatalf("Authentication failed: %v", err)
	}

	log.Printf("Authentication successful:")
	log.Printf("  SessionCookie: %s", creds.SessionCookie)
	log.Printf("  CSRFToken: %s", creds.CSRFToken)
	log.Printf("  KVMToken: %s", creds.KVMToken)
	log.Printf("  WebCookie: %s", creds.WebCookie)

	if connectInfo.ContainerArgs == nil {
		log.Fatal("No JViewer args in connect info")
	}
	args := connectInfo.ContainerArgs

	// Connect using native IVTP protocol
	webSecPort := 443
	if args.WebSecurePort != "" {
		fmt.Sscanf(args.WebSecurePort, "%d", &webSecPort)
	}
	log.Printf("WebSecurePort: %d, SinglePortEnabled: %s", webSecPort, args.SinglePortEnabled)

	client := ikvm.NewClient(ikvm.ClientConfig{
		Host:          args.Hostname,
		Port:          port,
		WebSecurePort: webSecPort,
		WebCookie:     args.WebCookie,
		KVMToken:      args.KVMToken,
		UseSSL:        args.KVMSecure == "1",
	})

	frameCount := 0
	client.OnVideoFrame = func(header *ikvm.ASPEEDVideoHeader, data []byte) {
		frameCount++
		if frameCount <= 5 || frameCount%100 == 0 {
			log.Printf("VIDEO FRAME #%d: %dx%d mode=%d scale=%d tbl=%d macroblocks=%d compressed=%d bytes (data=%d bytes)",
				frameCount, header.DstX, header.DstY,
				header.CompressionMode, header.JPEGScaleFactor, header.JPEGTableSelector,
				header.NumberOfMB, header.CompressSize, len(data))
		}
		// Save first frame for offline decoder testing
		if frameCount == 1 {
			os.WriteFile("/tmp/ikvm-frame-header.bin", serializeHeader(header), 0644)
			os.WriteFile("/tmp/ikvm-frame-data.bin", data, 0644)
			log.Printf("Saved frame 1 to /tmp/ikvm-frame-*.bin (%d bytes data)", len(data))
		}
	}

	if err := client.Connect(); err != nil {
		log.Fatalf("Connection failed: %v", err)
	}

	// Handle ctrl+C
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down...")
		client.Stop()
	}()

	log.Printf("Starting IVTP session...")
	if err := client.RunSession(); err != nil {
		log.Printf("Session ended: %v", err)
	}

	log.Printf("Total frames received: %d", frameCount)

	// Logout
	authenticator.Logout(context.Background(), host, port, creds)
}

func serializeHeader(h *ikvm.ASPEEDVideoHeader) []byte {
	return fmt.Appendf(nil, "DstX=%d DstY=%d SrcX=%d SrcY=%d Mode=%d Scale=%d Tbl=%d Mapping=%d AdvTbl=%d AdvScale=%d MBs=%d CompressSize=%d Mode420=%d VQMode=%d AutoMode=%d RC4=%d",
		h.DstX, h.DstY, h.SrcX, h.SrcY,
		h.CompressionMode, h.JPEGScaleFactor, h.JPEGTableSelector, h.JPEGYUVTableMapping,
		h.AdvTableSelector, h.AdvScaleFactor, h.NumberOfMB, h.CompressSize,
		h.Mode420, h.VQMode, h.AutoMode, h.RC4Enable)
}
