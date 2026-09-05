// Package transportv2 implements the QUIC + QUIC-DATAGRAM transport for
// RemoteAU v2 (RFC 9000 + RFC 9221), with device-identity TLS: both sides
// present self-signed certificates derived from their long-term pairing
// identity; peers are authenticated by certificate fingerprint against the
// trust store.
package transportv2

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"remote-au/internal/pairing"
)
// CertFingerprint returns the SHA-256 fingerprint (hex) of a certificate's
// raw DER.
func CertFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// DeviceCertificate builds the self-signed TLS certificate for an identity.
// The certificate is self-signed with the identity key so its fingerprint is
// stable and pinnable after pairing.
func DeviceCertificate(id *pairing.Identity) (tls.Certificate, error) {
	if id == nil || id.Private == nil {
		return tls.Certificate{}, errors.New("nil identity")
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "remoteau-device",
			Organization: []string{"RemoteAU"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &id.Private.PublicKey, id.Private)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create device certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  id.Private,
		Leaf:        leaf,
	}, nil
}

// FingerprintVerifier returns a TLS verification function that accepts only
// peers whose certificate fingerprint is in `trusted` (hex, lowercase). When
// `trustAny` is set (pairing mode), any peer certificate is accepted — the
// exchange itself is protected by the PIN.
func FingerprintVerifier(trusted map[string]bool, trustAny bool) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("peer sent no certificate")
		}
		sum := sha256.Sum256(rawCerts[0])
		fp := hex.EncodeToString(sum[:])
		if trustAny || trusted[fp] {
			return nil
		}
		return fmt.Errorf("peer certificate %s is not a paired device", fp[:16])
	}
}

// TLSConfig builds the mutual-TLS configuration for a QUIC endpoint. Both
// roles present their device certificate; identity is verified by
// fingerprint, not the WebPKI.
func TLSConfig(cert tls.Certificate, verify func([][]byte, [][]*x509.Certificate) error) *tls.Config {
	return &tls.Config{
		Certificates:          []tls.Certificate{cert},
		NextProtos:            []string{"remoteau/2"},
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, // fingerprint verification below
		VerifyPeerCertificate: verify,
		ClientAuth:            tls.RequireAnyClientCert,
	}
}
