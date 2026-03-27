package ikvm

import (
	"encoding/binary"
	"fmt"
)

// IVTP (iKVM Video Transfer Protocol) packet header.
// All fields are little-endian, 8 bytes total.
type IVTPHeader struct {
	Type    uint16
	PktSize uint32
	Status  uint16
}

const IVTPHeaderSize = 8

// IVTP message types (from JViewer IVTPPktHdr.java).
const (
	IVTPHIDPkt                  = 1
	IVTPSetBandwidth            = 2
	IVTPSetFPS                  = 3
	IVTPPauseRedirection        = 4
	IVTPRefreshVideoScreen      = 5
	IVTPResumeRedirection       = 6
	IVTPSetCompressionType      = 7
	IVTPStopSessionImmediate    = 8
	IVTPBlankScreen             = 9
	IVTPGetUSBMouseMode         = 10
	IVTPGetFullScreen           = 11
	IVTPEnableEncryption        = 12
	IVTPDisableEncryption       = 13
	IVTPEncryptionStatus        = 14
	IVTPInitialEncryptionStatus = 15
	IVTPBWDetectReq             = 16
	IVTPBWDetectResp            = 17
	IVTPValidateVideoSession    = 18
	IVTPValidateVideoSessionRsp = 19
	IVTPGetKeybdLED             = 20
	IVTPGetWebToken             = 21
	IVTPMaxSessionClosing       = 22
	IVTPSessionAccepted         = 23
	IVTPMediaState              = 24
	IVTPVideoFragment           = 25
	IVTPWebPreviewerSession     = 26
	IVTPSetMouseMode            = 28
	IVTPKVMSharing              = 32
	IVTPSocketStatus            = 33
	IVTPPowerStatus             = 34
	IVTPPowerControlReq         = 35
	IVTPPowerControlResp        = 36
	IVTPConfServiceStatus       = 37
	IVTPGetActiveClients        = 39
)

// Session validation results.
const (
	SessionInvalid             = 0
	SessionValid               = 1
	KVMDisabled                = 2
	InvalidVideoSessionToken   = 3
	StopSessionConfChange      = 5
	StopSessionWebLogout       = 7
	StopSessionTimedOut        = 9
	StopSessionKVMDisconnect   = 10
)

// IUSB HID constants.
const (
	IUSBHeaderSize       = 32
	IUSBSignature        = "IUSB    "
	IUSBProtoKeybdData   = 0x10
	IUSBProtoMouseData   = 0x20
	IUSBDeviceKeybd      = 0x30
	IUSBDeviceMouse      = 0x31
	IUSBFromRemote       = 0x80
	IUSBKeybdDevNum      = 2
	IUSBKeybdIfNum       = 0
	IUSBMouseDevNum      = 2
	IUSBMouseIfNum       = 1
)

// ASPEED video header size.
const VideoHeaderSize = 86

// Fragment numbering: bit 15 set means last fragment in frame.
const FragmentLastMask = 0x8000

func (h *IVTPHeader) Encode() []byte {
	buf := make([]byte, IVTPHeaderSize)
	binary.LittleEndian.PutUint16(buf[0:2], h.Type)
	binary.LittleEndian.PutUint32(buf[2:6], h.PktSize)
	binary.LittleEndian.PutUint16(buf[6:8], h.Status)
	return buf
}

func DecodeIVTPHeader(data []byte) (*IVTPHeader, error) {
	if len(data) < IVTPHeaderSize {
		return nil, fmt.Errorf("IVTP header too short: %d bytes", len(data))
	}
	return &IVTPHeader{
		Type:    binary.LittleEndian.Uint16(data[0:2]),
		PktSize: binary.LittleEndian.Uint32(data[2:6]),
		Status:  binary.LittleEndian.Uint16(data[6:8]),
	}, nil
}

// ASPEEDVideoHeader is the 86-byte header at the start of each video frame.
type ASPEEDVideoHeader struct {
	EngVersion          uint16
	HeaderLen           uint16
	SrcX, SrcY          uint16
	SrcColorDepth       uint16
	SrcRefreshRate      uint16
	SrcModeIndex        uint8
	DstX, DstY          uint16
	DstColorDepth       uint16
	DstRefreshRate      uint16
	DstModeIndex        uint8
	FrameStartCode      uint32
	FrameNumber         uint32
	HSize, VSize        uint16
	Reserved            [8]byte
	CompressionMode     uint8
	JPEGScaleFactor     uint8
	JPEGTableSelector   uint8
	JPEGYUVTableMapping uint8
	SharpModeSelection  uint8
	AdvTableSelector    uint8
	AdvScaleFactor      uint8
	NumberOfMB          uint32
	RC4Enable           uint8
	RC4Reset            uint8
	Mode420             uint8
	DownScalingMethod   uint8
	DiffSetting         uint8
	AnalogDiffThresh    uint16
	DigitalDiffThresh   uint16
	ExtSignalEnable     uint8
	AutoMode            uint8
	VQMode              uint8
	SrcFrameSize        uint32
	CompressSize        uint32
	HDebug              uint32
	VDebug              uint32
	InputSignal         uint8
	CursorXPos          uint16
	CursorYPos          uint16
}

func DecodeASPEEDVideoHeader(data []byte) (*ASPEEDVideoHeader, error) {
	if len(data) < VideoHeaderSize {
		return nil, fmt.Errorf("video header too short: %d bytes", len(data))
	}
	h := &ASPEEDVideoHeader{}
	// Read fields in order matching JViewer's VideoHeader.set()
	off := 0
	r16 := func() uint16 { v := binary.LittleEndian.Uint16(data[off:]); off += 2; return v }
	r32 := func() uint32 { v := binary.LittleEndian.Uint32(data[off:]); off += 4; return v }
	r8 := func() uint8 { v := data[off]; off++; return v }

	h.EngVersion = r16()
	h.HeaderLen = r16()
	h.SrcX = r16()
	h.SrcY = r16()
	h.SrcColorDepth = r16()
	h.SrcRefreshRate = r16()
	h.SrcModeIndex = r8()
	h.DstX = r16()
	h.DstY = r16()
	h.DstColorDepth = r16()
	h.DstRefreshRate = r16()
	h.DstModeIndex = r8()
	h.FrameStartCode = r32()
	h.FrameNumber = r32()
	h.HSize = r16()
	h.VSize = r16()
	copy(h.Reserved[:], data[off:off+8]); off += 8
	h.CompressionMode = r8()
	h.JPEGScaleFactor = r8()
	h.JPEGTableSelector = r8()
	h.JPEGYUVTableMapping = r8()
	h.SharpModeSelection = r8()
	h.AdvTableSelector = r8()
	h.AdvScaleFactor = r8()
	h.NumberOfMB = r32()
	h.RC4Enable = r8()
	h.RC4Reset = r8()
	h.Mode420 = r8()
	h.DownScalingMethod = r8()
	h.DiffSetting = r8()
	h.AnalogDiffThresh = r16()
	h.DigitalDiffThresh = r16()
	h.ExtSignalEnable = r8()
	h.AutoMode = r8()
	h.VQMode = r8()
	h.SrcFrameSize = r32()
	h.CompressSize = r32()
	h.HDebug = r32()
	h.VDebug = r32()
	h.InputSignal = r8()
	h.CursorXPos = r16()
	h.CursorYPos = r16()

	return h, nil
}
