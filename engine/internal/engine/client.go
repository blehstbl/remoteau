package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"remote-au/internal/codec"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
	"remote-au/internal/relay"
	"remote-au/internal/session"
	"remote-au/internal/transport/v2"
)

// ClientOptions configures the v2 client (iPhone receiver; also used by the
// CLI test client).
type ClientOptions struct {
	HostAddr string // "ip:port"
	Name     string
	Store    pairing.Store

	// TrustedFingerprints are host certificate fingerprints (hex, SHA-256)
	// this client will accept. When empty, any host certificate is accepted
	// (first-pairing mode); after a successful pairing, callers should pin
	// the host fingerprint here so later connections are authenticated.
	TrustedFingerprints map[string]bool

	// PINProvider is called when pairing is needed; the UI prompts the user
	// to enter the code displayed on the PC.
	PINProvider func() (string, error)

	// OnMedia delivers decoded PCM (S16LE) for playback. Must be non-blocking.
	OnMedia func(pcm []byte)

	// OnState receives state transitions ("connecting", "streaming",
	// "reconnecting", "idle", "error: ...") for UIs.
	OnState func(state string)

	// RequestedCaps selects what the client asks for. Zero fields let the
	// host decide.
	RequestedCaps protocolv2.Caps

	// Reconnect enables the automatic reconnect loop (Phase 9).
	Reconnect bool

	// RelayAddr + HostDeviceID enable relay mode (Phase 12): the connection
	// goes through the relay splice and all payloads are sealed with the
	// pairing secret so the relay cannot read them. Requires a prior
	// pairing with the host.
	RelayAddr    string
	HostDeviceID string // hex, 32 chars

	Logger logging.Logger

	// DialTimeout bounds connection establishment.
	DialTimeout time.Duration
}

// Client is the v2 receiver.
type Client struct {
	opts ClientOptions
	log  logging.Logger
	id   *pairing.Identity

	conn   *transportv2.Conn
	sess   *session.Session
	codecM sync.Mutex
	codec  codec.Codec
	mediaM sync.Mutex
	media  *clientMedia

	// Resume state (across reconnects).
	resumeToken [32]byte
	lastSeq     uint32

	// helloDone reports the handshake (hello/resume) completed on the
	// current session: async receiver announcements (RECEIVER_STATE) are
	// held back until then so the strict handshake windows stay clean.
	helloDone bool

	// Ping round-trip via the control handler (single-reader discipline).
	pingMu   sync.Mutex
	pingNext uint32
	pending  map[uint32]chan uint64

	// Live stats snapshot for UIs.
	statsMu      sync.Mutex
	stats        clientStats
	pairedHostFp string

	stop     chan struct{}
	stopOnce sync.Once
}

type clientStats struct {
	rttMs    float64
	loss     float64
	late     float64
	jitterMs float64
	bufMs    float64
	seen     uint64
	lossPk   uint64
	latePk   uint64
	reorder  uint64
	conceal  uint64
	maxBurst int
}

// NewClient creates a client.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Store == nil {
		return nil, errors.New("engine: client requires a pairing store")
	}
	if opts.HostAddr == "" {
		return nil, errors.New("engine: client requires HostAddr")
	}
	if opts.Logger == nil {
		opts.Logger = logging.Nop()
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.Name == "" {
		name, err := os.Hostname()
		if err != nil || name == "" {
			name = "remoteau-client"
		}
		opts.Name = name
	}
	return &Client{
		opts:    opts,
		log:     opts.Logger,
		stop:    make(chan struct{}),
		pending: make(map[uint32]chan uint64),
	}, nil
}

// knowsHost reports whether we already have a pairing record for the host.
func (c *Client) knowsHost(peerID [16]byte) bool {
	peers, err := c.opts.Store.Peers()
	if err != nil {
		return false
	}
	for _, p := range peers {
		if p.ID == peerID {
			return true
		}
	}
	return false
}

// identity loads or creates the local identity.
func (c *Client) ensureIdentity() error {
	id, err := c.opts.Store.LoadIdentity()
	if err != nil {
		return err
	}
	if id == nil {
		id, err = pairing.NewIdentity()
		if err != nil {
			return err
		}
		if err := c.opts.Store.SaveIdentity(id); err != nil {
			return err
		}
	}
	c.id = id
	return nil
}

// setState publishes a state transition to the OnState callback and mirrors
// it to the host as a RECEIVER_STATE message once the session is established
// (best effort; the host shows receiver lifecycle states in its UI).
func (c *Client) setState(s string) {
	if c.opts.OnState != nil {
		c.opts.OnState(s)
	}
	state, ok := receiverStateForLocal(s)
	if !ok || c.sess == nil || !c.helloDone {
		return
	}
	_ = c.SendReceiverState(state)
}

// receiverStateForLocal maps a local state string to the closest wire
// receiver state (ok=false when there is no meaningful match, e.g. errors).
func receiverStateForLocal(s string) (uint8, bool) {
	switch {
	case strings.HasPrefix(s, "reconnecting"):
		return protocolv2.ReceiverStateReconnecting, true
	case s == "connecting":
		return protocolv2.ReceiverStateConnecting, true
	case s == "streaming":
		return protocolv2.ReceiverStatePlaying, true
	case s == "idle" || s == "stopped":
		return protocolv2.ReceiverStateStopped, true
	default:
		return 0, false
	}
}

// SetQualityMode asks the host to apply a quality preset (Phase 15). The
// host remaps its adaptive controller bounds; Advanced keeps host settings.
func (c *Client) SetQualityMode(mode uint8) error {
	if c.sess == nil {
		return errors.New("engine: client is not connected")
	}
	return c.sess.SendRaw(protocolv2.Message{
		Type:    protocolv2.MsgQualityMode,
		Payload: protocolv2.AppendQualityMode(nil, protocolv2.QualityMode{Mode: mode}),
	})
}

// SetSource asks the host to switch its capture source (system default,
// named playback device, or synthetic test tone).
func (c *Client) SetSource(kind uint8, name string) error {
	if c.sess == nil {
		return errors.New("engine: client is not connected")
	}
	return c.sess.SendRaw(protocolv2.Message{
		Type:    protocolv2.MsgSetSource,
		Payload: protocolv2.AppendSetSource(nil, protocolv2.SetSource{Kind: kind, Name: name}),
	})
}

// SendReceiverState reports the receiver's lifecycle state to the host.
func (c *Client) SendReceiverState(state uint8) error {
	if c.sess == nil {
		return errors.New("engine: client is not connected")
	}
	return c.sess.SendRaw(protocolv2.Message{
		Type:    protocolv2.MsgReceiverState,
		Payload: protocolv2.AppendReceiverState(nil, protocolv2.ReceiverState{State: state}),
	})
}

// Run connects, pairs if needed, requests the stream and receives media
// until ctx is done. With Reconnect enabled it retries with exponential
// backoff and resumes previously negotiated streams when possible.
func (c *Client) Run(ctx context.Context) error {
	if err := c.ensureIdentity(); err != nil {
		return err
	}
	if !c.opts.Reconnect {
		return c.runOnce(ctx)
	}

	const (
		minDelay = 250 * time.Millisecond
		maxDelay = 5 * time.Second
		healthy  = 10 * time.Second
	)
	backoff := minDelay
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := time.Now()
		err := c.runOnce(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, pairing.ErrPinMismatch) {
			return err // terminal: wrong code is not retryable
		}
		if time.Since(start) >= healthy {
			backoff = minDelay
		}
		c.setState(fmt.Sprintf("reconnecting (%v)", backoff))
		c.log.Warnf("disconnected: %v; retrying in %s", err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stop:
			return nil
		case <-time.After(backoff):
		}
		if backoff < maxDelay {
			backoff *= 2
			if backoff > maxDelay {
				backoff = maxDelay
			}
		}
	}
}

// runOnce performs one full connection lifecycle.
func (c *Client) runOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, c.opts.DialTimeout)
	defer cancel()
	cert, err := transportv2.DeviceCertificate(c.id)
	if err != nil {
		return err
	}

	var sealer *relay.Sealer
	verify := transportv2.FingerprintVerifier(c.opts.TrustedFingerprints, len(c.opts.TrustedFingerprints) == 0)
	var conn *transportv2.Conn

	if c.opts.RelayAddr != "" {
		// Relay mode: identity is proven by the sealed control payloads (the
		// TLS peer here is the relay), so certificate pinning does not apply.
		verify = transportv2.FingerprintVerifier(nil, true)
		sealer, err = c.relaySealer()
		if err != nil {
			return err
		}
		conn, err = c.dialRelay(dialCtx, byte('P'), c.opts.HostDeviceID)
		if err != nil {
			return fmt.Errorf("relay dial: %w", err)
		}
	} else {
		conn, err = transportv2.Dial(dialCtx, c.opts.HostAddr, transportv2.TLSConfig(cert, verify))
		if err != nil {
			return err
		}
	}
	c.conn = conn
	defer func() {
		_ = conn.Close()
		c.conn = nil
	}()
	c.log.Infof("connected to %s (datagrams: %v)", c.opts.HostAddr, conn.SupportsDatagrams())

	sess, err := session.New(conn, session.RoleClient, c.id.DeviceID())
	if err != nil {
		return err
	}
	if sealer != nil {
		sess.SetSealer(sealer.Seal, sealer.Open)
	}
	c.sess = sess
	defer sess.Close()
	c.helloDone = false
	c.setState("connecting")

	clientCaps := c.opts.RequestedCaps
	if clientCaps.SampleRate == 0 {
		clientCaps = protocolv2.Caps{
			Codec:      protocolv2.CodecPCMS16LE,
			SampleRate: 48000,
			Channels:   2,
			FrameMs:    5,
		}
	}

	resumed, ack, err := c.tryResume(ctx, sess)
	if err != nil {
		return err
	}
	var activeCaps protocolv2.Caps
	if resumed {
		activeCaps = ack.Active
		c.helloDone = true
		c.log.Infof("resumed stream (fmtGen=%d)", ack.FormatGen)
	} else {
		hostHello, err := sess.ExchangeHellos(ctx, c.opts.Name, clientCaps)
		if err != nil {
			return fmt.Errorf("hello: %w", err)
		}
		c.helloDone = true
		c.resumeToken = sess.ResumeToken()
		c.log.Infof("host %q (id %x…)", hostHello.DeviceName, hostHello.DeviceID[:4])

		// Pair when we have no record of this host yet.
		if !c.knowsHost(hostHello.DeviceID) {
			if err := c.runPairing(ctx, sess); err != nil {
				return fmt.Errorf("pairing: %w", err)
			}
			// Pin the host certificate from here on.
			if fp := c.hostFingerprint(); fp != "" {
				c.pairedHostFp = fp
			}
		}

		ack, err = sess.RequestStream(ctx, clientCaps)
		if err != nil {
			return fmt.Errorf("stream start: %w", err)
		}
		activeCaps = ack.Active
	}
	c.log.Infof("stream active: %dHz %dch %dms codec=%d fmtGen=%d",
		activeCaps.SampleRate, activeCaps.Channels, activeCaps.FrameMs, activeCaps.Codec, ack.FormatGen)

	dec, err := codec.New(capsToCodecConfig(activeCaps), c.log)
	if err != nil {
		return err
	}
	c.setCodec(dec)
	c.media = newClientMedia(dec, c.opts.OnMedia)
	defer func() { _ = dec.Close() }()
	c.setState("streaming")

	// Control-plane handler: pong delivery and format updates. The session
	// has a single reader (ServeLoop); pings are matched by request ID here.
	sess.SetHandler(c.handleControl)

	// Background: media flush ticker + stats/ping loop.
	flushStop := make(chan struct{})
	go c.flushLoop(flushStop)
	statsDone := make(chan struct{})
	go func() {
		defer close(statsDone)
		c.utilityLoop(ctx)
	}()

	// Media receive loop (runs inline; it is the RT-heavy path).
	mediaErr := c.mediaLoop(ctx)
	close(flushStop)
	<-statsDone

	if ctx.Err() == nil && c.sess != nil {
		// Graceful local stop: tell the host we're done (best effort).
		_ = sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgStreamStop})
	}
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return mediaErr
	}
	return nil
}

// handleControl processes host→client control messages: pong completion and
// format updates (codec switches).
func (c *Client) handleControl(m protocolv2.Message) {
	switch m.Type {
	case protocolv2.MsgPong:
		if len(m.Payload) >= 8 {
			var sent uint64
			for i := 0; i < 8; i++ {
				sent |= uint64(m.Payload[i]) << (8 * i)
			}
			c.pingMu.Lock()
			ch, ok := c.pending[m.RequestID]
			if ok {
				delete(c.pending, m.RequestID)
			}
			c.pingMu.Unlock()
			if ok {
				select {
				case ch <- sent:
				default:
				}
			}
		}
	case protocolv2.MsgFormatUpdate:
		fu, err := protocolv2.DecodeFormatUpdate(m.Payload)
		if err != nil {
			return
		}
		// Rebuild the decoder only when the negotiated format actually
		// changes the decode configuration (bitrate/FEC/DTX/complexity are
		// encoder-side settings a decoder ignores). Always ack regardless.
		want := capsToCodecConfig(fu.Caps)
		rebuild := false
		if cur := c.currentCodec(); cur == nil {
			rebuild = true
		} else {
			have := cur.Config()
			rebuild = have.IsOpus != want.IsOpus ||
				have.SampleRate != want.SampleRate ||
				have.Channels != want.Channels ||
				have.FrameMs != want.FrameMs
		}
		if rebuild {
			dec, err := codec.New(want, c.log)
			if err != nil {
				return
			}
			c.setCodec(dec)
			c.mediaM.Lock()
			c.media = newClientMedia(dec, c.opts.OnMedia)
			c.mediaM.Unlock()
			c.log.Infof("format updated: codec=%d frameMs=%d (gen %d)", fu.Caps.Codec, fu.Caps.FrameMs, fu.FormatGen)
		} else {
			c.log.Debugf("format update gen %d: decoder config unchanged; ack only", fu.FormatGen)
		}
		_ = c.sess.SendRaw(protocolv2.Message{
			Type:    protocolv2.MsgFormatAck,
			Payload: protocolv2.AppendFormatAck(nil, protocolv2.FormatAck{FormatGen: fu.FormatGen, Applied: 1}),
		})
	default:
	}
}

// ping measures RTT via a request-ID matched PONG (single-reader safe).
func (c *Client) ping(ctx context.Context) (time.Duration, error) {
	c.pingMu.Lock()
	c.pingNext++
	id := c.pingNext
	ch := make(chan uint64, 1)
	c.pending[id] = ch
	c.pingMu.Unlock()

	defer func() {
		c.pingMu.Lock()
		delete(c.pending, id)
		c.pingMu.Unlock()
	}()

	nowUs := uint64(time.Now().UnixNano() / 1000)
	payload := make([]byte, 8)
	for i := 0; i < 8; i++ {
		payload[i] = byte(nowUs >> (8 * i))
	}
	if err := c.sess.SendRaw(protocolv2.Message{
		Type: protocolv2.MsgPing, RequestID: id, Payload: payload,
	}); err != nil {
		return 0, err
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case sent := <-ch:
		rtt := time.Since(time.UnixMicro(int64(sent)))
		return rtt, nil
	}
}

// tryResume attempts to resume a previously negotiated stream. Returns
// (resumed, ack, err); a missed resume simply falls back to the full flow.
// RESUME_OK payload: nextSeq(4) formatGen(4) caps(15).
func (c *Client) tryResume(ctx context.Context, sess *session.Session) (bool, protocolv2.StreamAck, error) {
	var zero protocolv2.StreamAck
	if c.resumeToken == ([32]byte{}) {
		return false, zero, nil
	}
	payload := make([]byte, 0, 36)
	payload = append(payload, c.resumeToken[:]...)
	payload = binaryAppendU32(payload, c.lastSeq)
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgResume, Payload: payload}); err != nil {
		return false, zero, nil // resume is best-effort
	}
	waitCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	m, err := sess.RecvRaw(waitCtx)
	if err != nil || m.Type != protocolv2.MsgResumeOK {
		return false, zero, nil
	}
	if len(m.Payload) < 23 {
		return false, zero, nil
	}
	nextSeq := uint32(m.Payload[0]) | uint32(m.Payload[1])<<8 | uint32(m.Payload[2])<<16 | uint32(m.Payload[3])<<24
	formatGen := uint32(m.Payload[4]) | uint32(m.Payload[5])<<8 | uint32(m.Payload[6])<<16 | uint32(m.Payload[7])<<24
	caps, err := protocolv2.DecodeCaps(m.Payload[8:])
	if err != nil {
		return false, zero, nil
	}
	c.lastSeq = nextSeq
	return true, protocolv2.StreamAck{Active: caps, FormatGen: formatGen}, nil
}

func binaryAppendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// hostFingerprint returns the current connection's peer certificate
// fingerprint (empty when unavailable).
func (c *Client) hostFingerprint() string {
	if c.conn == nil {
		return ""
	}
	return PeerCertFingerprint(c.conn)
}

// runPairing drives the client side of the pairing exchange.
func (c *Client) runPairing(ctx context.Context, sess *session.Session) error {
	if c.opts.PINProvider == nil {
		return errors.New("host requires pairing but no PIN provider is configured")
	}

	ex, err := pairing.NewExchange(pairing.RoleReceiver, "", c.opts.Name)
	if err != nil {
		return err
	}

	// Send PAIR_BEGIN first: the host shows its code when it arrives, then
	// the UI prompts the user to type it here.
	begin, err := ex.BeginMessage()
	if err != nil {
		return err
	}
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgPairBegin, Payload: begin}); err != nil {
		return err
	}

	pin, err := c.opts.PINProvider()
	if err != nil {
		return err
	}
	ex.SetPIN(pin)

	challenge, err := sess.RecvRaw(ctx)
	if err != nil {
		return err
	}
	if challenge.Type != protocolv2.MsgPairChallenge {
		return fmt.Errorf("expected pair-challenge, got %04x", challenge.Type)
	}
	if err := ex.OnChallenge(challenge.Payload); err != nil {
		return err
	}

	confirm, err := ex.ConfirmMessage()
	if err != nil {
		return err
	}
	if err := sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgPairConfirm, Payload: confirm}); err != nil {
		return err
	}

	result, err := sess.RecvRaw(ctx)
	if err != nil {
		return err
	}
	if result.Type != protocolv2.MsgPairResult {
		return fmt.Errorf("expected pair-result, got %04x", result.Type)
	}
	peerName, peerID, err := ex.OnResult(result.Payload)
	if err != nil {
		return err
	}
	c.log.Infof("paired with %q", peerName)

	return c.opts.Store.SavePeer(pairing.PeerRecord{
		ID:            peerID,
		Name:          peerName,
		PairingSecret: ex.Secret(),
		Fingerprint:   c.hostFingerprint(),
		PairedAt:      time.Now().Unix(),
	})
}

// flushLoop drives the media reorder window at frame pace.
func (c *Client) flushLoop(stop chan struct{}) {
	interval := 2 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-c.stop:
			return
		case <-t.C:
			c.mediaM.Lock()
			if c.media != nil {
				c.media.tick()
			}
			c.mediaM.Unlock()
		}
	}
}

// relaySealer builds the envelope sealer from the stored pairing secret for
// the configured host device.
func (c *Client) relaySealer() (*relay.Sealer, error) {
	var id [16]byte
	b, err := hex.DecodeString(strings.TrimSpace(c.opts.HostDeviceID))
	if err != nil || len(b) != 16 {
		return nil, errors.New("relay: HostDeviceID must be 32 hex chars")
	}
	copy(id[:], b)
	peers, err := c.opts.Store.Peers()
	if err != nil {
		return nil, err
	}
	for _, p := range peers {
		if p.ID == id {
			return relay.NewSealer(p.PairingSecret)
		}
	}
	return nil, errors.New("relay: no paired secret for this host; pair on the LAN first")
}

// dialRelay connects to the relay and registers (role + target id) on a
// dedicated unidirectional stream, keeping the control stream clean.
func (c *Client) dialRelay(ctx context.Context, role byte, targetIDHex string) (*transportv2.Conn, error) {
	cert, err := transportv2.DeviceCertificate(c.id)
	if err != nil {
		return nil, err
	}
	conn, err := transportv2.Dial(ctx, c.opts.RelayAddr, transportv2.TLSConfig(cert, transportv2.FingerprintVerifier(nil, true)))
	if err != nil {
		return nil, err
	}
	uni, err := conn.Inner().OpenUniStream()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("relay registration stream: %w", err)
	}
	idBytes := []byte(strings.TrimSpace(targetIDHex))
	if len(idBytes) == 0 || len(idBytes) > 64 {
		_ = conn.Close()
		return nil, errors.New("relay: bad device id")
	}
	head := []byte{role, byte(len(idBytes) >> 8), byte(len(idBytes))}
	if _, err := uni.Write(head); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := uni.Write(idBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = uni.Close()
	return conn, nil
}

// mediaLoop receives media datagrams into the reorder/PLC pipeline.
func (c *Client) mediaLoop(ctx context.Context) error {
	var sealer *relay.Sealer
	if c.opts.RelayAddr != "" {
		s, err := c.relaySealer()
		if err != nil {
			return err
		}
		sealer = s
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-c.stop:
			return nil
		default:
		}
		packet, err := c.conn.ReceiveMedia(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("receive media: %w", err)
		}
		media, err := protocolv2.DecodeMedia(packet)
		if err != nil {
			c.log.Debugf("drop media: %v", err)
			continue
		}
		payload := media.Payload
		if sealer != nil {
			plain, oerr := sealer.Open(payload)
			if oerr != nil {
				c.log.Debugf("drop sealed media: %v", oerr)
				continue
			}
			payload = plain
		}
		c.mediaM.Lock()
		if c.media != nil {
			c.lastSeq = media.Seq
			// DTX silence frames pass through as explicit concealment-free
			// silence.
			if media.Flags&protocolv2.FlagDTXSilence != 0 {
				c.media.emitSilence()
			} else {
				c.media.accept(media.Seq, media.Flags, payload)
			}
		}
		c.mediaM.Unlock()
	}
}

func (c *Client) currentCodec() codec.Codec {
	c.codecM.Lock()
	defer c.codecM.Unlock()
	return c.codec
}

func (c *Client) setCodec(dec codec.Codec) {
	c.codecM.Lock()
	c.codec = dec
	c.codecM.Unlock()
}

// utilityLoop sends real receiver statistics and measures RTT (Phase 10).
func (c *Client) utilityLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var rttMs float64
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if r, err := c.ping(pctx); err == nil {
				rttMs = float64(r.Microseconds()) / 1000.0
			}
			cancel()

			c.mediaM.Lock()
			var m *clientMedia
			if c.media != nil {
				c.media.tick() // also flush held frames between 1s ticks
				m = c.media
			}
			c.mediaM.Unlock()
			if m == nil {
				continue
			}
			loss, late, jitterMs, bufMs, _, _, _, _, _, _ := m.snapshot()
			c.statsMu.Lock()
			c.stats = clientStats{
				rttMs: rttMs, loss: loss, late: late,
				jitterMs: jitterMs, bufMs: bufMs,
			}
			c.statsMu.Unlock()

			_ = c.sess.SendStats(protocolv2.Stats{
				LossPct:    uint16(clampF64(loss*100, 0, 65535)),
				LatePct:    uint16(clampF64(late*100, 0, 65535)),
				JitterUs:   uint32(clampF64(jitterMs*1000, 0, 4294967295)),
				RTTUs:      uint32(clampF64(rttMs*1000, 0, 4294967295)),
				BufDepthMs: uint16(clampF64(bufMs, 0, 65535)),
				Underruns:  0, // underruns are receiver-render-side; surfaced by UI layers
			})
		}
	}
}

func clampF64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Stats returns the latest measured stats snapshot.
func (c *Client) Stats() clientStats {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	return c.stats
}

// Stop terminates the client (sends STREAM_STOP best-effort via the session
// teardown).
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
	})
	if c.sess != nil {
		_ = c.sess.SendRaw(protocolv2.Message{Type: protocolv2.MsgStreamStop})
		c.sess.Close()
	}
}

// capsToCodecConfig maps negotiated caps to a codec config.
func capsToCodecConfig(caps protocolv2.Caps) codec.Config {
	return codec.Config{
		IsOpus:     caps.Codec == protocolv2.CodecOpus,
		SampleRate: int(caps.SampleRate),
		Channels:   int(caps.Channels),
		FrameMs:    int(caps.FrameMs),
		Bitrate:    int(caps.OpusBitrate),
		FEC:        caps.FEC == 1,
		DTX:        caps.DTX == 1,
		Complexity: int(caps.Complexity),
		AppID:      int(caps.AppID),
	}
}
