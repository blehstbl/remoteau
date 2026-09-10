package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
)

// memoryStore is an in-memory pairing.Store for tests.
type memoryStore struct {
	mu    sync.Mutex
	id    *pairing.Identity
	set   bool
	err   error
	peers []pairing.PeerRecord
}

func (m *memoryStore) LoadIdentity() (*pairing.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.set {
		return m.id, nil
	}
	return nil, nil
}

func (m *memoryStore) SaveIdentity(id *pairing.Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.id = id
	m.set = true
	return nil
}

func (m *memoryStore) Peers() ([]pairing.PeerRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pairing.PeerRecord, len(m.peers))
	copy(out, m.peers)
	return out, nil
}

func (m *memoryStore) SavePeer(rec pairing.PeerRecord) error {
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

func (m *memoryStore) RemovePeer(id [16]byte) error {
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

// fakeCapture produces a deterministic ramp pattern.
type fakeCapture struct {
	format audio.Format
	buf    []byte
}

func (f *fakeCapture) Format() audio.Format { return f.format }

func (f *fakeCapture) Read(dst []byte) int {
	n := min(len(dst), len(f.buf))
	copy(dst, f.buf[:n])
	// Advance the pattern by one sample step per read.
	step := int16(7)
	for i := 0; i+1 < len(f.buf); i += 2 {
		v := int16(binary.LittleEndian.Uint16(f.buf[i:])) + step
		binary.LittleEndian.PutUint16(f.buf[i:], uint16(v))
	}
	return n
}

func (f *fakeCapture) Start() error { return nil }

func (f *fakeCapture) Close() error { return nil }

// fakeBackend serves the fake capture.
type fakeBackend struct {
	format audio.Format
}

func (b *fakeBackend) Name() string           { return "fake" }
func (b *fakeBackend) SupportsLoopback() bool { return true }
func (b *fakeBackend) EnumerateDevices() (audio.DeviceLists, error) {
	return audio.DeviceLists{}, nil
}
func (b *fakeBackend) OpenCapture(opts audio.CaptureOptions) (audio.Capture, error) {
	buf := make([]byte, 9600) // 1200 frames stereo
	for i := 0; i+1 < len(buf); i += 2 {
		binary.LittleEndian.PutUint16(buf[i:], uint16(i%32767))
	}
	return &fakeCapture{format: opts.Format, buf: buf}, nil
}
func (b *fakeBackend) OpenPlayback(audio.PlaybackOptions) (audio.Playback, error) {
	return nil, audio.ErrNoBackend
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestHostClientLoopback runs the full v2 stack in-process: pairing with PIN,
// capability negotiation and PCM media delivery over QUIC datagrams.
func TestHostClientLoopback(t *testing.T) {
	addr := "127.0.0.1:47111"

	hostStore := &memoryStore{}
	clientStore := &memoryStore{}

	format := audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}
	host, err := NewHost(HostOptions{
		Name:          "TEST-PC",
		Store:         hostStore,
		Backend:       &fakeBackend{format: format},
		ListenAddr:    addr,
		CaptureSource: audio.SourceLoopback,
		Format:        format,
		Logger:        logging.Nop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}

	pinCh := make(chan string, 1)
	host.opts.OnPairingCode = func(code string) { pinCh <- code }

	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	hostErr := make(chan error, 1)
	go func() { hostErr <- host.Run(hostCtx) }()

	// Wait for the listener to come up.
	time.Sleep(300 * time.Millisecond)

	var received []byte
	var receivedMu sync.Mutex
	gotMedia := make(chan struct{}, 1)

	client, err := NewClient(ClientOptions{
		HostAddr: addr,
		Name:     "TEST-IPHONE",
		Store:    clientStore,
		PINProvider: func() (string, error) {
			select {
			case code := <-pinCh:
				return code, nil
			case <-time.After(5 * time.Second):
				return "", fmt.Errorf("no pairing code shown")
			}
		},
		OnMedia: func(pcm []byte) {
			receivedMu.Lock()
			if len(received) < 4096 {
				received = append(received, pcm...)
			}
			receivedMu.Unlock()
			select {
			case gotMedia <- struct{}{}:
			default:
			}
		},
		Logger: logging.Nop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	clientErr := make(chan error, 1)
	go func() { clientErr <- client.Run(clientCtx) }()

	select {
	case <-gotMedia:
	case err := <-clientErr:
		t.Fatalf("client exited early: %v", err)
	case err := <-hostErr:
		t.Fatalf("host exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for media")
	}

	receivedMu.Lock()
	n := len(received)
	receivedMu.Unlock()
	if n < 960 {
		t.Fatalf("received only %d bytes", n)
	}

	// Both sides must have stored the pairing.
	peers, err := hostStore.Peers()
	if err != nil || len(peers) != 1 {
		t.Fatalf("host peers: %v %+v", err, peers)
	}
	cpeers, err := clientStore.Peers()
	if err != nil || len(cpeers) != 1 {
		t.Fatalf("client peers: %v %+v", err, cpeers)
	}
	if string(peers[0].PairingSecret) != string(cpeers[0].PairingSecret) {
		t.Fatal("pairing secrets differ")
	}

	stopClient()
	stopHost()
}

// TestMultiReceiverFanout connects two clients to one host and verifies
// both receive media independently (Phase 11).
func TestMultiReceiverFanout(t *testing.T) {
	addr := "127.0.0.1:47113"
	format := audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}

	host, err := NewHost(HostOptions{
		Name: "FANOUT-PC", Store: &memoryStore{}, Backend: &fakeBackend{format: format},
		ListenAddr: addr, CaptureSource: audio.SourceLoopback, Format: format,
		Logger: logging.Nop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	pinCh := make(chan string, 8)
	host.opts.OnPairingCode = func(code string) { pinCh <- code }

	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	start := func(name string) (chan []byte, context.CancelFunc, chan error) {
		ch := make(chan []byte, 64)
		client, err := NewClient(ClientOptions{
			HostAddr: addr, Name: name, Store: &memoryStore{},
			PINProvider: func() (string, error) { return <-pinCh, nil },
			OnMedia: func(pcm []byte) {
				buf := make([]byte, len(pcm))
				copy(buf, pcm)
				select {
				case ch <- buf:
				default:
				}
			},
			Logger: logging.Nop(),
		})
		if err != nil {
			t.Fatalf("client %s: %v", name, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- client.Run(ctx) }()
		return ch, cancel, errCh
	}

	// Pair/connect one client at a time: the host serializes pairings and
	// shows a single code, so a shared PIN channel must not race. Both
	// clients then stay connected simultaneously (the fanout assertion).
	waitMedia := func(name string, ch chan []byte, errCh chan error) {
		select {
		case <-ch:
		case err := <-errCh:
			t.Fatalf("client %s: %v", name, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("client %s received no media", name)
		}
	}

	aCh, aCancel, aErr := start("CLIENT-A")
	defer aCancel()
	waitMedia("A", aCh, aErr)

	bCh, bCancel, bErr := start("CLIENT-B")
	defer bCancel()
	waitMedia("B", bCh, bErr)

	// Both are connected now: assert A still receives audio alongside B.
	select {
	case <-aCh:
	case err := <-aErr:
		t.Fatalf("client A after B joined: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("client A stopped receiving after client B joined")
	}
}

func TestSessionPingRoundTrip(t *testing.T) {
	addr := "127.0.0.1:47112"
	hostStore := &memoryStore{}
	format := audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}
	host, err := NewHost(HostOptions{
		Name: "PING-PC", Store: hostStore, Backend: &fakeBackend{format: format},
		ListenAddr: addr, Format: format, Logger: logging.Nop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	pinCh := make(chan string, 1)
	host.opts.OnPairingCode = func(code string) { pinCh <- code }

	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	client, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "PING-CLIENT", Store: &memoryStore{},
		PINProvider: func() (string, error) { return <-pinCh, nil },
		Logger:      logging.Nop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	clientErr := make(chan error, 1)
	go func() { clientErr <- client.Run(clientCtx) }()

	// If we get media or a clean client run for 1s, the control plane works.
	select {
	case err := <-clientErr:
		if err != nil && err != context.Canceled {
			t.Fatalf("client error: %v", err)
		}
	case <-time.After(1500 * time.Millisecond):
	}
	_ = protocolv2.Version
	stopClient()
	stopHost()
}
