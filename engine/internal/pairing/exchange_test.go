package pairing

import (
	"bytes"
	"testing"
)

func runExchange(t *testing.T, pinA, pinB string) (senderSecret, recvSecret []byte, err error) {
	t.Helper()

	sender, err := NewExchange(RoleSender, pinA, "SHIVANG-PC")
	if err != nil {
		t.Fatalf("sender: %v", err)
	}
	receiver, err := NewExchange(RoleReceiver, pinB, "iPhone")
	if err != nil {
		t.Fatalf("receiver: %v", err)
	}

	// receiver → BEGIN
	begin, err := receiver.BeginMessage()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := sender.OnBegin(begin); err != nil {
		t.Fatalf("sender onBegin: %v", err)
	}

	// sender → CHALLENGE
	challenge, err := sender.ChallengeMessage()
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if err := receiver.OnChallenge(challenge); err != nil {
		t.Fatalf("receiver onChallenge: %v", err)
	}

	// receiver → CONFIRM
	confirm, err := receiver.ConfirmMessage()
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := sender.OnConfirm(confirm); err != nil {
		return nil, nil, err
	}

	// sender → RESULT
	var senderID [16]byte
	result, err := sender.ResultMessage(senderID)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	peerName, peerID, err := receiver.OnResult(result)
	if err != nil {
		return nil, nil, err
	}
	if peerName != "SHIVANG-PC" {
		t.Fatalf("peerName=%q", peerName)
	}
	if peerID != senderID {
		t.Fatal("peer id mismatch")
	}
	return sender.Secret(), receiver.Secret(), nil
}

func TestPairingExchangeSuccess(t *testing.T) {
	sSecret, rSecret, err := runExchange(t, "481516", "481516")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if len(sSecret) != secretLen || len(rSecret) != secretLen {
		t.Fatalf("secret lengths: %d/%d", len(sSecret), len(rSecret))
	}
	if !bytes.Equal(sSecret, rSecret) {
		t.Fatal("pairing secrets differ")
	}
}

func TestPairingPinMismatchRejected(t *testing.T) {
	if _, _, err := runExchange(t, "111111", "222222"); err == nil {
		t.Fatal("mismatched PINs were accepted")
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	blob, err := id.MarshalPrivate()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loaded, err := LoadIdentity(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if id.Fingerprint() != loaded.Fingerprint() {
		t.Fatal("fingerprint changed after round-trip")
	}
	if id.DeviceID() != loaded.DeviceID() {
		t.Fatal("device id changed after round-trip")
	}
}

func TestFingerprintStableAndDistinct(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("distinct identities share a fingerprint")
	}
	if a.Fingerprint() != a.Fingerprint() {
		t.Fatal("fingerprint unstable")
	}
}
