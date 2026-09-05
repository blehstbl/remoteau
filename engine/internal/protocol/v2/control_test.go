package protocolv2

import (
	"bufio"
	"bytes"
	"testing"
)

func TestControlPayloadRoundTrips(t *testing.T) {
	tests := []struct {
		name   string
		append func([]byte) []byte
		decode func([]byte) (bool, error) // returns ok + err
	}{
		{
			name:   "quality mode",
			append: func(dst []byte) []byte { return AppendQualityMode(dst, QualityMode{Mode: QualityModeRobust}) },
			decode: func(b []byte) (bool, error) {
				got, err := DecodeQualityMode(b)
				return err == nil && got.Mode == QualityModeRobust, err
			},
		},
		{
			name: "set source named device",
			append: func(dst []byte) []byte {
				return AppendSetSource(dst, SetSource{Kind: SetSourceDevice, Name: "Speakers (Realtek)"})
			},
			decode: func(b []byte) (bool, error) {
				got, err := DecodeSetSource(b)
				return err == nil && got.Kind == SetSourceDevice && got.Name == "Speakers (Realtek)", err
			},
		},
		{
			name:   "set source test tone",
			append: func(dst []byte) []byte { return AppendSetSource(dst, SetSource{Kind: SetSourceTestTone}) },
			decode: func(b []byte) (bool, error) {
				got, err := DecodeSetSource(b)
				return err == nil && got.Kind == SetSourceTestTone && got.Name == "", err
			},
		},
		{
			name: "set source ack",
			append: func(dst []byte) []byte {
				return AppendSetSourceAck(dst, SetSourceAck{OK: 1, Detail: "source: Speakers"})
			},
			decode: func(b []byte) (bool, error) {
				got, err := DecodeSetSourceAck(b)
				return err == nil && got.OK == 1 && got.Detail == "source: Speakers", err
			},
		},
		{
			name:   "receiver state",
			append: func(dst []byte) []byte { return AppendReceiverState(dst, ReceiverState{State: ReceiverStatePlaying}) },
			decode: func(b []byte) (bool, error) {
				got, err := DecodeReceiverState(b)
				return err == nil && got.State == ReceiverStatePlaying &&
					got.String() == "playing", err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blob := tt.append(nil)
			if ok, err := tt.decode(blob); err != nil {
				t.Fatalf("decode: %v", err)
			} else if !ok {
				t.Fatal("round-trip mismatch")
			}
		})
	}
}

func TestControlPayloadsRejectInvalid(t *testing.T) {
	tests := []struct {
		name   string
		blob   []byte
		decode func([]byte) error
	}{
		{
			name:   "quality mode wrong length",
			blob:   []byte{1, 2},
			decode: func(b []byte) error { _, err := DecodeQualityMode(b); return err },
		},
		{
			name:   "quality mode unknown",
			blob:   []byte{5},
			decode: func(b []byte) error { _, err := DecodeQualityMode(b); return err },
		},
		{
			name:   "set source truncated",
			blob:   []byte{1},
			decode: func(b []byte) error { _, err := DecodeSetSource(b); return err },
		},
		{
			name:   "set source length mismatch",
			blob:   []byte{1, 3, 0, 'a', 'b'},
			decode: func(b []byte) error { _, err := DecodeSetSource(b); return err },
		},
		{
			name:   "set source unknown kind",
			blob:   AppendSetSource(nil, SetSource{Kind: 3, Name: "x"}),
			decode: func(b []byte) error { _, err := DecodeSetSource(b); return err },
		},
		{
			name:   "set source ack truncated",
			blob:   []byte{1, 0},
			decode: func(b []byte) error { _, err := DecodeSetSourceAck(b); return err },
		},
		{
			name:   "set source ack length mismatch",
			blob:   []byte{1, 5, 0, 'o', 'k'},
			decode: func(b []byte) error { _, err := DecodeSetSourceAck(b); return err },
		},
		{
			name:   "receiver state wrong length",
			blob:   []byte{2, 3},
			decode: func(b []byte) error { _, err := DecodeReceiverState(b); return err },
		},
		{
			name:   "receiver state unknown",
			blob:   []byte{7},
			decode: func(b []byte) error { _, err := DecodeReceiverState(b); return err },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.decode(tt.blob); err == nil {
				t.Fatalf("invalid payload accepted: % x", tt.blob)
			}
		})
	}
}

func TestReceiverStateString(t *testing.T) {
	for state, want := range map[uint8]string{
		ReceiverStateConnecting:   "connecting",
		ReceiverStatePairing:      "pairing",
		ReceiverStateBuffering:    "buffering",
		ReceiverStatePlaying:      "playing",
		ReceiverStateInterrupted:  "interrupted",
		ReceiverStateReconnecting: "reconnecting",
		ReceiverStateStopped:      "stopped",
	} {
		if got := (ReceiverState{State: state}).String(); got != want {
			t.Fatalf("state %d: got %q, want %q", state, got, want)
		}
	}
}

// TestControlFramingRoundTrip: the new messages survive the 12-byte framing
// header untouched (guards against accidental overlap with existing types).
func TestControlFramingRoundTrip(t *testing.T) {
	msgs := []Message{
		{Type: MsgQualityMode, Payload: []byte{QualityModeLossless}},
		{Type: MsgSetSource, Payload: AppendSetSource(nil, SetSource{Kind: SetSourceDevice, Name: "TV"})},
		{Type: MsgSetSourceAck, Payload: AppendSetSourceAck(nil, SetSourceAck{OK: 1, Detail: "ok"})},
		{Type: MsgReceiverState, Payload: []byte{ReceiverStateBuffering}},
	}
	for _, m := range msgs {
		var buf bytes.Buffer
		if err := WriteMessage(bufio.NewWriter(&buf), m); err != nil {
			t.Fatalf("write %04x: %v", m.Type, err)
		}
		got, err := ReadMessage(bufio.NewReader(&buf))
		if err != nil {
			t.Fatalf("read %04x: %v", m.Type, err)
		}
		if got.Type != m.Type || !bytes.Equal(got.Payload, m.Payload) {
			t.Fatalf("framed %+v, want %+v", got, m)
		}
	}
}
