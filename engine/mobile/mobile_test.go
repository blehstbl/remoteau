package mobile

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

type testStatsSink struct{ ch chan string }

func (s *testStatsSink) OnStats(j string) {
	select {
	case s.ch <- j:
	default:
	}
}

func TestSetSourceAndQualityModeGuards(t *testing.T) {
	// Runs before any Setup in this package: the store is nil and there is
	// no client, so the control calls must fail fast rather than panic.
	Stop()
	if err := SetSource(1, "Speakers"); err == nil {
		t.Fatal("SetSource succeeded before Setup / while disconnected")
	}
	if err := SetQualityMode(2); err == nil {
		t.Fatal("SetQualityMode succeeded before Setup / while disconnected")
	}
}

func TestConnectViaRelayGuards(t *testing.T) {
	// Empty relay arguments are rejected before any Setup / guard state.
	if err := ConnectViaRelay("", "", "iPhone", 0, 0, 0, 0, 0, false, false, 0, 0, nil); err == nil {
		t.Fatal("ConnectViaRelay accepted empty relay addr and host device id")
	}
	if err := ConnectViaRelay("127.0.0.1:1", "", "iPhone", 0, 0, 0, 0, 0, false, false, 0, 0, nil); err == nil {
		t.Fatal("ConnectViaRelay accepted empty host device id")
	}
	if err := ConnectViaRelay("", "00112233445566778899aabbccddeeff", "iPhone", 0, 0, 0, 0, 0, false, false, 0, 0, nil); err == nil {
		t.Fatal("ConnectViaRelay accepted empty relay addr")
	}

	// Valid relay arguments but no Setup must return the same "call Setup
	// first" error as ConnectWithCaps (reset the shared globals so the test
	// does not depend on package test ordering).
	Stop()
	mu.Lock()
	prevStore, prevIdentity, prevRunning := store, identity, running
	store, identity, running = nil, nil, false
	mu.Unlock()
	defer func() {
		mu.Lock()
		store, identity, running = prevStore, prevIdentity, prevRunning
		mu.Unlock()
	}()

	err := ConnectViaRelay("127.0.0.1:1", "00112233445566778899aabbccddeeff", "iPhone",
		0, 0, 0, 0, 0, false, false, 0, 0, nil)
	if err == nil || err.Error() != "mobile: call Setup first" {
		t.Fatalf("ConnectViaRelay before Setup: got %v, want call Setup first", err)
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

func TestMobileStatsFlow(t *testing.T) {
	// Start a v2 host (fake capture) on its own port.
	addr := "127.0.0.1:47202"
	format := audio.Format{Rate: 48000, Channels: 2, FrameSamples: 480}
	pinCh := make(chan string, 1)

	host, err := engine.NewHost(engine.HostOptions{
		Name: "MOBILE-PC-STATS", Store: &memStore{}, Backend: newFakeBackend(format),
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

	dir := t.TempDir()
	if err := Setup(dir); err != nil {
		t.Fatalf("setup: %v", err)
	}

	media := &sink{ch: make(chan []byte, 64)}
	SetMediaSink(media)
	SetStateSink(&testStateSink{ch: make(chan string, 64)})
	stats := &testStatsSink{ch: make(chan string, 64)}
	SetStatsSink(stats)

	pins := asyncPIN{get: func() string { return <-pinCh }}
	if err := Connect(addr, "TestPhone", pins); err != nil {
		t.Fatalf("connect: %v", err)
	}

	select {
	case <-media.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no media received")
	}

	// Let the ~1 Hz mobile stats ticker publish at least once.
	time.Sleep(1500 * time.Millisecond)

	raw := LatestStats()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("latest stats not JSON: %v (%s)", err, raw)
	}
	if connected, _ := m["connected"].(bool); !connected {
		t.Fatalf("latest stats connected != true: %s", raw)
	}
	if _, ok := m["jitter_ms"].(float64); !ok {
		t.Fatalf("latest stats jitter_ms missing/not numeric: %s", raw)
	}

	select {
	case j := <-stats.ch:
		var sm map[string]any
		if err := json.Unmarshal([]byte(j), &sm); err != nil {
			t.Fatalf("stats sink not JSON: %v (%s)", err, j)
		}
		if _, ok := sm["state"]; !ok {
			t.Fatalf("stats sink payload missing state: %s", j)
		}
	default:
		t.Fatal("no stats published to StatsSink")
	}

	Stop()
	time.Sleep(200 * time.Millisecond)
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

// ---------------------------------------------------------------------------
// keychain-backed trust store

// fakeSource is an in-memory StoreSource test double.
type fakeSource struct {
	mu       sync.Mutex
	identity []byte
	peers    []byte
	putIDs   int
	putPeers int
}

func (f *fakeSource) GetIdentity() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.identity)
}

func (f *fakeSource) GetPeers() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return bytes.Clone(f.peers)
}

func (f *fakeSource) PutIdentity(pkcs8 []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.identity = bytes.Clone(pkcs8)
	f.putIDs++
	return nil
}

func (f *fakeSource) PutPeers(json []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = bytes.Clone(json)
	f.putPeers++
	return nil
}

func (f *fakeSource) identityWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putIDs
}

func TestSetupWithKeychainCreatesIdentity(t *testing.T) {
	src := &fakeSource{}
	if err := SetupWithKeychain(src); err != nil {
		t.Fatalf("setup keychain: %v", err)
	}
	if len(src.GetIdentity()) == 0 {
		t.Fatal("identity not written to source")
	}
	if src.identityWrites() != 1 {
		t.Fatalf("PutIdentity calls: %d", src.identityWrites())
	}
	first := src.GetIdentity()

	// Re-setup must reuse the stored identity (no second write).
	if err := SetupWithKeychain(src); err != nil {
		t.Fatalf("setup keychain 2: %v", err)
	}
	if src.identityWrites() != 1 {
		t.Fatalf("PutIdentity calls after re-setup: %d", src.identityWrites())
	}
	if !bytes.Equal(src.GetIdentity(), first) {
		t.Fatal("identity changed across re-setup")
	}
}

func TestKeychainStorePeerWriteThrough(t *testing.T) {
	src := &fakeSource{}
	if err := SetupWithKeychain(src); err != nil {
		t.Fatalf("setup keychain: %v", err)
	}

	rec := pairing.PeerRecord{
		ID:            [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		Name:          "Office-PC",
		PairingSecret: bytes.Repeat([]byte{0xAB}, 32),
		Fingerprint:   "abc123",
		PairedAt:      1234567890,
	}
	mu.Lock()
	st := store
	mu.Unlock()
	if st == nil {
		t.Fatal("store not set by SetupWithKeychain")
	}
	if err := st.SavePeer(rec); err != nil {
		t.Fatalf("save peer: %v", err)
	}

	// Peers blob written through to the source, JSON contains the peer.
	var got []pairing.PeerRecord
	if err := json.Unmarshal(src.GetPeers(), &got); err != nil {
		t.Fatalf("peers blob: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("peers blob has %d peers", len(got))
	}
	if got[0].Name != "Office-PC" || got[0].Fingerprint != "abc123" || got[0].PairedAt != rec.PairedAt {
		t.Fatalf("peers blob fields: %+v", got[0])
	}
	if got[0].ID != rec.ID {
		t.Fatal("peer id mismatch in blob")
	}
	if !bytes.Equal(got[0].PairingSecret, rec.PairingSecret) {
		t.Fatal("pairing secret mismatch in blob")
	}
	if PeerList() == "[]" {
		t.Fatal("peer list empty after save")
	}

	// ForgetPeer flows through the store; blob loses the peer (and secret).
	if err := ForgetPeer(hex.EncodeToString(rec.ID[:])); err != nil {
		t.Fatalf("forget peer: %v", err)
	}
	var after []pairing.PeerRecord
	if err := json.Unmarshal(src.GetPeers(), &after); err != nil {
		t.Fatalf("peers blob after remove: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("peers blob after remove: %+v", after)
	}
	if PeerList() != "[]" {
		t.Fatalf("peer list after forget: %s", PeerList())
	}
}

func TestSetupWithKeychainAndMigrate(t *testing.T) {
	dir := t.TempDir()
	legacyID, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	pkcs8, err := legacyID.MarshalPrivate()
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	legacyPeers := []pairing.PeerRecord{{
		ID:            [16]byte{0xAA, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		Name:          "Legacy-PC",
		PairingSecret: bytes.Repeat([]byte{0x5A}, 32),
		Fingerprint:   "cafe",
		PairedAt:      42,
	}}
	legacyBlob, err := json.Marshal(fileState{IdentityPKCS8: pkcs8, Peers: legacyPeers})
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	legacyPath := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(legacyPath, legacyBlob, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	src := &fakeSource{}
	if err := SetupWithKeychainAndMigrate(src, dir); err != nil {
		t.Fatalf("setup+migrate: %v", err)
	}
	// Identity was migrated, not regenerated.
	if !bytes.Equal(src.GetIdentity(), pkcs8) {
		t.Fatal("identity not migrated from legacy file")
	}
	var got []pairing.PeerRecord
	if err := json.Unmarshal(src.GetPeers(), &got); err != nil {
		t.Fatalf("peers blob: %v", err)
	}
	if len(got) != 1 || got[0].Name != "Legacy-PC" {
		t.Fatalf("peers blob: %+v", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy file not deleted: %v", err)
	}

	// No legacy file: plain keychain setup path, no error.
	if err := SetupWithKeychainAndMigrate(&fakeSource{}, t.TempDir()); err != nil {
		t.Fatalf("setup+migrate empty dir: %v", err)
	}

	// Migration is skipped when the source already holds an identity; the
	// legacy file is left alone and the source is not clobbered.
	keepID, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("identity 2: %v", err)
	}
	keepPKCS8, err := keepID.MarshalPrivate()
	if err != nil {
		t.Fatalf("marshal identity 2: %v", err)
	}
	if err := os.WriteFile(legacyPath, legacyBlob, 0o600); err != nil {
		t.Fatalf("rewrite legacy: %v", err)
	}
	src2 := &fakeSource{identity: keepPKCS8}
	if err := SetupWithKeychainAndMigrate(src2, dir); err != nil {
		t.Fatalf("setup+migrate 2: %v", err)
	}
	if !bytes.Equal(src2.GetIdentity(), keepPKCS8) {
		t.Fatal("source identity clobbered by migration")
	}
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		t.Fatal("legacy file deleted even though source was not empty")
	}
}
