package protocolv2

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Control message types.
const (
	MsgHello        uint16 = 0x0001
	MsgHelloOK      uint16 = 0x0002
	MsgPairBegin    uint16 = 0x0010
	MsgPairChallenge uint16 = 0x0011
	MsgPairConfirm  uint16 = 0x0012
	MsgPairResult   uint16 = 0x0013
	MsgStreamStart  uint16 = 0x0020
	MsgStreamAck    uint16 = 0x0021
	MsgStreamStop   uint16 = 0x0022
	MsgFormatUpdate uint16 = 0x0030
	MsgFormatAck    uint16 = 0x0031
	MsgStats        uint16 = 0x0040
	MsgStatsAck     uint16 = 0x0041
	MsgVolume       uint16 = 0x0050
	MsgPing         uint16 = 0x0060
	MsgPong         uint16 = 0x0061
	MsgResume       uint16 = 0x0070
	MsgResumeOK     uint16 = 0x0071
)

// Capabilities blob (fixed 13 bytes):
//
//	codec u8 | sampleRate u32 | channels u8 | frameMs u8 |
//	opusBitrate u32 | fec u8 | dtx u8 | complexity u8 | appID u8
type Caps struct {
	Codec      uint8  // 0=PCM_S16LE 1=Opus
	SampleRate uint32
	Channels   uint8
	FrameMs    uint8
	OpusBitrate uint32
	FEC        uint8
	DTX        uint8
	Complexity uint8
	AppID      uint8 // 0=audio 1=voip 2=lowdelay
}

// Codec identifiers.
const (
	CodecPCMS16LE uint8 = 0
	CodecOpus     uint8 = 1
)

// Opus application identifiers.
const (
	OpusAppAudio       uint8 = 0
	OpusAppVoIP        uint8 = 1
	OpusAppLowDelay    uint8 = 2
)

// 15 bytes total.
const capsSize = 15

func (c Caps) Append(dst []byte) []byte {
	dst = append(dst, c.Codec)
	dst = binary.LittleEndian.AppendUint32(dst, c.SampleRate)
	dst = append(dst, c.Channels, c.FrameMs)
	dst = binary.LittleEndian.AppendUint32(dst, c.OpusBitrate)
	dst = append(dst, c.FEC, c.DTX, c.Complexity, c.AppID)
	return dst
}

func DecodeCaps(b []byte) (Caps, error) {
	if len(b) < capsSize {
		return Caps{}, fmt.Errorf("caps truncated: %d < %d", len(b), capsSize)
	}
	return Caps{
		Codec:       b[0],
		SampleRate:  binary.LittleEndian.Uint32(b[1:5]),
		Channels:    b[5],
		FrameMs:     b[6],
		OpusBitrate: binary.LittleEndian.Uint32(b[7:11]),
		FEC:         b[11],
		DTX:         b[12],
		Complexity:  b[12 + 1],
		AppID:       b[14],
	}, nil
}

func (c Caps) Validate() error {
	if c.SampleRate < 8000 || c.SampleRate > 192000 {
		return fmt.Errorf("v2 caps: sample rate out of range: %d", c.SampleRate)
	}
	if c.Channels < 1 || c.Channels > 8 {
		return fmt.Errorf("v2 caps: channel count out of range: %d", c.Channels)
	}
	switch c.FrameMs {
	case 2, 5, 10, 20:
	default:
		return fmt.Errorf("v2 caps: unsupported frame duration %dms", c.FrameMs)
	}
	switch c.Codec {
	case CodecPCMS16LE, CodecOpus:
	default:
		return fmt.Errorf("v2 caps: unsupported codec %d", c.Codec)
	}
	if c.OpusBitrate != 0 && (c.OpusBitrate < 6000 || c.OpusBitrate > 510000) {
		return fmt.Errorf("v2 caps: opus bitrate out of range: %d", c.OpusBitrate)
	}
	return nil
}

// Payloads -------------------------------------------------------------------

// Hello is the first message both sides exchange.
type Hello struct {
	ProtoVersion uint16
	MinProto     uint16
	DeviceName   string
	DeviceID     [16]byte
	Caps         Caps // sender: what it can produce; receiver: what it can consume
}

type HelloOK struct {
	Profile     Caps // selected format
	ResumeToken [32]byte
}

type PairBegin struct {
	Nonce [16]byte
}

type PairChallenge struct {
	ECDHPub [65]byte // uncompressed P-256 point
	Salt    [16]byte
	Verify  [32]byte
}

type PairConfirm struct {
	ECDHPub [65]byte
	Verify  [32]byte
}

type PairResult struct {
	OK       uint8
	PeerName string
	PeerID   [16]byte
}

type StreamStart struct {
	Request Caps
}

type StreamAck struct {
	Active    Caps
	FormatGen uint32
}

func AppendFormatAck(dst []byte, f FormatAck) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, f.FormatGen)
	return append(dst, f.Applied)
}

func DecodeFormatAck(b []byte) (FormatAck, error) {
	if len(b) != 5 {
		return FormatAck{}, errors.New("format-ack length mismatch")
	}
	return FormatAck{
		FormatGen: binary.LittleEndian.Uint32(b[0:4]),
		Applied:   b[4],
	}, nil
}

// CodecName returns a human-readable codec name.
func (c Caps) CodecName() string {
	switch c.Codec {
	case CodecOpus:
		return "opus"
	default:
		return "pcm"
	}
}

type FormatUpdate struct {
	FormatGen uint32
	Caps      Caps
}

type FormatAck struct {
	FormatGen uint32
	Applied   uint8
}

type Stats struct {
	LossPct      uint16   // ×100 (0..10000)
	LatePct      uint16   // ×100
	JitterUs     uint32
	RTTUs        uint32
	BufDepthMs   uint16
	Underruns    uint32
	DriftPpm     int32
}

type Volume struct {
	VolumeQ16 uint16 // 0..65536
	Mute      uint8
}

type Ping struct {
	ClientTimeUs uint64
}

type Pong struct {
	ClientTimeUs uint64
	ServerTimeUs uint64
}

type Resume struct {
	Token   [32]byte
	LastSeq uint32
}

type ResumeOK struct {
	NextSeq   uint32
	FormatGen uint32
}

// Framing --------------------------------------------------------------------

// MaxControlPayload bounds control message payloads.
const MaxControlPayload = 4096

// Message is one framed control message.
type Message struct {
	Type      uint16
	Flags     uint16
	RequestID uint32
	Payload   []byte
}

// WriteMessage writes one framed message to w (flushed).
func WriteMessage(w *bufio.Writer, m Message) error {
	if len(m.Payload) > MaxControlPayload {
		return fmt.Errorf("v2 control payload too large: %d", len(m.Payload))
	}
	var header [12]byte
	binary.LittleEndian.PutUint16(header[0:2], m.Type)
	binary.LittleEndian.PutUint16(header[2:4], m.Flags)
	binary.LittleEndian.PutUint32(header[4:8], m.RequestID)
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(m.Payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(m.Payload) > 0 {
		if _, err := w.Write(m.Payload); err != nil {
			return err
		}
	}
	return w.Flush()
}

// ReadMessage reads one framed message from r.
func ReadMessage(r *bufio.Reader) (Message, error) {
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Message{}, err
	}
	n := int(binary.LittleEndian.Uint32(header[8:12]))
	if n > MaxControlPayload {
		return Message{}, fmt.Errorf("v2 control payload too large: %d", n)
	}
	m := Message{
		Type:      binary.LittleEndian.Uint16(header[0:2]),
		Flags:     binary.LittleEndian.Uint16(header[2:4]),
		RequestID: binary.LittleEndian.Uint32(header[4:8]),
	}
	if n > 0 {
		m.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, m.Payload); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

// Payload encoders/decoders ---------------------------------------------------

func AppendHello(dst []byte, h Hello) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, h.ProtoVersion)
	dst = binary.LittleEndian.AppendUint16(dst, h.MinProto)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(h.DeviceName)))
	dst = append(dst, h.DeviceID[:]...)
	dst = h.Caps.Append(dst)
	return append(dst, h.DeviceName...)
}

const helloFixedLen = 2 + 2 + 2 + 16 + capsSize

func DecodeHello(b []byte) (Hello, error) {
	if len(b) < helloFixedLen {
		return Hello{}, errors.New("hello truncated")
	}
	nameLen := int(binary.LittleEndian.Uint16(b[4:6]))
	if len(b) != helloFixedLen+nameLen {
		return Hello{}, errors.New("hello length mismatch")
	}
	var h Hello
	h.ProtoVersion = binary.LittleEndian.Uint16(b[0:2])
	h.MinProto = binary.LittleEndian.Uint16(b[2:4])
	copy(h.DeviceID[:], b[6:22])
	var err error
	h.Caps, err = DecodeCaps(b[22 : 22+capsSize])
	if err != nil {
		return Hello{}, err
	}
	h.DeviceName = string(b[helloFixedLen:])
	return h, nil
}

func AppendHelloOK(dst []byte, h HelloOK) []byte {
	dst = h.Profile.Append(dst)
	return append(dst, h.ResumeToken[:]...)
}

func DecodeHelloOK(b []byte) (HelloOK, error) {
	if len(b) != capsSize+32 {
		return HelloOK{}, errors.New("hello-ok length mismatch")
	}
	var h HelloOK
	var err error
	h.Profile, err = DecodeCaps(b[:capsSize])
	if err != nil {
		return HelloOK{}, err
	}
	copy(h.ResumeToken[:], b[capsSize:])
	return h, nil
}

func AppendStreamAck(dst []byte, s StreamAck) []byte {
	dst = s.Active.Append(dst)
	return binary.LittleEndian.AppendUint32(dst, s.FormatGen)
}

func DecodeStreamAck(b []byte) (StreamAck, error) {
	if len(b) != capsSize+4 {
		return StreamAck{}, errors.New("stream-ack length mismatch")
	}
	var s StreamAck
	var err error
	s.Active, err = DecodeCaps(b[:capsSize])
	if err != nil {
		return StreamAck{}, err
	}
	s.FormatGen = binary.LittleEndian.Uint32(b[capsSize:])
	return s, nil
}

func AppendFormatUpdate(dst []byte, f FormatUpdate) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, f.FormatGen)
	return f.Caps.Append(dst)
}

func DecodeFormatUpdate(b []byte) (FormatUpdate, error) {
	if len(b) != capsSize+4 {
		return FormatUpdate{}, errors.New("format-update length mismatch")
	}
	var f FormatUpdate
	f.FormatGen = binary.LittleEndian.Uint32(b[:4])
	var err error
	f.Caps, err = DecodeCaps(b[4:])
	return f, err
}

func AppendStats(dst []byte, s Stats) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, s.LossPct)
	dst = binary.LittleEndian.AppendUint16(dst, s.LatePct)
	dst = binary.LittleEndian.AppendUint32(dst, s.JitterUs)
	dst = binary.LittleEndian.AppendUint32(dst, s.RTTUs)
	dst = binary.LittleEndian.AppendUint16(dst, s.BufDepthMs)
	dst = binary.LittleEndian.AppendUint32(dst, s.Underruns)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(s.DriftPpm))
	return dst
}

func DecodeStats(b []byte) (Stats, error) {
	if len(b) != 2+2+4+4+2+4+4 {
		return Stats{}, errors.New("stats length mismatch")
	}
	var s Stats
	s.LossPct = binary.LittleEndian.Uint16(b[0:2])
	s.LatePct = binary.LittleEndian.Uint16(b[2:4])
	s.JitterUs = binary.LittleEndian.Uint32(b[4:8])
	s.RTTUs = binary.LittleEndian.Uint32(b[8:12])
	s.BufDepthMs = binary.LittleEndian.Uint16(b[12:14])
	s.Underruns = binary.LittleEndian.Uint32(b[14:18])
	s.DriftPpm = int32(binary.LittleEndian.Uint32(b[18:22]))
	return s, nil
}
