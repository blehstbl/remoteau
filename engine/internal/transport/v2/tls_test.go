package transportv2

import (
	"crypto/x509"
	"testing"

	"remote-au/internal/pairing"
)

// Device pinning must be stable across certificate regenerations: the same
// identity key yields a different certificate DER every time (random serial,
// ECDSA nonce), but a stable PUBLIC-KEY fingerprint.
func TestPublicKeyFingerprintStableAcrossCerts(t *testing.T) {
	id, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	c1, err := DeviceCertificate(id)
	if err != nil {
		t.Fatalf("cert 1: %v", err)
	}
	c2, err := DeviceCertificate(id)
	if err != nil {
		t.Fatalf("cert 2: %v", err)
	}

	fp1 := PublicKeyFingerprint(c1.Leaf)
	fp2 := PublicKeyFingerprint(c2.Leaf)
	if fp1 == "" || fp1 != fp2 {
		t.Fatalf("public-key fingerprint not stable: %q vs %q", fp1, fp2)
	}

	// Sanity: the DER hash may legitimately differ (that is exactly why it is
	// unsuitable for pinning).
	if CertFingerprint(c1.Leaf) == CertFingerprint(c2.Leaf) {
		t.Log("note: DER fingerprints happened to match (rare)")
	}

	// A different identity must have a different public-key fingerprint.
	other, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("other identity: %v", err)
	}
	oc, err := DeviceCertificate(other)
	if err != nil {
		t.Fatalf("other cert: %v", err)
	}
	if PublicKeyFingerprint(oc.Leaf) == fp1 {
		t.Fatal("distinct identities share a public-key fingerprint")
	}
}

func TestFingerprintVerifierPinsPublicKey(t *testing.T) {
	id, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	cert1, _ := DeviceCertificate(id)
	cert2, _ := DeviceCertificate(id) // regenerated cert, same key

	trusted := map[string]bool{PublicKeyFingerprint(cert1.Leaf): true}
	verify := FingerprintVerifier(trusted, false)

	// A regenerated certificate for the same identity must still verify.
	if err := verify([][]byte{cert2.Certificate[0]}, nil); err != nil {
		t.Fatalf("regenerated cert rejected: %v", err)
	}

	// An unknown identity must be rejected.
	stranger, _ := pairing.NewIdentity()
	sc, _ := DeviceCertificate(stranger)
	if err := verify([][]byte{sc.Certificate[0]}, nil); err == nil {
		t.Fatal("untrusted certificate accepted")
	}

	// trustAny accepts anything (pairing mode).
	if err := FingerprintVerifier(nil, true)([][]byte{sc.Certificate[0]}, nil); err != nil {
		t.Fatalf("trustAny rejected a cert: %v", err)
	}

	// Parsed-cert path used by tests still works.
	if _, err := x509.ParseCertificate(cert1.Certificate[0]); err != nil {
		t.Fatalf("parse: %v", err)
	}
}
