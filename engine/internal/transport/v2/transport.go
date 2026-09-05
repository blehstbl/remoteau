package transportv2

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// DefaultPort is the v2 listen port.
const DefaultPort = 47010

// Conn wraps a QUIC connection plus its transport (so the socket outlives
// individual calls) with the v2 helpers: one control stream + media
// datagrams.
type Conn struct {
	inner *quic.Conn
	tr    *quic.Transport
}

// Inner exposes the raw QUIC connection.
func (c *Conn) Inner() *quic.Conn { return c.inner }

// Close closes the QUIC connection and its transport socket (when owned).
func (c *Conn) Close() error {
	err := c.inner.CloseWithError(0, "bye")
	if c.tr != nil {
		_ = c.tr.Close()
	}
	return err
}

// OpenControlStream opens the (single) bidirectional control stream.
func (c *Conn) OpenControlStream(ctx context.Context) (*ControlStream, error) {
	raw, err := c.inner.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	return &ControlStream{raw: raw}, nil
}

// AcceptControlStream accepts the peer's control stream.
func (c *Conn) AcceptControlStream(ctx context.Context) (*ControlStream, error) {
	raw, err := c.inner.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("accept control stream: %w", err)
	}
	return &ControlStream{raw: raw}, nil
}

// SendMedia sends one media datagram (unreliable).
func (c *Conn) SendMedia(payload []byte) error {
	return c.inner.SendDatagram(payload)
}

// ReceiveMedia receives one media datagram (unreliable).
func (c *Conn) ReceiveMedia(ctx context.Context) ([]byte, error) {
	return c.inner.ReceiveDatagram(ctx)
}

// SupportsDatagrams reports whether both ends negotiated datagrams.
func (c *Conn) SupportsDatagrams() bool {
	cs := c.inner.ConnectionState()
	return cs.SupportsDatagrams.Local && cs.SupportsDatagrams.Remote
}

// ControlStream is the reliable ordered control channel.
type ControlStream struct {
	raw *quic.Stream
}

// Raw exposes the underlying QUIC stream.
func (s *ControlStream) Raw() *quic.Stream { return s.raw }

// Dial connects to a v2 endpoint over a fresh ephemeral UDP socket.
func Dial(ctx context.Context, addr string, tlsCfg *tls.Config) (*Conn, error) {
	remote, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("local udp: %w", err)
	}
	tr := &quic.Transport{Conn: local}
	inner, err := tr.Dial(ctx, remote, tlsCfg, &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  15 * time.Second,
		KeepAlivePeriod: 2 * time.Second,
	})
	if err != nil {
		_ = tr.Close()
		return nil, fmt.Errorf("quic dial %s: %w", addr, err)
	}
	return &Conn{inner: inner, tr: tr}, nil
}

// Listener accepts incoming v2 connections on a bound UDP address.
type Listener struct {
	tr    *quic.Transport
	inner *quic.Listener
}

// Listen binds addr (e.g. ":47010") and starts accepting.
func Listen(addr string, tlsCfg *tls.Config) (*Listener, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", addr, err)
	}
	tr := &quic.Transport{Conn: conn}
	inner, err := tr.Listen(tlsCfg, &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 5 * time.Second,
	})
	if err != nil {
		_ = tr.Close()
		return nil, fmt.Errorf("quic listen %s: %w", addr, err)
	}
	return &Listener{tr: tr, inner: inner}, nil
}

// Accept waits for the next connection.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	inner, err := l.inner.Accept(ctx)
	if err != nil {
		return nil, err
	}
	// Connections accepted by this listener share the listener transport; a
	// nil tr marks them as not owning one.
	return &Conn{inner: inner}, nil
}

// Addr returns the local listen address.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Close shuts the listener down.
func (l *Listener) Close() error {
	err := l.inner.Close()
	_ = l.tr.Close()
	return err
}
