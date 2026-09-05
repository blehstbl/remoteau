// Package engine orchestrates the v2 stack: capture → codec → QUIC datagram
// fanout (host) and receive → decode → playback (client). Real-time rules
// from remote-au are preserved: bounded rings, TryLock discipline, no
// blocking in callback paths.
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/codec"
	"remote-au/internal/discovery"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
	"remote-au/internal/quality"
	"remote-au/internal/session"
	"remote-au/internal/transport/v2"
)

// HostOptions configures the v2 host (Windows sender).
type HostOptions struct {
	Name           string
	Store          pairing.Store
	Backend        audio.Backend
	ListenAddr     string // default ":47010"
	CaptureSource  audio.Source
	DeviceSelector string
	Format         audio.Format // capture format
	MaxBitrate     int

	// OnPairingCode is invoked when an unpaired device starts pairing; the
	// UI must display this code for the user to enter on the phone.
	OnPairingCode func(code string)
	// PairingTimeout bounds each pairing exchange.
	PairingTimeout time.Duration

	Logger logging.Logger
}

// Host is the v2 audio host.
type Host struct {
	opts   HostOptions
	log    logging.Logger
	id     *pairing.Identity
	trusted map[string]bool // peer fingerprint -> paired

	mu      sync.Mutex
	conns   map[string]*hostStream // per-connection state
	capture audio.Capture

	pairingActive atomic.Bool
}

// NewHost creates a host; call Run to start serving.
func NewHost(opts HostOptions) (*Host, error) {
	if opts.Store == nil {
		return nil, errors.New("engine: host requires a pairing store")
	}
	if opts.Logger == nil {
		opts.Logger = logging.Nop()
	}
	if opts.ListenAddr == "" {
		opts.ListenAddr = fmt.Sprintf(":%d", transportv2.DefaultPort)
	}
	if opts.Format == (audio.Format{}) {
		opts.Format = audio.DefaultFormat()
	}
	if opts.PairingTimeout <= 0 {
		opts.PairingTimeout = 60 * time.Second
	}
	if opts.Name == "" {
		name, err := os.Hostname()
		if err != nil || name == "" {
			name = "remoteau-host"
		}
		opts.Name = name
	}
	return &Host{
		opts:    opts,
		log:     opts.Logger,
		trusted: make(map[string]bool),
		conns:   make(map[string]*hostStream),
	}, nil
}

// identity loads or creates the local identity.
func (h *Host) ensureIdentity() error {
	id, err := h.opts.Store.LoadIdentity()
	if err != nil {
		return fmt.Errorf("load identity: %w", err)
	}
	if id == nil {
		id, err = pairing.NewIdentity()
		if err != nil {
			return err
		}
		if err := h.opts.Store.SaveIdentity(id); err != nil {
			return fmt.Errorf("save identity: %w", err)
		}
		h.log.Infof("generated new device identity %s", id.Fingerprint()[:16])
	}
	h.id = id

	peers, err := h.opts.Store.Peers()
	if err != nil {
		return err
	}
	for _, p := range peers {
		h.trusted[p.Fingerprint] = true
	}
	return nil
}

// Run serves until ctx is done.
func (h *Host) Run(ctx context.Context) error {
	if err := h.ensureIdentity(); err != nil {
		return err
	}

	cert, err := transportv2.DeviceCertificate(h.id)
	if err != nil {
		return err
	}
	tlsCfg := transportv2.TLSConfig(cert, transportv2.FingerprintVerifier(h.trusted, true))

	listener, err := transportv2.Listen(h.opts.ListenAddr, tlsCfg)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	h.log.Infof("v2 host listening on %s as %q", listener.Addr(), h.opts.Name)

	// Discovery responder (v2 announce includes the protocol version).
	go func() {
		if err := h.runDiscoveryV2(ctx, listener.Addr()); err != nil && ctx.Err() == nil {
			h.log.Warnf("discovery responder stopped: %v", err)
		}
	}()

	// Open capture lazily on first stream; a single shared capture feeds all
	// receivers (fanout).
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go h.handleConn(ctx, conn)
	}
}

// runDiscoveryV2 answers discovery queries with a v2 announce (adds the
// protocol version byte after the v1 announce format).
func (h *Host) runDiscoveryV2(ctx context.Context, listenAddr net.Addr) error {
	conn, _, err := discovery.ListenFirst([]int{47001, 48001, 49001})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	announcePort := transportv2.DefaultPort
	if udpAddr, ok := listenAddr.(*net.UDPAddr); ok && udpAddr.Port != 0 {
		announcePort = udpAddr.Port
	}

	instanceID, err := discovery.NewInstanceID()
	if err != nil {
		return err
	}

	buf := make([]byte, discovery.MaxPacketBytes+1)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			return err
		}
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read discovery: %w", err)
		}
		msg, err := discovery.Decode(buf[:n])
		if err != nil || msg.Type != discovery.TypeQuery {
			continue
		}
		// Reply with the v2 announce: v1 format + trailing version byte.
		announce, err := discovery.EncodeAnnounce(discovery.Announce{
			TCPPort:        announcePort,
			InstanceID:     instanceID,
			AdvertisedAddr: src.Addr(),
			Name:           h.opts.Name + "\x02", // suffix marker: v2 host
		})
		if err != nil {
			continue
		}
		announce = append(announce, 2) // protocol version marker
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
			_, _ = conn.WriteToUDPAddrPort(announce, src)
		}
	}
}

// hostStream carries per-connection state.
type hostStream struct {
	conn    *transportv2.Conn
	sess    *session.Session
	codec   codec.Codec
	quality *quality.Controller
	cancel  context.CancelFunc
	stats   hostStats
}

type hostStats struct {
	lossEWMA      float64
	jitterEWMA    float64
	rttEWMA       float64
	bufDepthEWMA  float64
	underruns     uint64
	lastStatsUnix int64
}

func (h *Host) handleConn(ctx context.Context, conn *transportv2.Conn) {
	remoteFP := peerFingerprint(conn)
	h.log.Infof("v2 connection from %s (cert %s…)", conn.Inner().RemoteAddr(), short(remoteFP))

	sess, err := session.New(conn, session.RoleHost, h.id.DeviceID())
	if err != nil {
		h.log.Warnf("session setup failed: %v", err)
		_ = conn.Close()
		return
	}
	defer func() {
		_ = conn.Close()
	}()

	hostCaps := h.offerCaps()

	peerHello, err := sess.ExchangeHellos(ctx, h.opts.Name, hostCaps)
	if err != nil {
		h.log.Warnf("hello exchange failed: %v", err)
		return
	}
	h.log.Infof("peer %q (id %x…)", peerHello.DeviceName, peerHello.DeviceID[:4])

	paired := h.trusted[remoteFP]
	if !paired {
		if err := h.runPairing(ctx, sess); err != nil {
			if errors.Is(err, pairing.ErrPinMismatch) {
				h.log.Warnf("pairing failed: PIN mismatch")
			} else {
				h.log.Warnf("pairing failed: %v", err)
			}
			return
		}
		h.trusted[remoteFP] = true
		h.log.Infof("paired with %q (%s…)", peerHello.DeviceName, short(remoteFP))
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &hostStream{
		conn:    conn,
		sess:    sess,
		cancel:  cancel,
		quality: quality.NewController(32000, maxInt(h.opts.MaxBitrate, 256000)),
	}
	sess.SetHandler(func(m protocolv2.Message) { h.handleControl(st, m) })

	// Wait for the stream request and start sending.
	if err := sess.StartStream(streamCtx, func(req protocolv2.Caps) (protocolv2.Caps, uint32, error) {
		return h.startStreamFor(st, req)
	}); err != nil {
		h.log.Warnf("stream start failed: %v", err)
		return
	}

	// Serve control messages until the connection ends.
	_ = sess.ServeLoop(streamCtx)
}

// offerCaps describes what the host can produce.
func (h *Host) offerCaps() protocolv2.Caps {
	codecID := uint8(protocolv2.CodecPCMS16LE)
	frameMs := uint8(5) // PCM must fit the datagram budget
	if opusAvailable() {
		codecID = protocolv2.CodecOpus
		frameMs = 10
	}
	return protocolv2.Caps{
		Codec:       codecID,
		SampleRate:  uint32(h.opts.Format.Rate),
		Channels:    uint8(h.opts.Format.Channels),
		FrameMs:     frameMs,
		OpusBitrate: uint32(minInt(h.opts.MaxBitrate, 256000)),
	}
}

// runPairing performs the v2 pairing handshake over the control stream. The
// HOST displays a generated 6-digit PIN; the client user enters it.
func (h *Host) runPairing(ctx context.Context, sess *session.Session) error {
	if !h.pairingActive.CompareAndSwap(false, true) {
		return errors.New("another pairing is already in progress")
	}
	defer h.pairingActive.Store(false)

	pctx, cancel := context.WithTimeout(ctx, h.opts.PairingTimeout)
	defer cancel()

	// Wait for PAIR_BEGIN.
	m, err := sess.RecvRaw(pctx)
	if err != nil {
		return err
	}
	if m.Type != protocolv2.MsgPairBegin || len(m.Payload) != 16 {
		return errors.New("expected pair-begin")
	}

	// Generate and surface the PIN.
	code := generatePIN()
	if h.opts.OnPairingCode != nil {
		h.opts.OnPairingCode(code)
	}
	h.log.Infof("pairing requested; enter code %s on the device", code)

	ex, err := pairing.NewExchange(pairing.RoleSender, code, h.opts.Name)
	if err != nil {
		return err
	}
	if err := ex.OnBegin(m.Payload); err != nil {
		return err
	}

	challenge, err := ex.ChallengeMessage()
	if err != nil {
		return err
	}
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgPairChallenge, Payload: challenge}); err != nil {
		return err
	}

	confirm, err := sess.RecvRaw(pctx)
	if err != nil {
		return err
	}
	if confirm.Type != protocolv2.MsgPairConfirm {
		return fmt.Errorf("expected pair-confirm, got %04x", confirm.Type)
	}
	if err := ex.OnConfirm(confirm.Payload); err != nil {
		return err
	}

	result, err := ex.ResultMessage(h.id.DeviceID())
	if err != nil {
		return err
	}
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgPairResult, Payload: result}); err != nil {
		return err
	}

	// Persist the peer.
	rec := pairing.PeerRecord{
		ID:            sess.PeerID(),
		Name:          sess.PeerName(),
		PairingSecret: ex.Secret(),
		Fingerprint:   peerFingerprint(sess.Conn()),
		PairedAt:      time.Now().Unix(),
	}
	return h.opts.Store.SavePeer(rec)
}

// startStreamFor opens the shared capture (once) and selects the codec.
func (h *Host) startStreamFor(st *hostStream, req protocolv2.Caps) (protocolv2.Caps, uint32, error) {
	caps, err := h.negotiateCaps(req)
	if err != nil {
		return protocolv2.Caps{}, 0, err
	}
	cfg := codec.Config{
		IsOpus:     caps.Codec == protocolv2.CodecOpus,
		SampleRate: int(caps.SampleRate),
		Channels:   int(caps.Channels),
		FrameMs:    int(caps.FrameMs),
		Bitrate:    int(caps.OpusBitrate),
		FEC:        caps.FEC == 1,
		DTX:        caps.DTX == 1,
		Complexity: 5,
		AppID:      int(caps.AppID),
	}
	codecImpl, err := codec.New(cfg, h.log)
	if err != nil {
		return protocolv2.Caps{}, 0, err
	}
	st.codec = codecImpl

	if err := h.ensureCapture(); err != nil {
		return protocolv2.Caps{}, 0, err
	}

	go h.sendLoop(st, caps)
	return caps, 1, nil
}

// negotiateCaps reconciles the request with what we can produce.
func (h *Host) negotiateCaps(req protocolv2.Caps) (protocolv2.Caps, error) {
	out := h.offerCaps()
	out.OpusBitrate = clampU32(req.OpusBitrate, 16000, uint32(maxInt(h.opts.MaxBitrate, 256000)))
	out.FEC = boolBit(req.FEC == 1 && opusAvailable())
	out.DTX = boolBit(req.DTX == 1 && opusAvailable())

	if req.SampleRate != 0 {
		out.SampleRate = req.SampleRate
	}
	if req.Channels != 0 {
		out.Channels = req.Channels
	}
	// Codec: honor the request when supported.
	if req.Codec == protocolv2.CodecPCMS16LE || !opusAvailable() {
		out.Codec = protocolv2.CodecPCMS16LE
		out.FrameMs = 5
	} else if req.Codec == protocolv2.CodecOpus {
		out.Codec = protocolv2.CodecOpus
		out.FrameMs = clampFrameMs(req.FrameMs)
	}
	if out.Codec == protocolv2.CodecPCMS16LE {
		// PCM must fit the datagram budget: frameMs ≤ 5 for stereo 48k.
		for int(out.FrameMs)*int(out.Channels)*2 > protocolv2.MaxDatagramPayload-32 {
			if out.FrameMs > 2 {
				out.FrameMs /= 2
			} else {
				break
			}
		}
	}
	if err := out.Validate(); err != nil {
		return protocolv2.Caps{}, err
	}
	return out, nil
}

func clampFrameMs(ms uint8) uint8 {
	switch ms {
	case 2, 5, 10, 20:
		return ms
	default:
		return 10
	}
}

func boolBit(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func clampU32(v, lo, hi uint32) uint32 {
	if v == 0 {
		return hi
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensureCapture opens the shared capture stream once.
func (h *Host) ensureCapture() error {
	return h.ensureCaptureViaBackend()
}

// sendLoop reads the shared capture and sends encoded frames to one client.
func (h *Host) sendLoop(st *hostStream, caps protocolv2.Caps) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-st.conn.Inner().Context().Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	frameBytes := int(caps.Channels) * 2 * int(caps.FrameMs) * int(caps.SampleRate) / 1000
	chunk := make([]byte, frameBytes)
	var seq uint32
	var captureFrame uint64
	filled := 0
	var buf [4096]byte

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n := h.readCapture(buf[:])
		if n == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		src := buf[:n]
		for len(src) > 0 {
			take := min(frameBytes-filled, len(src))
			copy(chunk[filled:], src[:take])
			filled += take
			src = src[take:]
			if filled == frameBytes {
				wire, err := st.codec.EncodeFrame(chunk)
				if err != nil {
					h.log.Warnf("encode: %v", err)
					filled = 0
					continue
				}
				packet, err := protocolv2.AppendMedia(nil, protocolv2.Media{
					StreamID:    0,
					FormatGen:   1,
					Flags:       0,
					Seq:         seq,
					CaptureTsUs: captureFrame * uint64(1000000) / uint64(caps.SampleRate),
					Payload:     wire,
				})
				if err == nil {
					if err := st.conn.SendMedia(packet); err != nil {
						h.log.Debugf("send media: %v", err)
						return
					}
				}
				seq++
				filled = 0
			}
		}
		captureFrame += uint64(n / (int(caps.Channels) * 2))
	}
}

// readCapture reads from the shared capture; when no capture is open (no
// stream yet), returns 0.
func (h *Host) readCapture(dst []byte) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.capture == nil {
		return 0
	}
	return h.capture.Read(dst)
}

// handleControl processes host-relevant control messages (stats, format-ack).
func (h *Host) handleControl(st *hostStream, m protocolv2.Message) {
	switch m.Type {
	case protocolv2.MsgStats:
		stats, err := protocolv2.DecodeStats(m.Payload)
		if err != nil {
			return
		}
		st.stats.lossEWMA = ewma(st.stats.lossEWMA, float64(stats.LossPct)/100.0)
		st.stats.jitterEWMA = ewma(st.stats.jitterEWMA, float64(stats.JitterUs)/1000.0)
		st.stats.rttEWMA = ewma(st.stats.rttEWMA, float64(stats.RTTUs)/1000.0)
		st.stats.bufDepthEWMA = ewma(st.stats.bufDepthEWMA, float64(stats.BufDepthMs))
		st.stats.underruns += uint64(stats.Underruns)

		// Adaptive quality (Phase 6): drive bitrate/FEC from receiver stats.
		dec := st.quality.Update(quality.Sample{
			LossPercent: st.stats.lossEWMA,
			LatePercent: 0,
			JitterMs:    st.stats.jitterEWMA,
			RTTMs:       st.stats.rttEWMA,
			BufDepthMs:  st.stats.bufDepthEWMA,
			Underruns:   uint64(stats.Underruns),
		}, time.Now())
		h.applyQuality(st, dec)
	case protocolv2.MsgPing:
		// Answered by the session layer normally; ignore here.
	case protocolv2.MsgFormatAck:
		// Codec switch acknowledged.
	default:
	}
}

// applyQuality applies a quality decision to the stream's encoder when
// possible (Opus supports live bitrate/FEC changes).
func (h *Host) applyQuality(st *hostStream, dec quality.Decision) {
	type bitrateSetter interface {
		SetBitrate(int) error
	}
	type fecSetter interface {
		SetFEC(bool, int) error
	}
	if bs, ok := st.codec.(bitrateSetter); ok {
		if err := bs.SetBitrate(dec.BitrateBps); err != nil {
			h.log.Debugf("set bitrate: %v", err)
		}
	}
	if fs, ok := st.codec.(fecSetter); ok {
		if err := fs.SetFEC(dec.FECEnabled, dec.ExpectedLoss); err != nil {
			h.log.Debugf("set fec: %v", err)
		}
	}
}

func ewma(old, v float64) float64 {
	const a = 0.3
	if old == 0 {
		return v
	}
	return old + a*(v-old)
}

func short(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

func generatePIN() string {
	const digits = "0123456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	return string(b)
}

func peerFingerprint(conn *transportv2.Conn) string {
	state := conn.Inner().ConnectionState()
	if len(state.TLS.PeerCertificates) == 0 {
		return "unknown"
	}
	sum := sha256.Sum256(state.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

func opusAvailable() bool {
	return codec.OpusAvailable()
}

func openCapture(source audio.Source, selector string, format audio.Format, logger logging.Logger) (audio.Capture, error) {
	return nil, errors.New("engine: host requires a backend (Backend option)")
}

// ensureCapture opens the shared capture stream once (via the backend).
func (h *Host) ensureCaptureViaBackend() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.capture != nil {
		return nil
	}
	if h.opts.Backend == nil {
		return errors.New("engine: host requires a backend (Backend option)")
	}
	cap, err := h.opts.Backend.OpenCapture(audio.CaptureOptions{
		Format:         h.opts.Format,
		Source:         h.opts.CaptureSource,
		DeviceSelector: h.opts.DeviceSelector,
		Logger:         h.log,
	})
	if err != nil {
		return fmt.Errorf("open capture: %w", err)
	}
	h.capture = cap
	return nil
}
