package pairing

import (
	"os"
	"testing"
	"time"
)

// DPAPI round-trips are user-scoped, so this test works in a normal Windows
// test process: protect → persist → reload → unprotect.
func TestWindowsStoreIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	// Nothing stored yet.
	id, err := store.LoadIdentity()
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if id != nil {
		t.Fatal("expected nil identity on a fresh store")
	}

	// Create + persist.
	created, err := NewIdentity()
	if err != nil {
		t.Fatalf("new identity: %v", err)
	}
	if err := store.SaveIdentity(created); err != nil {
		t.Fatalf("save identity: %v", err)
	}

	// Reopen (fresh struct, same dir) and load.
	reopened, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	loaded, err := reopened.LoadIdentity()
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if loaded == nil {
		t.Fatal("loaded identity is nil")
	}
	if loaded.Fingerprint() != created.Fingerprint() {
		t.Fatalf("fingerprint mismatch: %s != %s", loaded.Fingerprint(), created.Fingerprint())
	}
}

func TestWindowsStorePeerPersistence(t *testing.T) {
	dir := t.TempDir()
	store, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}

	rec := PeerRecord{
		ID:            [16]byte{1, 2, 3, 4},
		Name:          "iPhone",
		PairingSecret: make([]byte, 32),
		Fingerprint:   "abc",
		PairedAt:      time.Now().Unix(),
	}
	if err := store.SavePeer(rec); err != nil {
		t.Fatalf("save peer: %v", err)
	}

	reopened, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	peers, err := reopened.Peers()
	if err != nil || len(peers) != 1 {
		t.Fatalf("peers: %v %+v", err, peers)
	}
	if peers[0].Name != "iPhone" {
		t.Fatalf("peer name: %q", peers[0].Name)
	}

	if err := reopened.RemovePeer(rec.ID); err != nil {
		t.Fatalf("remove peer: %v", err)
	}
	after, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	remaining, err := after.Peers()
	if err != nil {
		t.Fatalf("peers after remove: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected no peers, got %+v", remaining)
	}
}

// A corrupt/unreadable identity must not brick startup: it is preserved and
// treated as absent so a fresh identity can be generated.
func TestWindowsStoreCorruptIdentitySelfHeals(t *testing.T) {
	dir := t.TempDir()
	store, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	store.state.IdentityPKCS8 = []byte("not-a-dpapi-blob")
	if err := store.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reopened, err := newWindowsStoreAt(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	id, err := reopened.LoadIdentity()
	if err != nil {
		t.Fatalf("corrupt identity should not error, got: %v", err)
	}
	if id != nil {
		t.Fatal("expected nil identity after self-heal")
	}
	if _, err := os.Stat(store.path() + ".corrupt"); err != nil {
		t.Fatalf("expected preserved corrupt file: %v", err)
	}
}
