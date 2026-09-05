package engine

import (
	"crypto/sha256"
	"encoding/hex"

	"remote-au/internal/transport/v2"
)

// PeerCertFingerprint returns the SHA-256 fingerprint (hex) of the remote
// peer's TLS certificate, or "" when unavailable.
func PeerCertFingerprint(conn *transportv2.Conn) string {
	if conn == nil {
		return ""
	}
	state := conn.Inner().ConnectionState()
	if len(state.TLS.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(state.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}
