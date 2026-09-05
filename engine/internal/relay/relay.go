// Package relay implements the RemoteAU v2 relay fallback (Phase 12): a
// small process that splices two QUIC connections (phone ↔ PC) without
// interpreting the application protocol.
//
// Blindness: in relay mode the endpoints wrap every control payload and
// media payload in an AEAD envelope keyed from the pairing secret
// (HKDF-SHA256), so the relay only ever sees ciphertext inside TLS it
// terminates. Enrollment: the PC registers as "H:<hostDeviceIDHex>" and the
// phone as "P:<hostDeviceIDHex>"; the relay pairs the two and splices.
package relay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"

	"remote-au/internal/transport/v2"
)

// Logger receives relay diagnostics lines.
type Logger func(line string)

// RunRelay is the relay entrypoint.
func RunRelay(ctx context.Context, addr string, tlsCfg *tls.Config, logger Logger) error {
	ln, err := transportv2.Listen(addr, tlsCfg)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	if logger != nil {
		logger("relay listening on " + addr)
	}

type half struct {
	role byte // 'H' or 'P'
	id   string
	conn *transportv2.Conn
	// paired is closed by the phone side when the splice begins.
	paired chan struct{}
}

	waiting := make(map[string]*half) // keyed by hostDeviceID (phone side) or "H" (host side)
	var mu sync.Mutex

	for {
		if ctx.Err() != nil {
			return nil
		}
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		go func(conn *transportv2.Conn) {
			defer func() { _ = conn.Close() }()

			// Registration arrives on a dedicated unidirectional stream; the
			// bidirectional control stream stays clean for session data.
			regCtx, regCancel := context.WithTimeout(ctx, registrationTime)
			defer regCancel()
			uni, err := conn.Inner().AcceptUniStream(regCtx)
			if err != nil {
				return
			}
			head := make([]byte, 3)
			if _, err := io.ReadFull(uni, head); err != nil {
				return
			}
			role := head[0]
			idLen := int(head[1])<<8 | int(head[2])
			if idLen <= 0 || idLen > 64 || (role != 'H' && role != 'P') {
				return
			}
			idBytes := make([]byte, idLen)
			if _, err := io.ReadFull(uni, idBytes); err != nil {
				return
			}
			id := strings.TrimSpace(string(idBytes))
			uni.CancelRead(0)
			if logger != nil {
				logger(fmt.Sprintf("registered role=%c id=%s", role, id))
			}

			if role == 'H' {
				hf := &half{role: role, id: id, conn: conn, paired: make(chan struct{})}
				mu.Lock()
				waiting["H:"+id] = hf
				mu.Unlock()
				if logger != nil {
					logger(fmt.Sprintf("host %s waiting for a phone", id))
				}
				// Block until a phone picks this host up, then keep this
				// goroutine alive: returning would fire the deferred
				// conn.Close() and kill the host leg. The phone's goroutine
				// performs the splice and owns teardown.
				select {
				case <-hf.paired:
					<-ctx.Done()
					return
				case <-ctx.Done():
					return
				}
			}
			mu.Lock()
			peer := waiting["H:"+id]
			if peer != nil {
				delete(waiting, "H:"+id)
			}
			mu.Unlock()
			if peer != nil {
				close(peer.paired)
			}

			if role == 'H' {
				// Hosts wait for a phone to pick them up.
				return
			}
			if peer == nil {
				return
			}

			// Splice: media datagrams both ways + control streams both ways.
			// The phone OPENS its control stream (client role) and the host
			// ACCEPTS one (host role), so the relay opens toward the host and
			// accepts from the phone.
			if logger != nil {
				logger("relay: paired phone with host " + id)
			}
			done := make(chan struct{}, 4)
			go func() {
				pHostCtrl, err1 := peer.conn.OpenControlStream(ctx)
				if err1 != nil {
					done <- struct{}{}
					done <- struct{}{}
					return
				}
				pPhoneCtrl, err2 := conn.AcceptControlStream(ctx)
				if err2 != nil {
					done <- struct{}{}
					done <- struct{}{}
					return
				}
				go spliceControl(pHostCtrl, pPhoneCtrl, done)
				go spliceControl(pPhoneCtrl, pHostCtrl, done)
			}()
			go spliceMedia(peer.conn, conn, done)
			go spliceMedia(conn, peer.conn, done)
			<-done
			<-done
			<-done
			<-done
		}(conn)
	}
}

func spliceControl(dst, src *transportv2.ControlStream, done chan struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 8192)
	for {
		n, err := src.Raw().Read(buf)
		if n > 0 {
			if _, werr := dst.Raw().Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func spliceMedia(dst, src *transportv2.Conn, done chan struct{}) {
	defer func() { done <- struct{}{} }()
	ctx := context.Background()
	for {
		dgram, err := src.ReceiveMedia(ctx)
		if err != nil {
			return
		}
		if err := dst.SendMedia(dgram); err != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Envelope AEAD (used by the endpoints in relay mode)

// Sealer wraps payloads so a relay cannot read them. Key material is the
// pairing secret both sides already persisted.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer derives the relay envelope key from the pairing secret.
func NewSealer(pairingSecret []byte) (*Sealer, error) {
	if len(pairingSecret) < 32 {
		return nil, errors.New("relay: pairing secret too short")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, pairingSecret, []byte("remoteau-relay"), []byte("rau2-relay-envelope-v1")), key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal wraps a payload: nonce | ciphertext+tag.
func (s *Sealer) Seal(plain []byte) ([]byte, error) {
	return s.SealAAD(nil, plain)
}

// Open unwraps a sealed payload.
func (s *Sealer) Open(env []byte) ([]byte, error) {
	return s.OpenAAD(nil, env)
}

// SealAAD wraps a payload binding additional authenticated data.
func (s *Sealer) SealAAD(aad, plain []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, aad), nil
}

// OpenAAD unwraps a sealed payload with additional authenticated data.
func (s *Sealer) OpenAAD(aad, env []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(env) < ns {
		return nil, errors.New("relay: envelope too short")
	}
	return s.aead.Open(nil, env[:ns], env[ns:], aad)
}

// u64 helper kept for protocol symmetry with the session layer.
func appendU64(dst []byte, v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return append(dst, b[:]...)
}

const registrationTime = 10 * time.Second
