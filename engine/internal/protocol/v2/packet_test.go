package protocolv2

import (
	"bytes"
	"testing"
)

func TestMediaRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 960)
	m := Media{
		StreamID:    3,
		FormatGen:   7,
		Flags:       FlagFECEligible,
		Seq:         0xDEADBEEF,
		CaptureTsUs: 123456789,
		Payload:     payload,
	}
	packet, err := AppendMedia(nil, m)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(packet) != 21+960 {
		t.Fatalf("packet len=%d, want %d", len(packet), 21+960)
	}
	got, err := DecodeMedia(packet)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.StreamID != m.StreamID || got.FormatGen != m.FormatGen ||
		got.Flags != m.Flags || got.Seq != m.Seq || got.CaptureTsUs != m.CaptureTsUs {
		t.Fatalf("decoded %+v, want %+v", got, m)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestMediaRejectsGarbage(t *testing.T) {
	if _, err := DecodeMedia([]byte{0, 1, 2}); err == nil {
		t.Fatal("short packet accepted")
	}
	if _, err := DecodeMedia(bytes.Repeat([]byte{0}, 21)); err == nil {
		t.Fatal("bad magic accepted")
	}
	bad := make([]byte, 21)
	copy(bad, "RAU2")
	bad[4] = 2 // wrong version
	if _, err := DecodeMedia(bad); err == nil {
		t.Fatal("bad version accepted")
	}
}

func TestCapsRoundTripAndValidate(t *testing.T) {
	c := Caps{
		Codec:       CodecOpus,
		SampleRate:  48000,
		Channels:    2,
		FrameMs:     10,
		OpusBitrate: 128000,
		FEC:         1,
	}
	blob := c.Append(nil)
	got, err := DecodeCaps(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != c {
		t.Fatalf("got %+v, want %+v", got, c)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	bad := got
	bad.FrameMs = 7
	if err := bad.Validate(); err == nil {
		t.Fatal("bad frame duration accepted")
	}
}

func TestHelloRoundTrip(t *testing.T) {
	h := Hello{
		ProtoVersion: Version,
		MinProto:     Version,
		DeviceName:   "SHIVANG-PC",
		DeviceID:     [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Caps: Caps{
			Codec:      CodecPCMS16LE,
			SampleRate: 48000,
			Channels:   2,
			FrameMs:    5,
		},
	}
	blob := AppendHello(nil, h)
	got, err := DecodeHello(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.DeviceName != h.DeviceName || got.DeviceID != h.DeviceID || got.Caps != h.Caps {
		t.Fatalf("got %+v, want %+v", got, h)
	}
}

func TestStatsRoundTrip(t *testing.T) {
	s := Stats{
		LossPct:    123,
		JitterUs:   4500,
		RTTUs:      8800,
		BufDepthMs: 15,
		Underruns:  7,
		DriftPpm:   -42,
	}
	blob := AppendStats(nil, s)
	got, err := DecodeStats(blob)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != s {
		t.Fatalf("got %+v, want %+v", got, s)
	}
}
