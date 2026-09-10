package engine

import (
	"encoding/hex"
	"testing"

	"remote-au/internal/audio"
	"remote-au/internal/pairing"
	"remote-au/internal/protocol/v2"
)

// Per-app capture (Phase 9): pure validation tests only — no COM, no live
// audio endpoints (this machine has none). Live process-loopback capture
// still needs runtime validation on a real Windows system.

func TestParsePerAppSourceName(t *testing.T) {
	pids, exclude, err := parsePerAppSourceName("p:1,2,3")
	if err != nil || exclude || len(pids) != 3 || pids[0] != 1 || pids[1] != 2 || pids[2] != 3 {
		t.Fatalf("p:1,2,3 → (%v, %v, %v)", pids, exclude, err)
	}

	pids, exclude, err = parsePerAppSourceName("x:9")
	if err != nil || !exclude || len(pids) != 1 || pids[0] != 9 {
		t.Fatalf("x:9 → (%v, %v, %v)", pids, exclude, err)
	}

	// Duplicates collapse; whitespace tolerated.
	pids, exclude, err = parsePerAppSourceName("p: 5 , 5,6 ")
	if err != nil || exclude || len(pids) != 2 || pids[0] != 5 || pids[1] != 6 {
		t.Fatalf("p: 5 , 5,6 → (%v, %v, %v)", pids, exclude, err)
	}

	bad := []string{
		"", "p", "p:", "x:", "q:1", "123", "p:0", "x:0", "p:1,,2", "p:abc",
		"x:1,2", "p:-1", "p:4294967296",
	}
	for _, name := range bad {
		if _, _, err := parsePerAppSourceName(name); err == nil {
			t.Fatalf("per-app name %q should be rejected", name)
		}
	}
}

func TestSetCaptureApps(t *testing.T) {
	format := audioTestFormat()
	host, err := NewHost(HostOptions{
		Name: "APPS-PC", Store: &memoryStore{}, Backend: &fakeBackend{format: format},
		Format: format, Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}

	// Validation errors.
	if err := host.SetCaptureApps(nil, false); err == nil {
		t.Fatal("empty pid list accepted")
	}
	if err := host.SetCaptureApps([]uint32{1, 2}, true); err == nil {
		t.Fatal("exclude with two pids accepted")
	}
	if err := host.SetCaptureApps([]uint32{0}, false); err == nil {
		t.Fatal("pid 0 accepted")
	}

	// Include mode: options set, capture source forced back to loopback.
	if err := host.SetCaptureApps([]uint32{123, 456}, false); err != nil {
		t.Fatalf("SetCaptureApps: %v", err)
	}
	if len(host.opts.CaptureApps) != 2 || host.opts.CaptureApps[0] != 123 ||
		host.opts.ExcludeApps != nil || host.opts.CaptureSource != audio.SourceLoopback ||
		host.opts.DeviceSelector != "" {
		t.Fatalf("include mode not applied: %+v", host.opts)
	}

	// Exclude mode replaces the include list.
	if err := host.SetCaptureApps([]uint32{77}, true); err != nil {
		t.Fatalf("SetCaptureApps exclude: %v", err)
	}
	if len(host.opts.ExcludeApps) != 1 || host.opts.ExcludeApps[0] != 77 || host.opts.CaptureApps != nil {
		t.Fatalf("exclude mode not applied: %+v", host.opts)
	}

	// A normal source switch must clear the per-app lists.
	if err := host.SetSourceDevice(""); err != nil {
		t.Fatalf("SetSourceDevice: %v", err)
	}
	if host.opts.CaptureApps != nil || host.opts.ExcludeApps != nil {
		t.Fatalf("per-app lists survive SetSourceDevice: %+v", host.opts)
	}
}

func TestApplySetSourcePerAppKind(t *testing.T) {
	format := audioTestFormat()
	host, err := NewHost(HostOptions{
		Name: "SRCAPPS-PC", Store: &memoryStore{}, Backend: &listingBackend{format: format},
		Format: format, Logger: loggingNop(),
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}

	ok, detail := host.applySetSourceWithApps(protocolv2.SetSource{Kind: protocolv2.SetSourcePerApp, Name: "p:123"})
	if !ok || detail == "" {
		t.Fatalf("per-app include rejected: %q", detail)
	}
	if len(host.opts.CaptureApps) != 1 || host.opts.CaptureApps[0] != 123 {
		t.Fatalf("per-app include not applied: %+v", host.opts)
	}

	ok, _ = host.applySetSourceWithApps(protocolv2.SetSource{Kind: protocolv2.SetSourcePerApp, Name: "junk"})
	if ok {
		t.Fatal("per-app junk accepted")
	}

	// Unknown kinds stay rejected.
	if ok, _ := host.applySetSourceWithApps(protocolv2.SetSource{Kind: 9}); ok {
		t.Fatal("unknown source kind accepted")
	}
}

func TestForgetPeer(t *testing.T) {
	store := &memoryStore{}
	host, err := NewHost(HostOptions{Name: "FORGET-PC", Store: store, Logger: loggingNop()})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	var id [16]byte
	for i := range id {
		id[i] = byte(i + 1)
	}
	const fp = "feedfacefeedface"
	if err := store.SavePeer(pairing.PeerRecord{ID: id, Name: "phone", Fingerprint: fp}); err != nil {
		t.Fatalf("save peer: %v", err)
	}
	host.trusted[fp] = true

	if err := host.ForgetPeer(hex.EncodeToString(id[:])); err != nil {
		t.Fatalf("ForgetPeer: %v", err)
	}
	peers, _ := store.Peers()
	if len(peers) != 0 {
		t.Fatalf("peer still stored: %v", peers)
	}
	if host.trusted[fp] {
		t.Fatal("peer still trusted")
	}

	// Unknown and malformed IDs are errors.
	if err := host.ForgetPeer(hex.EncodeToString(id[:])); err == nil {
		t.Fatal("forgetting unknown peer should fail")
	}
	if err := host.ForgetPeer("nothex"); err == nil {
		t.Fatal("malformed device id should fail")
	}
}
