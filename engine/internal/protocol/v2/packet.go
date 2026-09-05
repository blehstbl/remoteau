// Package protocolv2 implements the RemoteAU v2 wire protocol: the media
// datagram format (sent over QUIC DATAGRAM) and the control message framing
// (sent over the QUIC control stream). See docs/PROTOCOL_V2.md.
package protocolv2

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Version = 1

	Magic0 byte = 'R'
	Magic1 byte = 'A'
	Magic2 byte = 'U'
	Magic3 byte = '2'

	TypeAudio uint8 = 1

	MaxDatagramPayload = 1152 // QUIC-safe datagram budget
)

// Flags on media datagrams.
const (
	FlagFECEligible uint8 = 1 << 0
	FlagDTXSilence  uint8 = 1 << 1
)

// Media is the decoded form of one media datagram. Payload aliases the input
// buffer; copy before reusing it.
type Media struct {
	StreamID    uint8
	FormatGen   uint16
	Flags       uint8
	Seq         uint32
	CaptureTsUs uint64 // 40-bit, mod 2^40
	Payload     []byte
}

// AppendMedia appends one media datagram to dst.
func AppendMedia(dst []byte, m Media) ([]byte, error) {
	n := len(m.Payload)
	if n == 0 {
		return dst, errors.New("v2 media payload is empty")
	}
	if n > MaxDatagramPayload {
		return dst, fmt.Errorf("v2 media payload too large: %d > %d", n, MaxDatagramPayload)
	}
	dst = append(dst, Magic0, Magic1, Magic2, Magic3, Version, TypeAudio, m.StreamID)
	dst = binary.BigEndian.AppendUint16(dst, m.FormatGen)
	dst = append(dst, m.Flags)
	dst = binary.BigEndian.AppendUint32(dst, m.Seq)
	dst = appendU40(dst, m.CaptureTsUs)
	dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	return append(dst, m.Payload...), nil
}

// DecodeMedia decodes one media datagram. Layout:
//
//	0-3 magic | 4 version | 5 type | 6 streamID | 7-8 formatGen |
//	9 flags | 10-13 seq | 14-18 captureTs u40 | 19-20 len | payload
func DecodeMedia(packet []byte) (Media, error) {
	const headerLen = 4 + 1 + 1 + 1 + 2 + 1 + 4 + 5 + 2 // 21
	if len(packet) < headerLen {
		return Media{}, errors.New("v2 media datagram truncated")
	}
	if packet[0] != Magic0 || packet[1] != Magic1 || packet[2] != Magic2 || packet[3] != Magic3 {
		return Media{}, errors.New("v2 media magic mismatch")
	}
	if packet[4] != Version {
		return Media{}, fmt.Errorf("v2 unsupported version %d", packet[4])
	}
	if packet[5] != TypeAudio {
		return Media{}, fmt.Errorf("v2 unsupported media type %d", packet[5])
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[19:21]))
	if payloadLen == 0 {
		return Media{}, errors.New("v2 media payload empty")
	}
	if len(packet) != headerLen+payloadLen {
		return Media{}, fmt.Errorf("v2 media length mismatch: got %d, want %d", len(packet), headerLen+payloadLen)
	}
	ts := readU40(packet[14:19])
	return Media{
		StreamID:    packet[6],
		FormatGen:   binary.BigEndian.Uint16(packet[7:9]),
		Flags:       packet[9],
		Seq:         binary.BigEndian.Uint32(packet[10:14]),
		CaptureTsUs: ts,
		Payload:     packet[headerLen:],
	}, nil
}

func appendU40(dst []byte, v uint64) []byte {
	return append(dst, byte(v>>32), byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func readU40(b []byte) uint64 {
	return uint64(b[0])<<32 | uint64(b[1])<<24 | uint64(b[2])<<16 | uint64(b[3])<<8 | uint64(b[4])
}
