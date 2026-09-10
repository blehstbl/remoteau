package engine

import (
	"net/url"
	"testing"

	"remote-au/internal/pairing"
)

// TestPairingURLFormat locks the QR pairing URL contract the iOS parser and
// the CLI/tray QR generators depend on:
//
//	remoteau://pair?v=2&h=<ip>&p=<port>&id=<deviceIDhex>&c=<code>
func TestPairingURLFormat(t *testing.T) {
	id, err := pairing.NewIdentity()
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	h := &Host{id: id, listenPort: 47010}

	raw := h.pairingURL("481516")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Scheme != "remoteau" {
		t.Fatalf("scheme = %q", u.Scheme)
	}
	if u.Host != "pair" {
		t.Fatalf("host = %q, want pair", u.Host)
	}
	q := u.Query()
	if q.Get("v") != "2" {
		t.Fatalf("v = %q", q.Get("v"))
	}
	if q.Get("p") != "47010" {
		t.Fatalf("p = %q", q.Get("p"))
	}
	if q.Get("c") != "481516" {
		t.Fatalf("c = %q", q.Get("c"))
	}
	wantID := id.DeviceID()
	if len(q.Get("id")) != 32 {
		t.Fatalf("id = %q, want 32 hex chars", q.Get("id"))
	}
	for i, b := range wantID {
		hi := "0123456789abcdef"[b>>4]
		lo := "0123456789abcdef"[b&0xF]
		if q.Get("id")[i*2] != hi || q.Get("id")[i*2+1] != lo {
			t.Fatalf("id = %q does not match device id", q.Get("id"))
		}
	}

	// Port defaults to the v2 default when the host has not bound yet.
	h2 := &Host{id: id}
	u2, err := url.Parse(h2.pairingURL("111111"))
	if err != nil {
		t.Fatalf("parse default-port url: %v", err)
	}
	if u2.Query().Get("p") != "47010" {
		t.Fatalf("default port = %q, want 47010", u2.Query().Get("p"))
	}
}
