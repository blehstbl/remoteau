package engine

import (
	"bytes"
	"errors"
	"sync"
	"testing"
)

var errFakeFEC = errors.New("fec decode failed")

// fecCodec is fakeCodec plus the optional codec.FECDecoder capability:
// FEC recovery emits a distinctive 0xDD fill so tests can tell it apart
// from the 0xEE PLC fill. When failFEC is set, DecodeFEC fails so the
// receiver must fall back to PLC.
type fecCodec struct {
	fakeCodec
	failFEC bool
}

func (c fecCodec) DecodeFEC(wire []byte, out []byte) (int, error) {
	if c.failFEC {
		return 0, errFakeFEC
	}
	for i := range out {
		out[i] = 0xDD
	}
	return len(out), nil
}

func TestClientMediaFECRecovery(t *testing.T) {
	var got [][]byte
	var mu sync.Mutex
	m := newClientMedia(fecCodec{}, collectMedia(&got, &mu))

	// Frame 0 arrives; frame 1 is lost; frame 2 arrives carrying FEC data
	// for 1 → after the hold, FEC reconstructs 1, then 2 emits normally.
	m.accept(0, 0, []byte("frame-00"))
	m.accept(2, 0, []byte("frame-02"))
	m.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("emitted %d frames, want 3: %q", len(got), got)
	}
	if string(got[0]) != "frame-00" {
		t.Fatalf("frame-00 misplaced: %q", got[0])
	}
	if !bytes.Equal(got[1], bytes.Repeat([]byte{0xDD}, 8)) {
		t.Fatalf("FEC frame wrong (want 0xDD fill): %q", got[1])
	}
	if string(got[2]) != "frame-02" {
		t.Fatalf("frame-02 misplaced: %q", got[2])
	}
	if m.FECRecovered() != 1 {
		t.Fatalf("FECRecovered = %d, want 1", m.FECRecovered())
	}
	loss, _, _, _, _, lossPk, _, _, conceal, maxBurst := m.snapshot()
	if lossPk != 1 || maxBurst != 1 {
		t.Fatalf("loss stats: pk=%d burst=%d", lossPk, maxBurst)
	}
	// FEC output is not PLC concealment.
	if conceal != 0 {
		t.Fatalf("concealed = %d, want 0 (FEC path)", conceal)
	}
	if loss <= 0 || loss >= 1 {
		t.Fatalf("loss EWMA out of range: %v", loss)
	}
}

func TestClientMediaFECFailureFallsBackToPLC(t *testing.T) {
	var got [][]byte
	var mu sync.Mutex
	m := newClientMedia(fecCodec{failFEC: true}, collectMedia(&got, &mu))

	m.accept(0, 0, []byte("frame-00"))
	m.accept(2, 0, []byte("frame-02"))
	m.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("emitted %d frames, want 3: %q", len(got), got)
	}
	if !bytes.Equal(got[1], bytes.Repeat([]byte{0xEE}, 8)) {
		t.Fatalf("PLC fallback wrong (want 0xEE fill): %q", got[1])
	}
	if m.FECRecovered() != 0 {
		t.Fatalf("FECRecovered = %d, want 0", m.FECRecovered())
	}
	_, _, _, _, _, lossPk, _, _, conceal, _ := m.snapshot()
	if lossPk != 1 || conceal != 2 {
		t.Fatalf("PLC stats: pk=%d conceal=%d", lossPk, conceal)
	}
}

func TestClientMediaFECHasNoNextFrame(t *testing.T) {
	var got [][]byte
	var mu sync.Mutex
	m := newClientMedia(fecCodec{}, collectMedia(&got, &mu))

	// Frames 1 and 2 are lost; frame 3 arrives. The gap at 1 has no next
	// frame held (3 != 2) → PLC; the gap at 2 has 3 = 2+1 → FEC.
	m.accept(0, 0, []byte("frame-00"))
	m.accept(3, 0, []byte("frame-03"))
	m.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("emitted %d frames, want 4: %q", len(got), got)
	}
	if !bytes.Equal(got[1], bytes.Repeat([]byte{0xEE}, 8)) {
		t.Fatalf("gap-1 should be PLC fill: %q", got[1])
	}
	if !bytes.Equal(got[2], bytes.Repeat([]byte{0xDD}, 8)) {
		t.Fatalf("gap-2 should be FEC fill: %q", got[2])
	}
	if string(got[3]) != "frame-03" {
		t.Fatalf("frame-03 misplaced: %q", got[3])
	}
	if m.FECRecovered() != 1 {
		t.Fatalf("FECRecovered = %d, want 1", m.FECRecovered())
	}
	_, _, _, _, _, lossPk, _, _, _, maxBurst := m.snapshot()
	if lossPk != 2 || maxBurst != 2 {
		t.Fatalf("loss stats: pk=%d burst=%d", lossPk, maxBurst)
	}
}
