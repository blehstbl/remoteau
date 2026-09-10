// Package session implements the RemoteAU v2 control-plane state machine on
// top of the QUIC control stream: hello/capability exchange, pairing,
// stream start/stop, format renegotiation, stats and volume.
package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
	"remote-au/internal/transport/v2"
)

// Role selects which side of the session this instance is.
type Role int

const (
	RoleHost   Role = iota // Windows: produces audio
	RoleClient             // iPhone: consumes audio
)

// Errors surfaced to callers for UI handling.
var (
	ErrClosed      = errors.New("session closed")
	ErrNotPaired   = errors.New("device is not paired")
	ErrPinMismatch = pairing.ErrPinMismatch
	ErrUnsupported = errors.New("no supported format negotiated")
)

// Handler receives control messages that the role-specific loop does not
// consume itself (e.g. STATS at the host, VOLUME at the host).
type Handler func(m protocolv2.Message)

// Session runs the control protocol over an established transport conn.
type Session struct {
	conn *transportv2.Conn
	ctrl *transportv2.ControlStream
	br   *bufio.Reader
	bw   *bufio.Writer
	role Role

	mu       sync.Mutex
	handler  Handler
	closed   chan struct{}
	peerName string
	peerID   [16]byte
	localID  [16]byte

	localDeviceName string

	// Relay-mode envelope protection (nil = direct connection).
	seal func([]byte) ([]byte, error)
	open func([]byte) ([]byte, error)

	// Negotiated stream parameters (host side fills on STREAM_ACK).
	activeCaps  protocolv2.Caps
	formatGen   uint32
	resumeToken [32]byte
}

// New wraps an established connection. Exactly one side must call
// OpenControlStream, the other AcceptControlStream — determined by role
// (client opens, host accepts).
func New(conn *transportv2.Conn, role Role, localDeviceID [16]byte) (*Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ctrl *transportv2.ControlStream
	var err error
	if role == RoleClient {
		ctrl, err = conn.OpenControlStream(ctx)
	} else {
		ctrl, err = conn.AcceptControlStream(ctx)
	}
	if err != nil {
		return nil, err
	}
	s := &Session{
		conn:    conn,
		ctrl:    ctrl,
		br:      bufio.NewReader(ctrl.Raw()),
		bw:      bufio.NewWriter(ctrl.Raw()),
		role:    role,
		closed:  make(chan struct{}),
		localID: localDeviceID,
	}
	return s, nil
}

// SetHandler registers a handler for messages not consumed internally.
func (s *Session) SetHandler(h Handler) {
	s.mu.Lock()
	s.handler = h
	s.mu.Unlock()
}

// SetSealer enables relay-mode envelope protection: every control payload is
// sealed/unsealed with an AEAD keyed from the pairing secret, so a relay
// cannot read application data (Phase 12).
func (s *Session) SetSealer(seal func([]byte) ([]byte, error), open func([]byte) ([]byte, error)) {
	s.mu.Lock()
	s.seal = seal
	s.open = open
	s.mu.Unlock()
}

// SetLocalName records this device's name (included in the host's HELLO).
func (s *Session) SetLocalName(name string) {
	s.mu.Lock()
	s.localDeviceName = name
	s.mu.Unlock()
}

// PeerName returns the remote device name once known.
func (s *Session) PeerName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peerName
}

// PeerID returns the remote device ID once known.
func (s *Session) PeerID() [16]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peerID
}

// PeerIDString returns the remote device ID as hex ("" before hello).
func (s *Session) PeerIDString() string {
	id := s.PeerID()
	if id == ([16]byte{}) {
		return ""
	}
	return fmt.Sprintf("%x", id)
}

// Conn exposes the underlying transport connection.
func (s *Session) Conn() *transportv2.Conn { return s.conn }

// SendRaw writes one control message (used by the pairing flow).
func (s *Session) SendRaw(m protocolv2.Message) error { return s.send(m) }

// RecvRaw reads the next control message (used by the pairing flow).
func (s *Session) RecvRaw(ctx context.Context) (protocolv2.Message, error) { return s.recv(ctx) }

// ActiveCaps returns the negotiated stream format.
func (s *Session) ActiveCaps() protocolv2.Caps {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCaps
}

// Close tears the session down.
func (s *Session) Close() {
	select {
	case <-s.closed:
		return
	default:
	}
	close(s.closed)
	_ = s.conn.Close()
}

func (s *Session) send(m protocolv2.Message) error {
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seal != nil && len(m.Payload) > 0 {
		sealed, err := s.seal(m.Payload)
		if err != nil {
			return fmt.Errorf("seal: %w", err)
		}
		m.Payload = sealed
	}
	return protocolv2.WriteMessage(s.bw, m)
}

// recv reads the next message, treating EOF/conn close as ErrClosed.
func (s *Session) recv(ctx context.Context) (protocolv2.Message, error) {
	type result struct {
		m   protocolv2.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m, err := protocolv2.ReadMessage(s.br)
		ch <- result{m, err}
	}()
	select {
	case <-ctx.Done():
		return protocolv2.Message{}, ctx.Err()
	case <-s.closed:
		return protocolv2.Message{}, ErrClosed
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				return r.m, ErrClosed
			}
			return r.m, r.err
		}
		if s.open != nil && len(r.m.Payload) > 0 {
			plain, oerr := s.open(r.m.Payload)
			if oerr != nil {
				return protocolv2.Message{}, fmt.Errorf("open control envelope: %w", oerr)
			}
			r.m.Payload = plain
		}
		return r.m, nil
	}
}

// ExchangeHellos performs the initial HELLO/HELLO_OK handshake. offered lists
// the formats this host can produce (RoleHost) or consume (RoleClient).
func (s *Session) ExchangeHellos(ctx context.Context, name string, offered protocolv2.Caps) (protocolv2.Hello, error) {
	if err := s.SendHello(name, offered); err != nil {
		return protocolv2.Hello{}, err
	}
	m, err := s.recv(ctx)
	if err != nil {
		return protocolv2.Hello{}, err
	}
	if s.role == RoleHost {
		if m.Type != protocolv2.MsgHello {
			return protocolv2.Hello{}, fmt.Errorf("expected hello, got %04x", m.Type)
		}
		return s.CompleteHello(m, offered)
	}
	// Client role: expect the host's HELLO...
	if m.Type != protocolv2.MsgHello {
		return protocolv2.Hello{}, fmt.Errorf("expected hello, got %04x", m.Type)
	}
	peerHello, err := protocolv2.DecodeHello(m.Payload)
	if err != nil {
		return protocolv2.Hello{}, fmt.Errorf("decode hello: %w", err)
	}
	s.mu.Lock()
	s.peerName = peerHello.DeviceName
	s.peerID = peerHello.DeviceID
	s.mu.Unlock()

	okMsg, err := s.recv(ctx)
	if err != nil {
		return peerHello, err
	}
	if okMsg.Type != protocolv2.MsgHelloOK {
		return peerHello, fmt.Errorf("expected hello-ok, got %04x", okMsg.Type)
	}
	okPayload, err := protocolv2.DecodeHelloOK(okMsg.Payload)
	if err != nil {
		return peerHello, fmt.Errorf("decode hello-ok: %w", err)
	}
	s.mu.Lock()
	s.activeCaps = okPayload.Profile
	s.resumeToken = okPayload.ResumeToken
	s.mu.Unlock()
	return peerHello, nil
}

// SendHello sends this side's HELLO (host sends first so the engine can
// interleave RESUME handling).
func (s *Session) SendHello(name string, offered protocolv2.Caps) error {
	return s.send(protocolv2.Message{
		Type: protocolv2.MsgHello,
		Payload: protocolv2.AppendHello(nil, protocolv2.Hello{
			ProtoVersion: protocolv2.Version,
			MinProto:     protocolv2.Version,
			DeviceName:   name,
			DeviceID:     s.localID,
			Caps:         offered,
		}),
	})
}

// CompleteHello processes a received HELLO on the host: records the peer,
// answers with the host's own HELLO plus HELLO_OK (selected profile and a
// resume token).
func (s *Session) CompleteHello(m protocolv2.Message, offered protocolv2.Caps) (protocolv2.Hello, error) {
	if s.role != RoleHost {
		return protocolv2.Hello{}, errors.New("only the host completes hello")
	}
	if m.Type != protocolv2.MsgHello {
		return protocolv2.Hello{}, fmt.Errorf("expected hello, got %04x", m.Type)
	}
	peerHello, err := protocolv2.DecodeHello(m.Payload)
	if err != nil {
		return protocolv2.Hello{}, fmt.Errorf("decode hello: %w", err)
	}
	s.mu.Lock()
	s.peerName = peerHello.DeviceName
	s.peerID = peerHello.DeviceID
	s.mu.Unlock()

	selected, ok := selectProfile(offered, peerHello.Caps)
	if !ok {
		_ = s.send(protocolv2.Message{Type: protocolv2.MsgHelloOK})
		return peerHello, ErrUnsupported
	}
	var token [32]byte
	_, _ = rand.Read(token[:])
	s.mu.Lock()
	s.activeCaps = selected
	s.resumeToken = token
	active := s.activeCaps
	s.mu.Unlock()

	// Host's own HELLO (device identity), then HELLO_OK.
	if err := s.send(protocolv2.Message{
		Type: protocolv2.MsgHello,
		Payload: protocolv2.AppendHello(nil, protocolv2.Hello{
			ProtoVersion: protocolv2.Version,
			MinProto:     protocolv2.Version,
			DeviceName:   s.localName(),
			DeviceID:     s.localID,
			Caps:         offered,
		}),
	}); err != nil {
		return peerHello, err
	}
	if err := s.send(protocolv2.Message{
		Type:    protocolv2.MsgHelloOK,
		Payload: protocolv2.AppendHelloOK(nil, protocolv2.HelloOK{Profile: active, ResumeToken: token}),
	}); err != nil {
		return peerHello, err
	}
	return peerHello, nil
}

// localName returns the host device name (set via SetLocalName).
func (s *Session) localName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localDeviceName
}

// ResumeToken returns the token assigned by the host (host role after
// CompleteHello, client role after ExchangeHellos).
func (s *Session) ResumeToken() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeToken
}

// RequestStream (client) asks the host to start streaming the given format.
func (s *Session) RequestStream(ctx context.Context, request protocolv2.Caps) (protocolv2.StreamAck, error) {
	if s.role != RoleClient {
		return protocolv2.StreamAck{}, errors.New("only the client requests streams")
	}
	if err := s.send(protocolv2.Message{
		Type:    protocolv2.MsgStreamStart,
		Payload: request.Append(nil),
	}); err != nil {
		return protocolv2.StreamAck{}, err
	}
	m, err := s.recv(ctx)
	if err != nil {
		return protocolv2.StreamAck{}, err
	}
	if m.Type != protocolv2.MsgStreamAck {
		return protocolv2.StreamAck{}, fmt.Errorf("expected stream-ack, got %04x", m.Type)
	}
	ack, err := protocolv2.DecodeStreamAck(m.Payload)
	if err != nil {
		return protocolv2.StreamAck{}, err
	}
	s.mu.Lock()
	s.activeCaps = ack.Active
	s.formatGen = ack.FormatGen
	s.mu.Unlock()
	return ack, nil
}

// StartStream (host) waits for STREAM_START and answers with STREAM_ACK.
func (s *Session) StartStream(ctx context.Context, producer func(request protocolv2.Caps) (protocolv2.Caps, uint32, error)) error {
	if s.role != RoleHost {
		return errors.New("only the host accepts stream starts")
	}
	m, err := s.recv(ctx)
	if err != nil {
		return err
	}
	if m.Type != protocolv2.MsgStreamStart {
		return fmt.Errorf("expected stream-start, got %04x", m.Type)
	}
	req, err := protocolv2.DecodeCaps(m.Payload)
	if err != nil {
		return fmt.Errorf("decode stream-start caps: %w", err)
	}
	active, gen, err := producer(req)
	if err != nil {
		// Answer with an empty-ack to signal failure, then propagate.
		_ = s.send(protocolv2.Message{Type: protocolv2.MsgStreamAck})
		return err
	}
	s.mu.Lock()
	s.activeCaps = active
	s.formatGen = gen
	s.mu.Unlock()
	return s.send(protocolv2.Message{
		Type:    protocolv2.MsgStreamAck,
		Payload: protocolv2.AppendStreamAck(nil, protocolv2.StreamAck{Active: active, FormatGen: gen}),
	})
}

// SendStats (client) reports receiver-side network/buffer statistics.
func (s *Session) SendStats(st protocolv2.Stats) error {
	return s.send(protocolv2.Message{
		Type:    protocolv2.MsgStats,
		Payload: protocolv2.AppendStats(nil, st),
	})
}

// SendVolume (client) sets host output volume/mute (reserved for desktop
// receivers; iPhone keeps device volume).
func (s *Session) SendVolume(v protocolv2.Volume) error {
	return s.send(protocolv2.Message{
		Type:    protocolv2.MsgVolume,
		Payload: append([]byte{}, byte(v.VolumeQ16&0xFF), byte(v.VolumeQ16>>8), v.Mute),
	})
}

// ServeLoop reads messages until close, dispatching to the registered
// handler. Internal types (PONG, FORMAT_ACK) are answered inline where the
// role expects it.
func (s *Session) ServeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.closed:
			return nil
		default:
		}
		m, err := s.recv(ctx)
		if err != nil {
			if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		h := s.handler
		s.mu.Unlock()
		if h != nil {
			h(m)
		}
	}
}

// selectProfile picks the first offered format the peer can accept.
func selectProfile(offered, peer protocolv2.Caps) (protocolv2.Caps, bool) {
	if offered.SampleRate != peer.SampleRate {
		return protocolv2.Caps{}, false
	}
	if offered.Channels != peer.Channels {
		return protocolv2.Caps{}, false
	}
	// Codec: both must agree; prefer the peer's codec if supported.
	codec := offered.Codec
	if peer.Codec != offered.Codec {
		// Fall back to PCM when in doubt.
		codec = protocolv2.CodecPCMS16LE
	}
	out := offered
	out.Codec = codec
	out.FrameMs = peer.FrameMs
	if out.FrameMs == 0 {
		out.FrameMs = 10
	}
	return out, true
}
