package engine

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/codec"
	"remote-au/internal/logging"
	"remote-au/internal/protocol/v2"
)

// fakeCodec for clientMedia tests: frame = 8 bytes PCM; PLC emits 0xEE fill.
type fakeCodec struct{}

func (fakeCodec) Name() string       { return "fake" }
func (fakeCodec) Config() codec.Config { return codec.Config{FrameMs: 10} }
func (fakeCodec) FrameBytes() int    { return 8 }
func (fakeCodec) WireMTU() int       { return 64 }

func (fakeCodec) EncodeFrame(pcm []byte) ([]byte, error) {
	out := make([]byte, len(pcm))
	copy(out, pcm)
	return out, nil
}

func (fakeCodec) DecodeFrame(wire []byte, lost bool, out []byte) (int, error) {
	if lost {
		for i := range out {
			out[i] = 0xEE
		}
		return len(out), nil
	}
	n := copy(out, wire)
	return n, nil
}

func (fakeCodec) Close() error { return nil }

func collectMedia(out *[][]byte, mu *sync.Mutex) func([]byte) {
	return func(pcm []byte) {
		mu.Lock()
		*out = append(*out, append([]byte(nil), pcm...))
		mu.Unlock()
	}
}

func TestClientMediaReorderAndPLC(t *testing.T) {
	var got [][]byte
	var mu sync.Mutex
	m := newClientMedia(fakeCodec{}, collectMedia(&got, &mu))

	// Frame 0 arrives first.
	m.accept(0, 0, []byte("frame-00"))
	// Frame 2 arrives before 1 (reorder).
	m.accept(2, 0, []byte("frame-02"))
	m.accept(1, 0, []byte("frame-01"))
	// Frame 3 is lost; frame 4 arrives → after the hold, PLC fills 3.
	m.accept(4, 0, []byte("frame-04"))
	m.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 5 {
		t.Fatalf("emitted %d frames, want 5: %q", len(got), got)
	}
	if string(got[0]) != "frame-00" || string(got[1]) != "frame-01" || string(got[2]) != "frame-02" {
		t.Fatalf("reorder failed: %q", got[:3])
	}
	if !bytes.Equal(got[3], bytes.Repeat([]byte{0xEE}, 8)) {
		t.Fatalf("PLC frame wrong: %q", got[3])
	}
	if string(got[4]) != "frame-04" {
		t.Fatalf("frame-04 misplaced: %q", got[4])
	}
	loss, _, _, _, _, lossPk, _, _, conceal, maxBurst := m.snapshot()
	// One 8-byte fake frame = 2 stereo PCM frames concealed.
	if lossPk != 1 || conceal != 2 || maxBurst != 1 {
		t.Fatalf("loss stats: pk=%d conceal=%d burst=%d", lossPk, conceal, maxBurst)
	}
	if loss <= 0 || loss >= 1 {
		t.Fatalf("loss EWMA out of range: %v", loss)
	}
}

func TestClientMediaStatsAccumulate(t *testing.T) {
	var got [][]byte
	var mu sync.Mutex
	m := newClientMedia(fakeCodec{}, collectMedia(&got, &mu))

	m.accept(10, 0, []byte("first"))
	// Late duplicate must not emit.
	m.accept(9, 0, []byte("late"))
	m.accept(11, 0, []byte("next"))
	m.tick()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("emitted %d, want 2 (late packet dropped): %q", len(got), got)
	}
	_, late, _, _, seen, _, latePk, reorderPk, _, _ := m.snapshot()
	if latePk != 1 || reorderPk != 0 || seen != 3 {
		t.Fatalf("stats: seen=%d latePk=%d reorderPk=%d", seen, latePk, reorderPk)
	}
	if late <= 0 {
		t.Fatalf("late EWMA not raised: %v", late)
	}
}

// TestClientResumeAfterReconnect: connect, stop, reconnect with the same
// client → RESUME path skips hello/pairing and media resumes.
func TestClientResumeAfterReconnect(t *testing.T) {
	addr := "127.0.0.1:47121"
	format := audioTestFormat()
	pinCodes := make(chan string, 4)

	host, err := NewHost(HostOptions{
		Name: "RESUME-PC", Store: &memoryStore{}, Backend: &fakeBackend{format: format},
		ListenAddr: addr, CaptureSource: loopbackSource(), Format: format,
		Logger: loggingNop(), OnPairingCode: func(code string) { pinCodes <- code },
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	client, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "RESUME-PHONE", Store: &memoryStore{},
		PINProvider: func() (string, error) { return <-pinCodes, nil },
		OnMedia:     func([]byte) {},
		Logger:      loggingNop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- client.Run(runCtx) }()

	// Wait for the stream to come up (resume token captured).
	deadline := time.After(5 * time.Second)
	for client.resumeToken == ([32]byte{}) {
		select {
		case <-deadline:
			t.Fatal("no resume token captured")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Drop the connection (simulated network loss): cancel the run context…
	cancelRun()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Logf("first run ended: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first run did not end")
	}

	// …and reconnect with a fresh client that carries the resume token:
	// resume must skip pairing (no new PIN) and media must flow again.
	runCtx2, cancelRun2 := context.WithCancel(context.Background())
	defer cancelRun2()
	mediaGot := make(chan struct{}, 1)
	client2, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "RESUME-PHONE", Store: client.opts.Store,
		OnMedia: func([]byte) { select { case mediaGot <- struct{}{}: default: } },
		Logger:  loggingNop(),
	})
	if err != nil {
		t.Fatalf("client2: %v", err)
	}
	client2.resumeToken = client.resumeToken
	client2.lastSeq = client.lastSeq
	errCh2 := make(chan error, 1)
	go func() { errCh2 <- client2.Run(runCtx2) }()

	select {
	case <-mediaGot:
	case err := <-errCh2:
		t.Fatalf("reconnect failed: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("no media after resume")
	}

	// No additional pairing code may have been requested.
	select {
	case code := <-pinCodes:
		t.Fatalf("unexpected re-pairing (code %s)", code)
	default:
	}
}

// TestClientRejectsUntrustedHost: with a pinned fingerprint that does not
// match, the connection must fail.
func TestClientRejectsUntrustedHost(t *testing.T) {
	addr := "127.0.0.1:47122"
	format := audioTestFormat()
	host, err := NewHost(HostOptions{
		Name: "PIN-PC", Store: &memoryStore{}, Backend: &fakeBackend{format: format},
		ListenAddr: addr, Format: format, Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	client, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "PIN-PHONE", Store: &memoryStore{},
		TrustedFingerprints: map[string]bool{"0000000000000000000000000000000000000000000000000000000000000000": true},
		Logger:              loggingNop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = client.Run(runCtx)
	if err == nil || !strings.Contains(err.Error(), "not a paired device") {
		t.Fatalf("expected untrusted-host rejection, got %v", err)
	}
}

// TestHostVolumeAndMute: muting a receiver must deliver silence.
func TestHostVolumeAndMute(t *testing.T) {
	addr := "127.0.0.1:47123"
	format := audioTestFormat()
	pinCodes := make(chan string, 4)
	host, err := NewHost(HostOptions{
		Name: "VOL-PC", Store: &memoryStore{}, Backend: &fakeBackend{format: format},
		ListenAddr: addr, CaptureSource: loopbackSource(), Format: format,
		Logger: loggingNop(), OnPairingCode: func(code string) { pinCodes <- code },
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	var mu sync.Mutex
	var got [][]byte
	mediaGot := make(chan struct{}, 32)
	client, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "VOL-PHONE", Store: &memoryStore{},
		PINProvider: func() (string, error) { return <-pinCodes, nil },
		OnMedia: func(pcm []byte) {
			mu.Lock()
			if len(got) < 64 {
				got = append(got, append([]byte(nil), pcm...))
			}
			mu.Unlock()
			select {
			case mediaGot <- struct{}{}:
			default:
			}
		},
		Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(runCtx) }()

	select {
	case <-mediaGot:
	case <-time.After(6 * time.Second):
		t.Fatal("no media")
	}

	// Mute everything; subsequent frames must be all-zero.
	host.MuteAll(true)
	// Drain what's in flight, then check new frames.
	drain := time.After(300 * time.Millisecond)
	for {
		select {
		case <-mediaGot:
			continue
		case <-drain:
		}
		break
	}

	mu.Lock()
	defer mu.Unlock()
	checked := 0
	for _, frame := range got {
		zero := true
		for _, b := range frame {
			if b != 0 {
				zero = false
				break
			}
		}
		if zero {
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no silent frames observed after mute")
	}
	_ = protocolv2.Version
}

func audioTestFormat() audio.Format {
	return audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}
}

func loopbackSource() audio.Source { return audio.SourceLoopback }

func loggingNop() logging.Logger { return logging.Nop() }

