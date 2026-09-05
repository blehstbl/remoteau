// Package engine orchestrates the v2 stack: capture → codec → QUIC datagram
// fanout (host) and receive → decode → playback (client). Real-time rules
// from remote-au are preserved: bounded rings, TryLock discipline, no
// blocking in callback paths.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
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
	// OnReceiversChanged is invoked whenever the connected-receiver set or
	// its stats change meaningfully (UI surface).
	OnReceiversChanged func(receivers []ReceiverInfo)
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
	conns   map[string]*hostStream // per-connection state (key: peer ID hex)
	capture audio.Capture

	// resumeStates persist stream contexts so receivers can RESUME after a
	// reconnect without renegotiation (key: resume token).
	resumeStates map[[32]byte]*resumeState

	// pairMu serializes pairing exchanges (one code on screen at a time);
	// later requests wait their turn instead of failing.
	pairMu sync.Mutex
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
		opts:         opts,
		log:          opts.Logger,
		trusted:      make(map[string]bool),
		conns:        make(map[string]*hostStream),
		resumeStates: make(map[[32]byte]*resumeState),
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

	// Default-endpoint monitor: follow Windows default-device changes so
	// loopback capture survives the user switching outputs (Phase 9).
	go h.monitorDefaultEndpoint(ctx)

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

// primaryIPv4 returns the host's primary outbound IPv4 address (no traffic
// is sent; a UDP "connect" just makes the OS pick the route).
func primaryIPv4() netip.Addr {
	conn, err := net.Dial("udp", "192.0.2.1:9") // TEST-NET, never sent to
	if err != nil {
		return netip.Addr{}
	}
	defer func() { _ = conn.Close() }()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil {
		if ip4 := addr.IP.To4(); ip4 != nil {
			return netip.AddrFrom4([4]byte(ip4))
		}
	}
	return netip.Addr{}
}

// runDiscoveryV2 answers discovery queries with a clean v1-compatible
// announce (any stock remote-au parser accepts it) plus a type-3 v2
// announce carrying the protocol version for v2-aware finders.
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

	ownAddr := primaryIPv4()

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
		advertised := ownAddr
		if !advertised.IsValid() {
			advertised = src.Addr()
		}
		v1Announce, err := discovery.EncodeAnnounce(discovery.Announce{
			TCPPort:        announcePort,
			InstanceID:     instanceID,
			AdvertisedAddr: advertised,
			Name:           h.opts.Name,
		})
		if err != nil {
			continue
		}
		v2Announce, err := discovery.EncodeAnnounceV2(discovery.Announce{
			TCPPort:        announcePort,
			InstanceID:     instanceID,
			AdvertisedAddr: advertised,
			Name:           h.opts.Name,
			ProtoVersion:   2,
		})
		if err != nil {
			continue
		}
		if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err == nil {
			_, _ = conn.WriteToUDPAddrPort(v1Announce, src)
			_, _ = conn.WriteToUDPAddrPort(v2Announce, src)
		}
	}
}

// monitorDefaultEndpoint watches the default render endpoint and reopens
// loopback capture when it changes. Polling (no COM event sink) keeps the
// implementation cgo-free and simple; 2s latency is imperceptible.
func (h *Host) monitorDefaultEndpoint(ctx context.Context) {
	if h.opts.CaptureSource != audio.SourceLoopback || h.opts.DeviceSelector != "" {
		return
	}
	type endpointer interface {
		DefaultRenderEndpointID() (string, error)
	}
	ep, ok := h.opts.Backend.(endpointer)
	if !ok {
		return
	}

	last := ""
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		id, err := ep.DefaultRenderEndpointID()
		if err != nil || id == "" {
			continue
		}
		if last != "" && id != last {
			h.mu.Lock()
			old := h.capture
			h.capture = nil
			h.mu.Unlock()
			if old != nil {
				_ = old.Close()
				h.log.Infof("default render endpoint changed; capture will follow the new device")
			}
		}
		last = id
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

	// seq is the next media sequence to send (updated by sendLoop only).
	seq atomic.Uint32

	// Per-receiver output controls.
	volume float64 // 0..2, 1 = unity
	muted  bool

	// Codec/stream bookkeeping.
	formatGen  uint32
	pendingGen uint32
	caps       protocolv2.Caps
	lastSwitch time.Time
	name       string
}

type hostStats struct {
	lossEWMA      float64
	lateEWMA      float64
	jitterEWMA    float64
	rttEWMA       float64
	bufDepthEWMA  float64
	underruns     uint64
	lastStatsUnix int64
}

// resumeState is a persisted stream context for reconnecting receivers.
type resumeState struct {
	caps      protocolv2.Caps
	formatGen uint32
	nextSeq   uint32
	deviceID  [16]byte
}

func (h *Host) handleConn(ctx context.Context, conn *transportv2.Conn) {
	remoteFP := PeerCertFingerprint(conn)
	h.log.Infof("v2 connection from %s (cert %s…)", conn.Inner().RemoteAddr(), short(remoteFP))

	sess, err := session.New(conn, session.RoleHost, h.id.DeviceID())
	if err != nil {
		h.log.Warnf("session setup failed: %v", err)
		_ = conn.Close()
		return
	}
	sess.SetLocalName(h.opts.Name)
	defer func() {
		_ = conn.Close()
	}()

	hostCaps := h.offerCaps()

	// The first control message decides the path: RESUME (reconnect without
	// renegotiation) or HELLO (full flow). The host sends its own HELLO
	// inside CompleteHello, after reading the receiver's.
	first, err := sess.RecvRaw(ctx)
	if err != nil {
		h.log.Warnf("first control message failed: %v", err)
		return
	}

	if first.Type == protocolv2.MsgResume {
		if h.tryResume(sess, conn, first.Payload) {
			return
		}
		// Unknown/expired token: fall through to the full flow; the next
		// message must be the receiver's HELLO.
		first, err = sess.RecvRaw(ctx)
		if err != nil {
			h.log.Warnf("hello after failed resume: %v", err)
			return
		}
	}

	if first.Type != protocolv2.MsgHello {
		h.log.Warnf("expected hello, got %04x", first.Type)
		return
	}
	peerHello, err := sess.CompleteHello(first, hostCaps)
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
		volume:  1.0,
		name:    peerHello.DeviceName,
	}
	sess.SetHandler(func(m protocolv2.Message) { h.handleControl(st, m) })
	h.registerStream(st)
	defer h.unregisterStream(st)

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

// registerStream/unregisterStream maintain the live receiver list surfaced in
// the tray/UI.
func (h *Host) registerStream(st *hostStream) {
	h.mu.Lock()
	h.conns[st.sess.PeerIDString()] = st
	h.mu.Unlock()
	h.notifyReceivers()
}

func (h *Host) unregisterStream(st *hostStream) {
	h.mu.Lock()
	delete(h.conns, st.sess.PeerIDString())
	h.mu.Unlock()
	h.notifyReceivers()
}
func (h *Host) notifyReceivers() {
	if h.opts.OnReceiversChanged != nil {
		h.opts.OnReceiversChanged(h.Receivers())
	}
}

// ReceiverInfo describes one connected receiver for UIs.
type ReceiverInfo struct {
	DeviceID  string  `json:"device_id"`
	Name      string  `json:"name"`
	Addr      string  `json:"addr"`
	LossPct   float64 `json:"loss_pct"`
	JitterMs  float64 `json:"jitter_ms"`
	RTTMs     float64 `json:"rtt_ms"`
	BufMs     float64 `json:"buf_ms"`
	Codec     string  `json:"codec"`
	Volume    float64 `json:"volume"`
	Muted     bool    `json:"muted"`
}

// Receivers snapshots the connected receivers.
func (h *Host) Receivers() []ReceiverInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ReceiverInfo, 0, len(h.conns))
	for _, st := range h.conns {
		peerID := st.sess.PeerID()
		out = append(out, ReceiverInfo{
			DeviceID: hex.EncodeToString(peerID[:]),
			Name:     st.name,
			Addr:     st.conn.Inner().RemoteAddr().String(),
			LossPct:  st.stats.lossEWMA,
			JitterMs: st.stats.jitterEWMA,
			RTTMs:    st.stats.rttEWMA,
			BufMs:    st.stats.bufDepthEWMA,
			Codec:    st.codecName(),
			Volume:   st.volume,
			Muted:    st.muted,
		})
	}
	return out
}

func (s *hostStream) codecName() string {
	if s.codec == nil {
		return "-"
	}
	return s.codec.Name()
}

// SetVolume scales one receiver's stream (1 = unity).
func (h *Host) SetVolume(deviceIDHex string, volume float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.conns[deviceIDHex]; ok {
		st.volume = clampF64(volume, 0, 2)
	}
}

// SetMuted mutes one receiver's stream.
func (h *Host) SetMuted(deviceIDHex string, muted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.conns[deviceIDHex]; ok {
		st.muted = muted
	}
}

// MuteAll mutes (or unmutes) every receiver.
func (h *Host) MuteAll(muted bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range h.conns {
		st.muted = muted
	}
}

// SetQuality applies a quality preset to all receivers (bitrate bounds +
// FEC policy bias).
func (h *Host) SetQuality(minBitrate, maxBitrate int, fecBias bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, st := range h.conns {
		st.quality = quality.NewController(minBitrate, maxBitrate)
		_ = fecBias
	}
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
// HOST displays a generated 6-digit PIN; the client user enters it. Pairings
// are serialized: a second device waits for the first exchange to finish.
func (h *Host) runPairing(ctx context.Context, sess *session.Session) error {
	h.pairMu.Lock()
	defer h.pairMu.Unlock()

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
		Fingerprint:   PeerCertFingerprint(sess.Conn()),
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
	codecImpl, err := codec.New(codecConfigFromCaps(caps), h.log)
	if err != nil {
		return protocolv2.Caps{}, 0, err
	}
	st.codec = codecImpl
	st.caps = caps
	st.formatGen = 1
	st.pendingGen = 1

	if err := h.ensureCapture(); err != nil {
		return protocolv2.Caps{}, 0, err
	}

	h.storeResumeState(st)
	go h.sendLoop(st, caps, 0)
	return caps, st.formatGen, nil
}

// codecConfigFromCaps maps wire caps to a codec config.
func codecConfigFromCaps(caps protocolv2.Caps) codec.Config {
	complexity := int(caps.Complexity)
	if complexity == 0 {
		complexity = 5
	}
	return codec.Config{
		IsOpus:     caps.Codec == protocolv2.CodecOpus,
		SampleRate: int(caps.SampleRate),
		Channels:   int(caps.Channels),
		FrameMs:    int(caps.FrameMs),
		Bitrate:    int(caps.OpusBitrate),
		FEC:        caps.FEC == 1,
		DTX:        caps.DTX == 1,
		Complexity: complexity,
		AppID:      int(caps.AppID),
	}
}

// storeResumeState records the stream context so the receiver can RESUME
// after a reconnect without renegotiation.
func (h *Host) storeResumeState(st *hostStream) {
	token := st.sess.ResumeToken()
	if token == ([32]byte{}) {
		return
	}
	h.mu.Lock()
	h.resumeStates[token] = &resumeState{
		caps:      st.caps,
		formatGen: st.formatGen,
		nextSeq:   st.seq.Load(),
		deviceID:  st.sess.PeerID(),
	}
	h.mu.Unlock()
}

// tryResume handles a RESUME message. On success it answers RESUME_OK,
// re-attaches the stream and serves until the connection ends.
func (h *Host) tryResume(sess *session.Session, conn *transportv2.Conn, payload []byte) bool {
	if len(payload) < 36 {
		return false
	}
	var token [32]byte
	copy(token[:], payload[:32])
	h.mu.Lock()
	st_, ok := h.resumeStates[token]
	h.mu.Unlock()
	if !ok {
		h.log.Debugf("resume token not found; falling back to full handshake")
		return false
	}
	nextSeq := st_.nextSeq
	formatGen := st_.formatGen
	caps := st_.caps

	// RESUME_OK: nextSeq(4) formatGen(4) caps(15).
	okPayload := make([]byte, 0, 23)
	okPayload = appendU32LE(okPayload, nextSeq)
	okPayload = appendU32LE(okPayload, formatGen)
	okPayload = caps.Append(okPayload)
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgResumeOK, Payload: okPayload}); err != nil {
		return false
	}
	h.log.Infof("resumed stream for %q (fmtGen=%d nextSeq=%d)", sess.PeerName(), formatGen, nextSeq)

	streamCtx, cancel := context.WithCancel(context.Background())
	st := &hostStream{
		conn:      conn,
		sess:      sess,
		cancel:    cancel,
		quality:   quality.NewController(32000, maxInt(h.opts.MaxBitrate, 256000)),
		volume:    1.0,
		name:      sess.PeerName(),
		caps:      caps,
		formatGen: formatGen,
		pendingGen: formatGen,
	}
	st.seq.Store(nextSeq)
	codecImpl, err := codec.New(codecConfigFromCaps(caps), h.log)
	if err != nil {
		cancel()
		return false
	}
	st.codec = codecImpl
	sess.SetHandler(func(m protocolv2.Message) { h.handleControl(st, m) })
	h.registerStream(st)
	defer h.unregisterStream(st)
	go h.sendLoop(st, caps, nextSeq)
	_ = sess.ServeLoop(streamCtx)
	return true
}

func appendU32LE(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
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
	// Complexity / application mode: honor the receiver's request.
	if req.Complexity != 0 {
		out.Complexity = req.Complexity
	}
	if req.AppID != 0 {
		out.AppID = req.AppID
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

// sendLoop reads the shared capture and sends encoded frames to one client,
// applying that receiver's volume/mute and format generation.
func (h *Host) sendLoop(st *hostStream, caps protocolv2.Caps, startSeq uint32) {
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
	seq := startSeq
	var captureFrame uint64
	filled := 0
	var buf [4096]byte

	for {
		select {
		case <-ctx.Done():
			// Persist the resume position before exiting.
			h.storeResumeState(st)
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
			applyGain(chunk[filled:filled+take], src[:take], st.volume, st.muted)
			filled += take
			src = src[take:]
			if filled == frameBytes {
				wire, err := st.codec.EncodeFrame(chunk)
				if err != nil {
					h.log.Warnf("encode: %v", err)
					filled = 0
					continue
				}
	gen := st.formatGen
	packet, err := protocolv2.AppendMedia(nil, protocolv2.Media{
		StreamID:    0,
		FormatGen:   uint16(gen),
		Flags:       0,
		Seq:         seq,
		CaptureTsUs: captureFrame * uint64(1000000) / uint64(caps.SampleRate),
		Payload:     wire,
	})
				if err == nil {
					if err := st.conn.SendMedia(packet); err != nil {
						h.log.Debugf("send media: %v", err)
						h.storeResumeState(st)
						return
					}
				}
				st.seq.Store(seq + 1)
				seq++
				filled = 0
			}
		}
		captureFrame += uint64(n / (int(caps.Channels) * 2))
	}
}

// applyGain scales S16LE samples into dst (mute writes silence).
func applyGain(dst, src []byte, volume float64, muted bool) {
	if muted {
		clear(dst[:len(src)])
		return
	}
	if volume == 1.0 {
		copy(dst, src)
		return
	}
	for i := 0; i+1 < len(src); i += 2 {
		s := int16(uint16(src[i]) | uint16(src[i+1])<<8)
		v := int16(clampF64(float64(s)*volume, -32768, 32767))
		dst[i] = byte(v & 0xFF)
		dst[i+1] = byte(uint16(v) >> 8)
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

// handleControl processes host-relevant control messages: stats (adaptive
// quality), ping (RTT), volume/mute, stream stop, and format acks.
func (h *Host) handleControl(st *hostStream, m protocolv2.Message) {
	switch m.Type {
	case protocolv2.MsgStats:
		stats, err := protocolv2.DecodeStats(m.Payload)
		if err != nil {
			return
		}
		st.stats.lossEWMA = ewma(st.stats.lossEWMA, float64(stats.LossPct)/100.0)
		st.stats.lateEWMA = ewma(st.stats.lateEWMA, float64(stats.LatePct)/100.0)
		st.stats.jitterEWMA = ewma(st.stats.jitterEWMA, float64(stats.JitterUs)/1000.0)
		st.stats.rttEWMA = ewma(st.stats.rttEWMA, float64(stats.RTTUs)/1000.0)
		st.stats.bufDepthEWMA = ewma(st.stats.bufDepthEWMA, float64(stats.BufDepthMs))
		st.stats.underruns += uint64(stats.Underruns)

		// Adaptive quality (Phase 6): drive bitrate/FEC from receiver stats.
		dec := st.quality.Update(quality.Sample{
			LossPercent: st.stats.lossEWMA,
			LatePercent: st.stats.lateEWMA,
			JitterMs:    st.stats.jitterEWMA,
			RTTMs:       st.stats.rttEWMA,
			BufDepthMs:  st.stats.bufDepthEWMA,
			Underruns:   uint64(stats.Underruns),
		}, time.Now())
		h.applyQuality(st, dec)

	case protocolv2.MsgPing:
		if len(m.Payload) >= 8 {
			pong := make([]byte, 16)
			copy(pong, m.Payload[:8])
			nowUs := uint64(time.Now().UnixNano() / 1000)
			for i := 0; i < 8; i++ {
				pong[8+i] = byte(nowUs >> (8 * i))
			}
			_ = st.sess.SendRaw(protocolv2.Message{
				Type: protocolv2.MsgPong, RequestID: m.RequestID, Payload: pong,
			})
		}

	case protocolv2.MsgVolume:
		if len(m.Payload) >= 3 {
			vol := float64(uint16(m.Payload[0])|uint16(m.Payload[1])<<8) / 65536.0
			st.volume = clampF64(vol, 0, 2)
			st.muted = m.Payload[2] != 0
			h.notifyReceivers()
		}

	case protocolv2.MsgStreamStop:
		// Receiver is done: tear the connection down (idempotent).
		_ = st.conn.Close()

	case protocolv2.MsgFormatAck:
		// Codec switch acknowledged by the receiver.
		st.lastSwitch = time.Now()

	default:
	}
}

// applyQuality applies a quality decision: live bitrate/FEC changes when the
// codec supports them, and PCM↔Opus switches via FORMAT_UPDATE (with a
// minimum dwell between switches so settings do not flap).
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

	// Codec switching (Auto mode).
	const switchDwell = 10 * time.Second
	wantOpus := dec.SwitchToOpus && opusAvailable()
	wantPCM := dec.SwitchToPCM
	if !wantOpus && !wantPCM {
		return
	}
	isOpus := st.caps.Codec == protocolv2.CodecOpus
	if (wantOpus && isOpus) || (wantPCM && !isOpus) {
		return
	}
	if time.Since(st.lastSwitch) < switchDwell {
		return
	}
	h.switchCodec(st, wantOpus)
}

// switchCodec rebuilds the encoder for the other codec and informs the
// receiver with FORMAT_UPDATE.
func (h *Host) switchCodec(st *hostStream, toOpus bool) {
	newCaps := st.caps
	if toOpus {
		newCaps.Codec = protocolv2.CodecOpus
		if newCaps.FrameMs < 10 {
			newCaps.FrameMs = 10
		}
	} else {
		newCaps.Codec = protocolv2.CodecPCMS16LE
		newCaps.FrameMs = 5
		for int(newCaps.FrameMs)*int(newCaps.Channels)*2 > protocolv2.MaxDatagramPayload-32 {
			if newCaps.FrameMs > 2 {
				newCaps.FrameMs /= 2
			} else {
				break
			}
		}
	}
	newCaps, err := h.negotiateCaps(newCaps)
	if err != nil {
		return
	}
	codecImpl, err := codec.New(codecConfigFromCaps(newCaps), h.log)
	if err != nil {
		return
	}

	newGen := st.formatGen + 1
	// Swap encoder + generation atomically enough for the send loop (worst
	// case: one frame decoded with the previous codec — PLC covers it).
	st.codec = codecImpl
	st.caps = newCaps
	st.formatGen = newGen
	st.lastSwitch = time.Now()

	payload := protocolv2.AppendFormatUpdate(nil, protocolv2.FormatUpdate{
		FormatGen: newGen,
		Caps:      newCaps,
	})
	_ = st.sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgFormatUpdate, Payload: payload})
	h.storeResumeState(st)
	h.log.Infof("switched receiver %q to %s (gen %d)", st.name, newCaps.CodecName(), newGen)
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
