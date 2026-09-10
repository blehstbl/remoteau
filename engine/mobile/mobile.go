// Package mobile exposes the RemoteAU v2 client engine to iOS via
// gomobile bind. The Swift app drives it through the small API below:
//
//	Setup(dataDir)             — load/create identity + trust store
//	SetupWithKeychain(source)  — same, backed by host secure storage
//	Connect(host, name, pin)   — connect+pair+stream (async, non-blocking)
//	Stop()                     — tear down
//	LatestStats()              — JSON stats snapshot for the UI
//	SetMediaSink(sink)         — receives decoded PCM (S16LE)
//
// Real-time rule: OnMedia delivers on a Go goroutine; the Swift side must
// only copy into its ring buffer there (no locks it cannot afford).
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"remote-au/internal/engine"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
)

// MediaSink receives decoded PCM frames (S16LE interleaved).
type MediaSink interface {
	OnMedia(pcm []byte)
}

// StateSink receives JSON state/stat snapshots (~1 Hz).
type StateSink interface {
	OnState(jsonState string)
}

// StatsSink receives JSON measurement snapshots (~1 Hz) while connected.
type StatsSink interface {
	OnStats(jsonStats string)
}

// RequestSource is asked (on a Go goroutine) for the pairing PIN the user
// read from the PC screen. Returning an empty string cancels pairing.
type RequestSource interface {
	// GetPIN blocks until the user enters the code (or "" to cancel).
	GetPIN() string
}

var (
	mu sync.Mutex

	dataDir  string
	identity *pairing.Identity
	store    pairing.Store

	client  *engine.Client
	cancel  context.CancelFunc
	running bool
	state   = "idle"
	lastErr string

	mediaSink MediaSink
	stateSink StateSink
	statsSink StatsSink
	logLevel  = "info"
)

// Setup initializes the engine with the app's data directory.
func Setup(dir string) error {
	mu.Lock()
	if dir == "" {
		mu.Unlock()
		return errors.New("mobile: data dir required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		mu.Unlock()
		return fmt.Errorf("mobile: mkdir: %w", err)
	}
	dataDir = dir
	st, err := newFileStore(dir)
	if err != nil {
		mu.Unlock()
		return err
	}
	store = st

	id, err := st.LoadIdentity()
	if err != nil {
		mu.Unlock()
		return err
	}
	if id == nil {
		id, err = pairing.NewIdentity()
		if err != nil {
			mu.Unlock()
			return err
		}
		if err := st.SaveIdentity(id); err != nil {
			mu.Unlock()
			return err
		}
	}
	identity = id
	mu.Unlock()
	setState("ready")
	return nil
}

// SetupWithKeychain initializes the engine using the host-provided secure
// trust store (Keychain on iOS) instead of the file-based store. Same
// semantics as Setup: it loads the stored identity or creates a fresh one and
// leaves the engine ready. Safe to call more than once (re-setup reuses the
// stored identity; nothing is duplicated).
func SetupWithKeychain(source StoreSource) error {
	return setupKeychainStore(source, "")
}

// SetupWithKeychainAndMigrate is SetupWithKeychain with a one-time migration
// from the legacy file-based store: if the source is still empty and
// legacyDir/trust.json exists, its contents are copied into the source and
// the legacy file is then deleted (best effort). Nothing sensitive is logged.
func SetupWithKeychainAndMigrate(source StoreSource, legacyDir string) error {
	return setupKeychainStore(source, legacyDir)
}

func setupKeychainStore(source StoreSource, legacyDir string) error {
	if source == nil {
		return errors.New("mobile: store source required")
	}
	mu.Lock()
	if legacyDir != "" {
		if err := migrateLegacyStore(source, legacyDir); err != nil {
			mu.Unlock()
			return err
		}
	}
	st, err := newKeychainStore(source)
	if err != nil {
		mu.Unlock()
		return err
	}
	id, err := st.LoadIdentity()
	if err != nil {
		mu.Unlock()
		return err
	}
	if id == nil {
		id, err = pairing.NewIdentity()
		if err != nil {
			mu.Unlock()
			return err
		}
		if err := st.SaveIdentity(id); err != nil {
			mu.Unlock()
			return err
		}
	}
	identity = id
	store = st
	mu.Unlock()
	setState("ready")
	return nil
}

// SetMediaSink registers the media sink.
func SetMediaSink(s MediaSink) {
	mu.Lock()
	mediaSink = s
	mu.Unlock()
}

// SetStateSink registers the state/stats sink.
func SetStateSink(s StateSink) {
	mu.Lock()
	stateSink = s
	mu.Unlock()
}

// SetStatsSink registers the live measurement sink (JSON, ~1 Hz).
func SetStatsSink(s StatsSink) {
	mu.Lock()
	statsSink = s
	mu.Unlock()
}

// SetSource asks the host to switch its capture source. kind: 0=system
// default, 1=named render device, 2=test tone, 3=per-app (name "p:1,2" or
// "x:9"). Values outside 0..3 are clamped.
func SetSource(kind int, name string) error {
	mu.Lock()
	c := client
	run := running
	mu.Unlock()
	if !run || c == nil {
		return errors.New("mobile: not connected")
	}
	return c.SetSource(uint8(clampInt(kind, 0, 3)), name)
}

// SetQualityMode asks the host to apply a quality preset. mode: 0=auto,
// 1=lowest, 2=lossless, 3=robust, 4=advanced. Values outside 0..4 are
// clamped.
func SetQualityMode(mode int) error {
	mu.Lock()
	c := client
	run := running
	mu.Unlock()
	if !run || c == nil {
		return errors.New("mobile: not connected")
	}
	return c.SetQualityMode(uint8(clampInt(mode, 0, 4)))
}

// SetLogLevel sets the engine log verbosity ("debug", "info", "warn").
func SetLogLevel(level string) {
	mu.Lock()
	logLevel = level
	mu.Unlock()
}

// Connect starts the client against a v2 host. Non-blocking: returns after
// spawning the engine goroutine. PinSource is consulted when pairing is
// required.
func Connect(hostAddr, deviceName string, pins RequestSource) error {
	return ConnectWithCaps(hostAddr, deviceName, 0, 0, 0, 0, 0, false, false, 0, 0, pins)
}

// ConnectWithCaps is Connect with explicit codec preferences (0 = default;
// codec: 0=PCM 1=Opus). The reconnect loop is always enabled.
func ConnectWithCaps(hostAddr, deviceName string, codecID, rate, channels, frameMs, bitrate int,
	fec, dtx bool, complexity, appID int, pins RequestSource) error {
	return connectClient(hostAddr, "", "", deviceName, codecID, rate, channels, frameMs,
		bitrate, fec, dtx, complexity, appID, pins)
}

// ConnectViaRelay starts the client through a relay splice instead of a direct
// LAN connection. The host is identified by hostDeviceID (hex, 32 chars) and
// all payloads are sealed with the pairing secret from a prior LAN pairing.
// Semantics (reconnect loop, guards, stats) match ConnectWithCaps.
func ConnectViaRelay(relayAddr, hostDeviceID, deviceName string, codecID, rate, channels, frameMs, bitrate int,
	fec, dtx bool, complexity, appID int, pins RequestSource) error {
	if relayAddr == "" || hostDeviceID == "" {
		return errors.New("mobile: relay address and host device id required")
	}
	return connectClient("", relayAddr, hostDeviceID, deviceName, codecID, rate, channels, frameMs,
		bitrate, fec, dtx, complexity, appID, pins)
}

// connectClient is the shared connect path behind ConnectWithCaps (direct) and
// ConnectViaRelay. Exactly one of hostAddr or relayAddr is non-empty; callers
// validate their mode-specific arguments beforehand.
func connectClient(hostAddr, relayAddr, hostDeviceID, deviceName string,
	codecID, rate, channels, frameMs, bitrate int,
	fec, dtx bool, complexity, appID int, pins RequestSource) error {
	mu.Lock()
	if running {
		mu.Unlock()
		return errors.New("mobile: already connected")
	}
	if store == nil || identity == nil {
		mu.Unlock()
		return errors.New("mobile: call Setup first")
	}
	if relayAddr == "" && hostAddr == "" {
		mu.Unlock()
		return errors.New("mobile: host address required")
	}
	if deviceName == "" {
		deviceName = "iPhone"
	}
	running = true
	storeRef := store
	mu.Unlock()

	log := newLogger()
	setState("connecting")
	ctx, ctxCancel := context.WithCancel(context.Background())
	cancel = ctxCancel
	statsDone := make(chan struct{})
	go statsLoop(ctx, statsDone)

	requested := protocolv2.Caps{
		Codec:       uint8(clampInt(codecID, 0, 1)),
		SampleRate:  uint32(clampInt(rate, 0, 192000)),
		Channels:    uint8(clampInt(channels, 0, 8)),
		FrameMs:     uint8(clampInt(frameMs, 0, 20)),
		OpusBitrate: uint32(clampInt(bitrate, 0, 510000)),
		FEC:         boolBit(fec),
		DTX:         boolBit(dtx),
		Complexity:  uint8(clampInt(complexity, 0, 10)),
		AppID:       uint8(clampInt(appID, 0, 2)),
	}
	if requested.SampleRate == 0 {
		requested = protocolv2.Caps{
			Codec:      protocolv2.CodecPCMS16LE,
			SampleRate: 48000,
			Channels:   2,
			FrameMs:    5,
		}
	}

	// Pin the host certificate after pairing (Phase 5): stored peers carry
	// the host's fingerprint; only that certificate is accepted.
	trusted := map[string]bool{}
	if peers, perr := storeRef.Peers(); perr == nil {
		for _, p := range peers {
			if p.Fingerprint != "" {
				trusted[p.Fingerprint] = true
			}
		}
	}

	go func() {
		defer close(statsDone)
		defer func() {
			mu.Lock()
			running = false
			mu.Unlock()
			setState("idle")
		}()

		c, err := engine.NewClient(engine.ClientOptions{
			HostAddr:     hostAddr,
			RelayAddr:    relayAddr,
			HostDeviceID: hostDeviceID,
			Name:         deviceName,
			Store:        storeRef,
			PINProvider: func() (string, error) {
				if pins == nil {
					return "", errors.New("no pin source")
				}
				pin := pins.GetPIN()
				if pin == "" {
					return "", errors.New("pairing cancelled")
				}
				return pin, nil
			},
			OnMedia: func(pcm []byte) {
				mu.Lock()
				s := mediaSink
				mu.Unlock()
				if s != nil {
					s.OnMedia(pcm)
				}
			},
			OnState: func(state string) {
				setState(state)
			},
			RequestedCaps:       requested,
			TrustedFingerprints: trusted,
			Reconnect:           true,
			Logger:              log,
		})
		if err != nil {
			fail(err)
			return
		}
		mu.Lock()
		client = c
		mu.Unlock()

		runErr := c.Run(ctx)
		if runErr != nil && ctx.Err() == nil && !errors.Is(runErr, context.Canceled) {
			fail(runErr)
		}
		mu.Lock()
		client = nil
		mu.Unlock()
	}()
	return nil
}

// Stop tears down an active connection.
func Stop() {
	mu.Lock()
	c := client
	cc := cancel
	client = nil
	cancel = nil
	mu.Unlock()
	if c != nil {
		c.Stop()
	}
	if cc != nil {
		cc()
	}
}

// IsRunning reports whether the engine is connected/connecting.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return running
}

// LatestStats returns the latest stats snapshot as JSON (empty before the
// first tick).
func LatestStats() string {
	return latestStatsJSON
}

// PeerList returns the paired devices as JSON.
func PeerList() string {
	if store == nil {
		return "[]"
	}
	peers, err := store.Peers()
	if err != nil || peers == nil {
		return "[]"
	}
	out, _ := json.Marshal(peers)
	return string(out)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func boolBit(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// ForgetPeer removes a paired device by ID (hex 32 chars).
func ForgetPeer(idHex string) error {
	mu.Lock()
	st := store
	mu.Unlock()
	if st == nil {
		return errors.New("mobile: not set up")
	}
	var id [16]byte
	b, err := decodeHex(idHex)
	if err != nil || len(b) != 16 {
		return errors.New("mobile: bad peer id")
	}
	copy(id[:], b)
	return st.RemovePeer(id)
}

// ---------------------------------------------------------------------------
// internals

var latestStatsJSON = "{}"

func setState(s string) {
	mu.Lock()
	state = s
	snap := snapshotStateLocked()
	mu.Unlock()
	publish(snap)
}

func snapshotStateLocked() map[string]any {
	return map[string]any{
		"state": state,
		"error": lastErr,
		"time":  time.Now().UnixMilli(),
	}
}

func publish(snap map[string]any) {
	out, _ := json.Marshal(snap)
	mu.Lock()
	latestStatsJSON = string(out)
	sink := stateSink
	mu.Unlock()
	if sink != nil {
		sink.OnState(string(out))
	}
}

func fail(err error) {
	mu.Lock()
	lastErr = err.Error()
	mu.Unlock()
	publishState()
}

func publishState() {
	mu.Lock()
	s := state
	e := lastErr
	mu.Unlock()
	snap := map[string]any{
		"state": s,
		"error": e,
		"time":  time.Now().UnixMilli(),
	}
	out, _ := json.Marshal(snap)
	mu.Lock()
	latestStatsJSON = string(out)
	sink := stateSink
	mu.Unlock()
	if sink != nil {
		sink.OnState(string(out))
	}
}

// statsLoop periodically publishes a measurement snapshot while the connect
// lifecycle is active. It exits when the lifecycle goroutine closes done (or
// the context is cancelled) and never blocks callers.
func statsLoop(ctx context.Context, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			publishStats()
		}
	}
}

// publishStats merges the client's live measurements with the mobile state
// and publishes the result to LatestStats() and the stats sink (~1 Hz). When
// no client is connected it publishes a disconnected snapshot.
func publishStats() {
	mu.Lock()
	c := client
	s := state
	run := running
	sink := statsSink
	mu.Unlock()

	var out string
	if c != nil {
		snap := map[string]any{}
		if err := json.Unmarshal([]byte(c.StatsJSON()), &snap); err != nil || snap == nil {
			snap = map[string]any{}
		}
		snap["state"] = s
		snap["connected"] = run
		b, err := json.Marshal(snap)
		if err != nil {
			return
		}
		out = string(b)
	} else {
		b, _ := json.Marshal(map[string]any{"state": s, "connected": false})
		out = string(b)
	}

	mu.Lock()
	latestStatsJSON = out
	mu.Unlock()
	if sink != nil {
		sink.OnStats(out)
	}
}

func newLogger() logging.Logger {
	mu.Lock()
	level := logLevel
	mu.Unlock()
	l, err := logging.New(os.Stderr, level, "text")
	if err != nil {
		return logging.Nop()
	}
	return l
}

func decodeHex(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, errors.New("hex length")
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi := hexVal(s[i*2])
		lo := hexVal(s[i*2+1])
		if hi < 0 || lo < 0 {
			return nil, errors.New("hex digit")
		}
		out[i] = byte(hi<<4 | lo)
	}
	return out, nil
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

var _ = filepath.Join
