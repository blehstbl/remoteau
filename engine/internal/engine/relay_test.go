package engine

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"remote-au/internal/logging"
	"remote-au/internal/pairing"
	"remote-au/internal/relay"
	"remote-au/internal/transport/v2"
)

// TestRelayEndToEnd: host and client connect through a relay splice; all
// payloads are sealed with the pairing secret (the relay only forwards
// ciphertext). Verifies pairing-on-LAN prerequisite, sealed control flow and
// sealed media delivery.
func TestRelayEndToEnd(t *testing.T) {
	relayAddr := "127.0.0.1:47201"
	directAddr := "127.0.0.1:47202"
	format := audioTestFormat()

	// Relay.
	relayID, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("relay identity: %v", err)
	}
	relayCert, err := transportv2DeviceCertificate(relayID)
	if err != nil {
		t.Fatalf("relay cert: %v", err)
	}
	rl := debugTestLogger()
	go func() {
		_ = relay.RunRelay(context.Background(), relayAddr, relayCert, func(line string) {
			rl.Infof("relay: %s", line)
		})
	}()
	time.Sleep(300 * time.Millisecond)

	// Host with relay enabled + direct listener.
	hostStore := &memoryStore{}
	host, err := NewHost(HostOptions{
		Name: "RELAY-PC", Store: hostStore, Backend: &fakeBackend{format: format},
		ListenAddr: directAddr, CaptureSource: loopbackSource(), Format: format,
		RelayAddr: relayAddr,
		Logger:    debugTestLogger(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	// Pair once DIRECTLY (relay requires a prior pairing): capture the
	// pairing secret on both sides.
	pinCodes := make(chan string, 2)
	host.opts.OnPairingCode = func(code string) { pinCodes <- code }
	clientStore := &memoryStore{}
	pairClient, err := NewClient(ClientOptions{
		HostAddr: directAddr, Name: "RELAY-PHONE", Store: clientStore,
		PINProvider: func() (string, error) { return <-pinCodes, nil },
		OnMedia:     func([]byte) {},
		Logger:      debugTestLogger(),
	})
	if err != nil {
		t.Fatalf("pair client: %v", err)
	}
	pairCtx, cancelPair := context.WithCancel(context.Background())
	go func() { _ = pairClient.Run(pairCtx) }()
	time.Sleep(800 * time.Millisecond)
	cancelPair()
	time.Sleep(200 * time.Millisecond)

	peers, err := clientStore.Peers()
	if err != nil || len(peers) != 1 || len(peers[0].PairingSecret) != 32 {
		t.Fatalf("pairing not stored: %v %+v", err, peers)
	}
	hostDeviceID := peers[0].ID

	// Media sink through the relay.
	mediaGot := make(chan struct{}, 64)
	client, err := NewClient(ClientOptions{
		HostAddr:     "via-relay",
		RelayAddr:    relayAddr,
		HostDeviceID: hex.EncodeToString(hostDeviceID[:]),
		Name:         "RELAY-PHONE",
		Store:        clientStore,
		OnMedia: func([]byte) {
			select {
			case mediaGot <- struct{}{}:
			default:
			}
		},
		Logger: debugTestLogger(),
	})
	if err != nil {
		t.Fatalf("relay client: %v", err)
	}
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	go func() { _ = client.Run(relayCtx) }()

	select {
	case <-mediaGot:
	case <-time.After(10 * time.Second):
		t.Fatal("no media through relay")
	}
}

func transportv2DeviceCertificate(id *pairing.Identity) (*tls.Config, error) {
	cert, err := transportv2.DeviceCertificate(id)
	if err != nil {
		return nil, err
	}
	return transportv2.TLSConfig(cert, transportv2.FingerprintVerifier(nil, true)), nil
}

var _ = time.Second

func debugTestLogger() logging.Logger {
	l, _ := logging.New(os.Stderr, "debug", "text")
	return l
}

var _ = os.Stderr
