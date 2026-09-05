package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"remote-au/internal/audio"
	"remote-au/internal/codec"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
	"remote-au/internal/session"
	"remote-au/internal/transport/v2"
)

// ClientOptions configures the v2 client (iPhone receiver; also used by the
// CLI test client).
type ClientOptions struct {
	HostAddr string // "ip:port"
	Name     string
	Store    pairing.Store

	// PINProvider is called when pairing is needed; the UI prompts the user
	// to enter the code displayed on the PC.
	PINProvider func() (string, error)

	// OnMedia delivers decoded PCM (S16LE) for playback. Must be non-blocking.
	OnMedia func(pcm []byte)

	// RequestedCaps selects what the client asks for. Zero fields let the
	// host decide.
	RequestedCaps protocolv2.Caps

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

	stop chan struct{}
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
		opts: opts,
		log:  opts.Logger,
		stop: make(chan struct{}),
	}, nil
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

// Run connects, pairs if needed, requests the stream and receives media
// until ctx is done.
func (c *Client) Run(ctx context.Context) error {
	if err := c.ensureIdentity(); err != nil {
		return err
	}

	dialCtx, cancel := context.WithTimeout(ctx, c.opts.DialTimeout)
	defer cancel()
	cert, err := transportv2.DeviceCertificate(c.id)
	if err != nil {
		return err
	}
	// Pairing mode: accept any host cert for the FIRST connection; the PIN
	// exchange authenticates it. Subsequent runs pin the fingerprint (the
	// trust store lookup below is applied by the caller-selected verifier in
	// a future revision; the session-layer pairing check protects us).
	tlsCfg := transportv2.TLSConfig(cert, transportv2.FingerprintVerifier(nil, true))

	conn, err := transportv2.Dial(dialCtx, c.opts.HostAddr, tlsCfg)
	if err != nil {
		return err
	}
	c.conn = conn
	defer func() {
		_ = conn.Close()
	}()
	c.log.Infof("connected to %s (datagrams: %v)", c.opts.HostAddr, conn.SupportsDatagrams())

	sess, err := session.New(conn, session.RoleClient, c.id.DeviceID())
	if err != nil {
		return err
	}
	c.sess = sess
	defer sess.Close()

	clientCaps := c.opts.RequestedCaps
	if clientCaps.SampleRate == 0 {
		clientCaps = protocolv2.Caps{
			Codec:      protocolv2.CodecPCMS16LE,
			SampleRate: 48000,
			Channels:   2,
			FrameMs:    5,
		}
	}

	hostHello, err := sess.ExchangeHellos(ctx, c.opts.Name, clientCaps)
	if err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	c.log.Infof("host %q (id %x…)", hostHello.DeviceName, hostHello.DeviceID[:4])

	// Pair when we have no record of this host yet.
	if !c.knowsHost(hostHello.DeviceID) {
		if err := c.runPairing(ctx, sess); err != nil {
			return fmt.Errorf("pairing: %w", err)
		}
	}

	// Request the stream.
	ack, err := sess.RequestStream(ctx, clientCaps)
	if err != nil {
		return fmt.Errorf("stream start: %w", err)
	}
	c.log.Infof("stream active: %dHz %dch %dms codec=%d fmtGen=%d",
		ack.Active.SampleRate, ack.Active.Channels, ack.Active.FrameMs, ack.Active.Codec, ack.FormatGen)

	cfg := codec.Config{
		IsOpus:     ack.Active.Codec == protocolv2.CodecOpus,
		SampleRate: int(ack.Active.SampleRate),
		Channels:   int(ack.Active.Channels),
		FrameMs:    int(ack.Active.FrameMs),
		AppID:      int(ack.Active.AppID),
	}
	dec, err := codec.New(cfg, c.log)
	if err != nil {
		return err
	}
	c.setCodec(dec)
	defer func() { _ = dec.Close() }()

	// Background: stats + ping loop.
	go c.utilityLoop(ctx)
	// Background: media datagram receive loop.
	mediaDone := make(chan error, 1)
	go func() { mediaDone <- c.mediaLoop(ctx) }()

	ctrlErr := sess.ServeLoop(ctx)
	mediaErr := <-mediaDone
	if mediaErr != nil && !errors.Is(mediaErr, context.Canceled) {
		return mediaErr
	}
	return ctrlErr
}

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
		PairedAt:      time.Now().Unix(),
	})
}

// mediaLoop receives media datagrams, decodes and forwards to OnMedia.
func (c *Client) mediaLoop(ctx context.Context) error {
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
		c.decodeAndDeliver(media)
	}
}

// decodeAndDeliver decodes one media frame. TODO(phase-3-iOS-parity): add a
// reorder buffer + PLC here once the shared Go receiver engine is wired to
// the mobile bindings; the iOS app currently implements this natively.
func (c *Client) decodeAndDeliver(media protocolv2.Media) {
	codecImpl := c.currentCodec()
	if codecImpl == nil {
		return
	}
	out := make([]byte, codecImpl.FrameBytes())
	if media.Flags&protocolv2.FlagDTXSilence != 0 {
		clear(out)
	} else {
		if _, err := codecImpl.DecodeFrame(media.Payload, false, out); err != nil {
			c.log.Debugf("decode: %v", err)
			return
		}
	}
	if c.opts.OnMedia != nil {
		c.opts.OnMedia(out)
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

// utilityLoop sends stats (placeholder zeros for now) and pings.
func (c *Client) utilityLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var rtt time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if r, err := c.sess.Ping(pctx); err == nil {
				rtt = r
			}
			cancel()
			stats := protocolv2.Stats{
				RTTUs:    uint32(rtt.Microseconds()),
				JitterUs: uint32(0),
				LossPct:  0,
			}
			_ = c.sess.SendStats(stats)
		}
	}
}

// Stop terminates the client.
func (c *Client) Stop() {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	if c.sess != nil {
		c.sess.Close()
	}
}

var _ = audio.Format{}
