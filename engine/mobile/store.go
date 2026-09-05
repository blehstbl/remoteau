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
