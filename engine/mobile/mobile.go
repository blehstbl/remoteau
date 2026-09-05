// Package mobile exposes the RemoteAU v2 client engine to iOS via
// gomobile bind. The Swift app drives it through the small API below:
//
//	Setup(dataDir)             — load/create identity + trust store
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
	store    *fileStore

	client   *engine.Client
	cancel   context.CancelFunc
	running  bool
	state    = "idle"
	lastErr  string

	mediaSink MediaSink
	stateSink StateSink
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
	mu.Lock()
	if running {
		mu.Unlock()
		return errors.New("mobile: already connected")
	}
	if store == nil || identity == nil {
		mu.Unlock()
		return errors.New("mobile: call Setup first")
	}
	if hostAddr == "" {
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

	go func() {
		defer func() {
			mu.Lock()
			running = false
			mu.Unlock()
			setState("idle")
		}()

		c, err := engine.NewClient(engine.ClientOptions{
			HostAddr: hostAddr,
			Name:     deviceName,
			Store:    storeRef,
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
			RequestedCaps: requested,
			Reconnect:     true,
			Logger:        log,
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

// publishStats is called by the stats ticker in the client loop (wired via
// the state sink payload in future revisions).
func publishStats() { publishState() }

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
