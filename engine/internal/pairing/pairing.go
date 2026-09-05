// Package pairing implements RemoteAU v2 device pairing and trusted-device
// identity: long-term P-256 identity keys, PIN-verified ECDH pairing, and
// per-platform secure trust stores (DPAPI on Windows, Keychain on iOS via the
// mobile bindings).
package pairing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
)

// Identity is a device's long-term identity key.
type Identity struct {
	Private *ecdsa.PrivateKey
}

// NewIdentity generates a fresh identity key.
func NewIdentity() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate identity key: %w", err)
	}
	return &Identity{Private: key}, nil
}

// LoadIdentity parses a PKCS#8 private key blob.
func LoadIdentity(pkcs8 []byte) (*Identity, error) {
	key, err := x509.ParsePKCS8PrivateKey(pkcs8)
	if err != nil {
		return nil, fmt.Errorf("parse identity key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("identity key is not ECDSA")
	}
	if ecKey.Curve != elliptic.P256() {
		return nil, errors.New("identity key is not P-256")
	}
	return &Identity{Private: ecKey}, nil
}

// MarshalPrivate returns the PKCS#8 encoding (for storage).
func (id *Identity) MarshalPrivate() ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(id.Private)
}

// PublicKeyDER returns the uncompressed public key (65-byte point).
func (id *Identity) PublicKeyDER() []byte {
	return elliptic.Marshal(elliptic.P256(), id.Private.PublicKey.X, id.Private.PublicKey.Y)
}

// Fingerprint returns the hex SHA-256 of the public point (device identity
// fingerprint used for trust-on-first-pairing).
func (id *Identity) Fingerprint() string {
	sum := sha256.Sum256(id.PublicKeyDER())
	return hex.EncodeToString(sum[:])
}

// DeviceID returns the stable 16-byte device identifier (first half of the
// fingerprint hash).
func (id *Identity) DeviceID() [16]byte {
	sum := sha256.Sum256(id.PublicKeyDER())
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

// PeerRecord is a paired peer persisted in the trust store.
type PeerRecord struct {
	ID            [16]byte `json:"id"`
	Name          string   `json:"name"`
	PairingSecret []byte   `json:"pairing_secret"` // 32 bytes
	Fingerprint   string   `json:"fingerprint"`    // peer identity fingerprint (hex)
	PairedAt      int64    `json:"paired_at"`      // unix seconds
}

// Store persists the local identity and paired peers securely.
type Store interface {
	// LoadIdentity returns the stored identity, or nil when none exists.
	LoadIdentity() (*Identity, error)
	// SaveIdentity stores (or replaces) the local identity.
	SaveIdentity(id *Identity) error
	// Peers lists all paired peers.
	Peers() ([]PeerRecord, error)
	// SavePeer inserts or updates a peer record.
	SavePeer(PeerRecord) error
	// RemovePeer forgets a peer by ID.
	RemovePeer(id [16]byte) error
}
