package mobile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"remote-au/internal/pairing"
)

// fileStore is the mobile trust store. It stores the identity (PKCS#8) and
// paired peers as JSON under the app container. The app container is
// sandbox-private per iOS; the Swift layer should ensure the folder carries
// NSFileProtectionComplete (Xcode "Data Protection" capability). A Keychain
// implementation is a later hardening step.
type fileStore struct {
	mu   sync.Mutex
	path string
}

type fileState struct {
	IdentityPKCS8 []byte             `json:"identity_pkcs8"`
	Peers         []pairing.PeerRecord `json:"peers"`
}

func newFileStore(dir string) (*fileStore, error) {
	return &fileStore{path: filepath.Join(dir, "trust.json")}, nil
}

func (f *fileStore) load() (fileState, error) {
	var st fileState
	data, err := os.ReadFile(f.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func (f *fileStore) save(st fileState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0o600)
}

// LoadIdentity returns the stored identity or nil.
func (f *fileStore) LoadIdentity() (*pairing.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return nil, err
	}
	if len(st.IdentityPKCS8) == 0 {
		return nil, nil
	}
	return pairing.LoadIdentity(st.IdentityPKCS8)
}

// SaveIdentity stores the identity.
func (f *fileStore) SaveIdentity(id *pairing.Identity) error {
	pkcs8, err := id.MarshalPrivate()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return err
	}
	st.IdentityPKCS8 = pkcs8
	return f.save(st)
}

// Peers lists paired peers.
func (f *fileStore) Peers() ([]pairing.PeerRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return nil, err
	}
	return st.Peers, nil
}

// SavePeer inserts or updates a peer.
func (f *fileStore) SavePeer(rec pairing.PeerRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return err
	}
	for i := range st.Peers {
		if st.Peers[i].ID == rec.ID {
			st.Peers[i] = rec
			return f.save(st)
		}
	}
	st.Peers = append(st.Peers, rec)
	return f.save(st)
}

// RemovePeer forgets a peer.
func (f *fileStore) RemovePeer(id [16]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := f.load()
	if err != nil {
		return err
	}
	kept := st.Peers[:0]
	for _, p := range st.Peers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	st.Peers = kept
	return f.save(st)
}

var _ = fmt.Sprintf
var _ = time.Now

// ---------------------------------------------------------------------------
// Keychain-backed store (iOS)

// StoreSource is implemented by the host platform (Swift/Keychain) and
// provides secure trust-storage persistence. gomobile binds it to Swift as
// the RemoteAUStoreSource protocol (getIdentity/getPeers/putIdentity/
// putPeers).
type StoreSource interface {
	GetIdentity() []byte // PKCS#8 identity key blob or nil
	GetPeers() []byte    // JSON array of pairing.PeerRecord or nil
	PutIdentity(pkcs8 []byte) error
	PutPeers(json []byte) error
}

// keychainStore implements pairing.Store on top of a host-provided
// StoreSource. Trust material is kept in memory and every mutation is
// written through to the source immediately, so the secure storage always
// mirrors the in-memory state. Secrets are never logged.
type keychainStore struct {
	mu    sync.Mutex
	src   StoreSource
	pkcs8 []byte
	peers []pairing.PeerRecord
}

// newKeychainStore loads the current trust material from the source. A
// non-empty peers blob must be a valid JSON array of pairing.PeerRecord
// (same field tags the fileStore uses).
func newKeychainStore(src StoreSource) (*keychainStore, error) {
	k := &keychainStore{src: src}
	if id := src.GetIdentity(); len(id) > 0 {
		k.pkcs8 = append([]byte(nil), id...)
	}
	if blob := src.GetPeers(); len(blob) > 0 {
		var peers []pairing.PeerRecord
		if err := json.Unmarshal(blob, &peers); err != nil {
			return nil, fmt.Errorf("mobile: keychain peers: %w", err)
		}
		k.peers = peers
	}
	return k, nil
}

// LoadIdentity returns the stored identity or nil.
func (k *keychainStore) LoadIdentity() (*pairing.Identity, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.pkcs8) == 0 {
		return nil, nil
	}
	return pairing.LoadIdentity(k.pkcs8)
}

// SaveIdentity stores the identity in memory and writes it through to the
// secure source.
func (k *keychainStore) SaveIdentity(id *pairing.Identity) error {
	pkcs8, err := id.MarshalPrivate()
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.src.PutIdentity(pkcs8); err != nil {
		return fmt.Errorf("mobile: keychain put identity: %w", err)
	}
	k.pkcs8 = append([]byte(nil), pkcs8...)
	return nil
}

// Peers lists paired peers.
func (k *keychainStore) Peers() ([]pairing.PeerRecord, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]pairing.PeerRecord, len(k.peers))
	copy(out, k.peers)
	return out, nil
}

// SavePeer inserts or updates a peer in memory and writes the peers blob
// through to the secure source.
func (k *keychainStore) SavePeer(rec pairing.PeerRecord) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	next := append([]pairing.PeerRecord(nil), k.peers...)
	replaced := false
	for i := range next {
		if next[i].ID == rec.ID {
			next[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		next = append(next, rec)
	}
	if err := putPeersLocked(k.src, next); err != nil {
		return err
	}
	k.peers = next
	return nil
}

// RemovePeer forgets a peer in memory and writes the updated peers blob
// (which drops the peer's pairing secret) through to the secure source.
func (k *keychainStore) RemovePeer(id [16]byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	next := make([]pairing.PeerRecord, 0, len(k.peers))
	for _, p := range k.peers {
		if p.ID != id {
			next = append(next, p)
		}
	}
	if err := putPeersLocked(k.src, next); err != nil {
		return err
	}
	k.peers = next
	return nil
}

// putPeersLocked serializes peers with the same field tags as fileStore and
// writes them through. keychainStore callers must hold k.mu; the migration
// path calls it with a bare source before any store exists.
func putPeersLocked(src StoreSource, peers []pairing.PeerRecord) error {
	if peers == nil {
		peers = []pairing.PeerRecord{}
	}
	blob, err := json.Marshal(peers)
	if err != nil {
		return fmt.Errorf("mobile: keychain peers: %w", err)
	}
	if err := src.PutPeers(blob); err != nil {
		return fmt.Errorf("mobile: keychain put peers: %w", err)
	}
	return nil
}

// migrateLegacyStore copies the legacy file-based trust store
// (legacyDir/trust.json) into the secure source and deletes the file. It
// only runs while the source is completely empty, so it can never clobber
// newer secure-storage data. Nothing sensitive is logged.
func migrateLegacyStore(source StoreSource, legacyDir string) error {
	if len(source.GetIdentity()) > 0 || len(source.GetPeers()) > 0 {
		return nil
	}
	legacyPath := filepath.Join(legacyDir, "trust.json")
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("mobile: legacy trust file: %w", err)
	}
	var legacy fileState
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("mobile: legacy trust file: %w", err)
	}
	if len(legacy.IdentityPKCS8) > 0 {
		if err := source.PutIdentity(legacy.IdentityPKCS8); err != nil {
			return fmt.Errorf("mobile: legacy migration: %w", err)
		}
	}
	if len(legacy.Peers) > 0 {
		if err := putPeersLocked(source, legacy.Peers); err != nil {
			return fmt.Errorf("mobile: legacy migration: %w", err)
		}
	}
	// Best-effort removal of the now-redundant file; leaving it behind is
	// harmless (the source is already populated, so migration never runs
	// again and the Swift layer removes its app-support leftovers too).
	_ = os.Remove(legacyPath)
	return nil
}
