package engine

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"remote-au/internal/audio"
)

// TestSyntheticSourceEndToEnd validates the FULL product audio path without
// any Windows audio endpoint: engine-internal test-tone source → codec →
// QUIC datagrams → client decode → PCM. This is the automated stand-in for
// the on-device validation while this PC has no active render endpoint.
func TestSyntheticSourceEndToEnd(t *testing.T) {
	addr := "127.0.0.1:47131"
	format := audioTestFormat()
	pinCodes := make(chan string, 2)

	host, err := NewHost(HostOptions{
		Name:          "TONE-PC",
		Store:         &memoryStore{},
		Backend:       &fakeBackend{format: format}, // backend unused for test tone
		ListenAddr:    addr,
		CaptureSource: audio.SourceTestTone,
		ToneMode:      audio.ToneSine,
		Format:        format,
		Logger:        loggingNop(),
		OnPairingCode: func(code string) { pinCodes <- code },
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	hostCtx, stopHost := context.WithCancel(context.Background())
	defer stopHost()
	go func() { _ = host.Run(hostCtx) }()
	time.Sleep(300 * time.Millisecond)

	var mu sync.Mutex
	var received []byte
	gotMedia := make(chan struct{}, 1)

	client, err := NewClient(ClientOptions{
		HostAddr: addr, Name: "TONE-PHONE", Store: &memoryStore{},
		PINProvider: func() (string, error) { return <-pinCodes, nil },
		OnMedia: func(pcm []byte) {
			mu.Lock()
			if len(received) < 48_000*4 { // cap at ~0.5 s
				received = append(received, pcm...)
			}
			mu.Unlock()
			select {
			case gotMedia <- struct{}{}:
			default:
			}
		},
		Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(runCtx) }()

	select {
	case <-gotMedia:
	case <-time.After(10 * time.Second):
		t.Fatal("no media from the synthetic source")
	}
	// Let a bit more audio accumulate so the amplitude check is meaningful.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	samples := append([]byte(nil), received...)
	mu.Unlock()
	if len(samples) < 960 {
		t.Fatalf("received only %d bytes", len(samples))
	}

	// A 440 Hz sine at -12 dBFS must be non-silent and within range.
	var maxAbs int
	for i := 0; i+1 < len(samples); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(samples[i:])))
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
	}
	if maxAbs == 0 {
		t.Fatal("received audio is silent — synthetic source path is broken")
	}
	if maxAbs > 32767 {
		t.Fatalf("sample out of range: %d", maxAbs)
	}
	if maxAbs < 1000 {
		t.Fatalf("synthetic tone amplitude suspiciously low: %d", maxAbs)
	}
	t.Logf("synthetic tone path OK: %d bytes, peak amplitude %d", len(samples), maxAbs)
}
