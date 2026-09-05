package mobile

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/engine"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
)

// memStore is an in-memory pairing.Store for tests.
type memStore struct {
	mu    sync.Mutex
	id    *pairing.Identity
	set   bool
	peers []pairing.PeerRecord
}

func (m *memStore) LoadIdentity() (*pairing.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set {
		return m.id, nil
	}
	return nil, nil
}

func (m *memStore) SaveIdentity(id *pairing.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id = id
	m.set = true
	return nil
}

func (m *memStore) Peers() ([]pairing.PeerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pairing.PeerRecord, len(m.peers))
	copy(out, m.peers)
	return out, nil
}

func (m *memStore) SavePeer(rec pairing.PeerRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.peers {
		if m.peers[i].ID == rec.ID {
			m.peers[i] = rec
			return nil
		}
	}
	m.peers = append(m.peers, rec)
	return nil
}

func (m *memStore) RemovePeer(id [16]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.peers[:0]
	for _, p := range m.peers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	m.peers = kept
	return nil
}

// asyncPIN adapts a channel to RequestSource.
type asyncPIN struct{ get func() string }

func (a asyncPIN) GetPIN() string { return a.get() }

// pinSource is a test double for RequestSource.
type pinSource struct{ pin string }

func (p pinSource) GetPIN() string { return p.pin }

// sink collects media.
type sink struct {
	ch chan []byte
}

func (s *sink) OnMedia(pcm []byte) {
	buf := make([]byte, len(pcm))
	copy(buf, pcm)
	select {
	case s.ch <- buf:
	default:
	}
}

type testStateSink struct{ ch chan string }

func (s *testStateSink) OnState(j string) {
	select {
	case s.ch <- j:
	default:
	}
}

func TestMobileSetupAndStore(t *testing.T) {
	dir := t.TempDir()
	if err := Setup(dir); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if PeerList() != "[]" {
		t.Fatalf("peer list: %s", PeerList())
	}
	// Setup again on the same dir must succeed and reuse the identity.
	if err := Setup(dir); err != nil {
		t.Fatalf("setup 2: %v", err)
	}
}

func TestMobileConnectFlow(t *testing.T) {
	// Start a v2 host (fake capture) as in engine_test.
	addr := "127.0.0.1:47201"
	format := audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}
	pinCh := make(chan string, 1)

	host, err := engine.NewHost(engine.HostOptions{
		Name: "MOBILE-PC", Store: &memStore{}, Backend: newFakeBackend(format),
		ListenAddr: addr, Format: format, Logger: logging.Nop(),
		OnPairingCode: func(code string) { pinCh <- code },
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	// Mobile side.
	dir := t.TempDir()
	if err := Setup(dir); err != nil {
		t.Fatalf("setup: %v", err)
	}

	media := &sink{ch: make(chan []byte, 64)}
	SetMediaSink(media)
	st := &testStateSink{ch: make(chan string, 64)}
	SetStateSink(st)

	pins := asyncPIN{get: func() string { return <-pinCh }}
	if err := Connect(addr, "TestPhone", pins); err != nil {
		t.Fatalf("connect: %v", err)
	}

	select {
	case <-media.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no media received")
	}

	if !IsRunning() {
		t.Fatal("not running after connect")
	}
	Stop()
	time.Sleep(200 * time.Millisecond)
	if IsRunning() {
		t.Fatal("still running after stop")
	}
}

// ---------------------------------------------------------------------------
// helpers shared with the fake host

type fakeBackend struct{ format audio.Format }

func newFakeBackend(format audio.Format) *fakeBackend { return &fakeBackend{format: format} }

func (b *fakeBackend) Name() string           { return "fake" }
func (b *fakeBackend) SupportsLoopback() bool { return true }
func (b *fakeBackend) EnumerateDevices() (audio.DeviceLists, error) {
	return audio.DeviceLists{}, nil
}
func (b *fakeBackend) OpenCapture(opts audio.CaptureOptions) (audio.Capture, error) {
	buf := make([]byte, 9600)
	for i := 0; i+1 < len(buf); i += 2 {
		binary.LittleEndian.PutUint16(buf[i:], uint16(i%32767))
	}
	return &fakeCapture{format: opts.Format, buf: buf}, nil
}
func (b *fakeBackend) OpenPlayback(audio.PlaybackOptions) (audio.Playback, error) {
	return nil, audio.ErrNoBackend
}

type fakeCapture struct {
	format audio.Format
	buf    []byte
}

func (f *fakeCapture) Format() audio.Format { return f.format }

func (f *fakeCapture) Read(dst []byte) int {
	n := min(len(dst), len(f.buf))
	copy(dst, f.buf[:n])
	for i := 0; i+1 < len(f.buf); i += 2 {
		v := int16(binary.LittleEndian.Uint16(f.buf[i:])) + 7
		binary.LittleEndian.PutUint16(f.buf[i:], uint16(v))
	}
	return n
}

func (f *fakeCapture) Start() error { return nil }
func (f *fakeCapture) Close() error { return nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

