# WebSocket Protocol for KVM Connections

## Connection Flow

1. Client opens `GET /ws/kvm/{id}` where `{id}` is a session ID from the session store.
2. The handler validates the session exists and has `Status == SessionConnected`.
3. The `"binary"` subprotocol is negotiated during the WebSocket upgrade (Gorilla upgrader with CORS origin check).
4. Based on `session.ConnMode`, the connection is dispatched to one of three proxy modes:
   - `KVMModeWebSocket` -> `proxyWSS` (iDRAC9 HTML5)
   - `KVMModeVNC` -> `proxyVNC` (iDRAC8 via VNC bridge)
   - `KVMModeIKVM` -> `proxyIKVM` (MegaRAC via iKVM bridge)
5. Audit log entries are written for `kvm_connect` (with connection mode) and `kvm_disconnect`.

**Source:** `internal/api/websocket.go` -- `HandleKVMWebSocket()`

## VNC/RFB Protocol Over WebSocket

All three modes present a VNC/RFB 3.8 interface to the browser (noVNC). The handshake sequence:

### Handshake

| Step | Direction | Message | Bytes |
|------|-----------|---------|-------|
| 1 | S -> C | ProtocolVersion | 12 (`"RFB 003.008\n"`) |
| 2 | C -> S | ProtocolVersion | 12 |
| 3 | S -> C | Security types | 1 (count) + N (type bytes) |
| 4 | C -> S | Security type selection | 1 |
| 5 | (varies) | Auth exchange | Type 1: none. Type 2: 16-byte challenge + 16-byte DES response |
| 6 | S -> C | SecurityResult | 4 (0 = OK) |
| 7 | C -> S | ClientInit | 1 (shared-flag) |
| 8 | S -> C | ServerInit | 24 (header) + N (desktop name) |

ServerInit header layout: `width(2) + height(2) + pixel-format(16) + name-length(4)`.

### Frame Format

**FramebufferUpdate** (server -> client, type 0):

```
[0]    message-type = 0
[1]    padding
[2:4]  number-of-rectangles (uint16 BE)

Per rectangle:
[0:2]  x-position (uint16 BE)
[2:4]  y-position (uint16 BE)
[4:6]  width (uint16 BE)
[6:8]  height (uint16 BE)
[8:12] encoding-type (int32 BE)
[12:]  pixel-data (Raw encoding: width * height * bpp/8 bytes)
```

**Encodings used:**

| Encoding | Value | Purpose |
|----------|-------|---------|
| Raw | 0 | Uncompressed pixel data |
| CopyRect | 1 | Rectangle copy (iDRAC8 only) |
| DesktopSize | -223 (0xFFFFFF21) | Pseudo-encoding for resolution changes |

**DesktopSize pseudo-encoding:** Sent as a FramebufferUpdate with 1 rectangle where `x=0, y=0`, `width` and `height` are the new resolution, encoding = `0xFFFFFF21`, and no pixel data. noVNC resizes its canvas on receipt.

### Input Messages (client -> server)

| Type | ID | Format |
|------|----|--------|
| SetPixelFormat | 0 | 4 + 16 bytes pixel format |
| SetEncodings | 2 | 4 + N*4 bytes encoding list |
| FramebufferUpdateRequest | 3 | 10 bytes |
| KeyEvent | 4 | 8 bytes: `[type, down-flag, pad(2), keysym(4)]` |
| PointerEvent | 5 | 6 bytes: `[type, button-mask, x(2), y(2)]` |

## Three Connection Modes

### proxyWSS -- iDRAC9 HTML5

**Path:** `proxyWSS()` in `internal/api/websocket.go`

Transparent WebSocket-to-WebSocket relay. The iDRAC9 speaks its own protocol over WSS, and noVNC on the browser side connects to the same WSS stream.

- Dials the iDRAC9's WSS endpoint (`session.KVMTarget`) with the `"binary"` subprotocol.
- Authenticates using the stored session cookie (`-http-session-`) and XSRF token.
- TLS verification respects per-server config (`tlsutil.SkipVerify`).
- After both ends are connected, runs `bidirectionalWSProxy()` -- two goroutines copying messages in each direction via `io.Copy` on NextReader/NextWriter.
- No protocol inspection or rewriting.

### proxyVNC -- iDRAC8 via VNC Bridge

**Path:** `proxyVNC()` in `internal/api/websocket.go`, `internal/vnc/bridge.go`

Bridges a WebSocket (noVNC) to a raw TCP VNC connection to iDRAC8. The VNC bridge maintains a persistent TCP connection across WebSocket client reconnects.

1. `ensureVNCBridge()` returns the existing bridge or creates a new one.
2. `Bridge.Start()` dials TCP to the BMC, performs the full VNC handshake (including VNC DES auth), and saves the `ServerInit` message.
3. `Bridge.ServeWebSocket()` checks TCP liveness (1ms read deadline probe), reconnects if dead, then:
   - Runs `clientHandshake()` with the browser: replays the RFB version/security exchange using `Security-Type = None` (auth is already done on the TCP side), then sends the saved `ServerInit` with the desktop name rewritten to `"Intel(r) AMT KVM"`.
   - Pipes data bidirectionally: BMC TCP -> WebSocket, WebSocket -> BMC TCP (with `SetEncodings` rewriting).

The desktop name rewrite to `"Intel(r) AMT KVM"` triggers noVNC's 8bpp pixel format mode. iDRAC8 crashes when receiving a 32bpp `SetPixelFormat`; 8bpp works.

**SetEncodings filtering:** Both the VNC bridge (`internal/vnc/bridge.go`) and the passthrough handshake (`internal/api/websocket.go`) rewrite `SetEncodings` (type 2) to only include `Raw(0)`, `CopyRect(1)`, `DesktopSize(-223)`. iDRAC8 silently fails to send framebuffer data when it receives unsupported encoding types.

### proxyIKVM -- MegaRAC via iKVM Bridge

**Path:** `proxyIKVM()` in `internal/api/websocket.go`, `internal/ikvm/bridge.go`, `internal/ikvm/vnc.go`

The Go process speaks the BMC's native IVTP protocol and translates it to VNC/RFB for noVNC. No VNC server runs on the BMC -- the bridge is a protocol translator.

1. WebSocket is upgraded immediately (before BMC auth) to avoid browser timeout.
2. `ensureIKVMBridge()` creates and starts the bridge if not already running.
3. `Bridge.ServeWebSocket()` waits for the bridge's `ready` channel (closed on first decoded frame), performs the VNC handshake, sends the cached framebuffer immediately, then runs input reader + frame sender in parallel.

## iKVM Bridge Specifics

**Source:** `internal/ikvm/bridge.go`, `internal/ikvm/vnc.go`

### Background Session

The bridge runs independently of WebSocket clients. `Start()` connects to the BMC via IVTP, starts the read loop (`RunSession()`), and a periodic refresh goroutine (every 30s). The bridge stays alive until `Stop()` is called or the BMC connection drops.

This means:
- Frames are decoded even when no browser is connected.
- WebSocket clients attach/detach without affecting the BMC session.
- The BMC session is marked as KVM-active to prevent the session manager from logging out or renewing it.

### VNC Handshake (Synthetic)

The iKVM bridge generates the VNC handshake itself (`vncHandshake()` in `vnc.go`):
- Advertises `RFB 003.008`, security type `None` (1).
- Sends a `ServerInit` with 32bpp RGBX pixel format (red-shift=0, green-shift=8, blue-shift=16, little-endian) and desktop name `"iKVM"`.
- Accepts and ignores `SetPixelFormat`, `SetEncodings`, and `FramebufferUpdateRequest` from the client -- the bridge always sends its own format and timing.

### BGRA to RGBA Conversion

The ASPEED video decoder outputs BGRA pixel data. Before sending to noVNC (which expects RGBA with red-shift=0), the bridge swaps B and R channels:

```
pixelData[i]   = fb[i+2]   // R <- byte 2 (was B)
pixelData[i+1] = fb[i+1]   // G <- byte 1
pixelData[i+2] = fb[i]     // B <- byte 0 (was R)
pixelData[i+3] = fb[i+3]   // A <- byte 3
```

This runs under `fbMu` lock, copying into a new buffer to avoid racing with the decoder.

### Frame Coalescing

`sendVNCFrames()` waits on the `frameReady` channel, then starts a 5ms coalescing window. Any additional frames arriving within 5ms reset the timer. This:
- Absorbs rapid bursts during initial connect and resolution changes (where transient decode artifacts self-correct in subsequent frames).
- Adds negligible latency during normal operation (BMC sends frames every ~5s).

### Instant Reconnect from Cached Framebuffer

When a new WebSocket client connects to a running bridge, `ServeWebSocket()` immediately sends the current framebuffer as a full `FramebufferUpdate` before entering the normal frame loop. This avoids a 5+ second wait for the next BMC frame.

### Resolution Change Handling

On resolution change:
1. `onVideoFrame()` detects width/height mismatch, sets `resChangeCountdown = 3` (except on the very first frame), zeros the framebuffer for 3 frames to suppress transitional garbage, and requests a full refresh from the BMC.
2. `sendVNCFrames()` sends a `DesktopSize` pseudo-encoding message, then skips the frame (noVNC will request a new one after processing the resize).

### Input Translation

- **KeyEvent:** X11 keysym -> USB HID keycode + modifier byte. Modifiers (Shift, Ctrl, Alt, Super) are tracked cumulatively. The bridge maintains `kbdModifiers` state across press/release events.
- **PointerEvent:** VNC coordinates (pixel) -> absolute USB HID coordinates (0-32767 range, scaled by `x * 32767 / width`). VNC button mask is remapped to USB HID buttons (middle and right are swapped: VNC bit 1=middle, bit 2=right; HID bit 1=right, bit 2=middle). Scroll wheel uses VNC buttons 8/16 mapped to wheel +1/-1.

## VNC Bridge Specifics

**Source:** `internal/vnc/bridge.go`

### Persistent TCP Connection

The bridge holds a single `net.Conn` to the BMC VNC server. When a WebSocket client connects:
1. TCP liveness is probed with a 1ms read deadline -- if the read returns a non-timeout error, the connection is dead and `Start()` is called to reconnect.
2. The TCP connection is NOT closed when the WebSocket disconnects -- it persists for the next client.

### ServerInit Replay

`bmcHandshake()` saves the raw `ServerInit` bytes from the BMC. `clientHandshake()` replays them to each new WebSocket client with:
- Security type `None` (the BMC auth was already done on the TCP side).
- Desktop name rewritten to `"Intel(r) AMT KVM"` to trigger noVNC 8bpp mode.

### SetEncodings Filtering

Client-to-BMC messages pass through a rewrite filter. `SetEncodings` (type 2) is replaced with `[Raw(0), CopyRect(1), DesktopSize(-223)]`. All other messages pass through unchanged. This prevents iDRAC8 from silently dropping framebuffer updates when it encounters unknown encoding types.

### VNC DES Authentication

The bridge implements VNC authentication (security type 2) using DES with bit-reversed key bytes, as specified by the RFB protocol. The password from the session config is used. If the BMC offers both type 1 (None) and type 2 (VNC Auth), type 2 is preferred.
