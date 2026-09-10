package engine

import (
	"remote-au/internal/transport/v2"
)

// PeerCertFingerprint returns the stable public-key fingerprint (hex) of the
// remote peer's TLS certificate, or "" when unavailable. It uses the public
// key (not the certificate DER) so it survives certificate regeneration
// across restarts, which device pinning requires.
func PeerCertFingerprint(conn *transportv2.Conn) string {
	if conn == nil {
		return ""
	}
	state := conn.Inner().ConnectionState()
	if len(state.TLS.PeerCertificates) == 0 {
		return ""
	}
	return transportv2.PublicKeyFingerprint(state.TLS.PeerCertificates[0])
}
