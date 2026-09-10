package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WindowsStore persists identity + peers with DPAPI (crypt32) protection
// under the user profile. DPAPI binds the blob to the Windows user account.
type WindowsStore struct {
	mu    sync.Mutex
	dir   string // %APPDATA%\RemoteAU
	state storedState
}

type storedState struct {
	IdentityPKCS8 []byte       `json:"identity_pkcs8_protected"` // DPAPI blob
	Peers         []PeerRecord `json:"peers"`
}

// NewWindowsStore opens (or creates) the store directory.
func NewWindowsStore() (*WindowsStore, error) {
	appdata, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}
	return newWindowsStoreAt(filepath.Join(appdata, "RemoteAU"))
}

// NewWindowsStoreAt opens a store rooted at dir. Useful for tests and for
// running multiple identities on one machine (each with its own data dir).
func NewWindowsStoreAt(dir string) (*WindowsStore, error) {
	return newWindowsStoreAt(dir)
}

// newWindowsStoreAt opens a store rooted at dir (used by tests).
func newWindowsStoreAt(dir string) (*WindowsStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &WindowsStore{dir: dir}
	_ = s.load()
	return s, nil
}

func (s *WindowsStore) path() string {
	return filepath.Join(s.dir, "trust.json")
}

func (s *WindowsStore) load() error {
	data, err := os.ReadFile(s.path())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &s.state)
}

func (s *WindowsStore) save() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(), data, 0o600)
}

// LoadIdentity returns the stored identity or nil.
func (s *WindowsStore) LoadIdentity() (*Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.IdentityPKCS8) == 0 {
		return nil, nil
	}
	pkcs8, err := dpapiUnprotect(s.state.IdentityPKCS8)
	if err != nil {
		// Self-heal: an identity we cannot decrypt (e.g. copied from another
		// user profile or corrupted) must not brick startup. Preserve it for
		// forensics and report "no identity" so a fresh one is generated.
		corrupt := s.path() + ".corrupt"
		_ = os.Rename(s.path(), corrupt)
		s.state.IdentityPKCS8 = nil
		s.state.Peers = nil
		return nil, nil
	}
	return LoadIdentity(pkcs8)
}

// SaveIdentity stores the identity key DPAPI-protected.
func (s *WindowsStore) SaveIdentity(id *Identity) error {
	pkcs8, err := id.MarshalPrivate()
	if err != nil {
		return err
	}
	blob, err := dpapiProtect(pkcs8)
	if err != nil {
		return fmt.Errorf("dpapi protect identity: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.IdentityPKCS8 = blob
	return s.save()
}

// Peers lists paired peers.
func (s *WindowsStore) Peers() ([]PeerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PeerRecord, 0, len(s.state.Peers))
	out = append(out, s.state.Peers...)
	return out, nil
}

// SavePeer inserts or updates a peer.
func (s *WindowsStore) SavePeer(rec PeerRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Peers {
		if s.state.Peers[i].ID == rec.ID {
			s.state.Peers[i] = rec
			return s.save()
		}
	}
	s.state.Peers = append(s.state.Peers, rec)
	return s.save()
}

// RemovePeer forgets a peer.
func (s *WindowsStore) RemovePeer(id [16]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.state.Peers[:0]
	for _, p := range s.state.Peers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	s.state.Peers = kept
	return s.save()
}

// DPAPI via crypt32 -----------------------------------------------------------

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptProtectData = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32.NewProc("CryptUnprotectData")
)

// dataBlob mirrors crypt32.DATA_BLOB.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, errors.New("dpapi protect: empty input")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	hr, _, _ := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		0, // optional entropy
		0, // reserved
		0, // prompt struct
		0, // flags
		uintptr(unsafe.Pointer(&out)),
	)
	if hr == 0 {
		return nil, fmt.Errorf("CryptProtectData failed")
	}
	defer LocalFree(out.pbData)
	outBytes := make([]byte, out.cbData)
	copy(outBytes, unsafe.Slice(out.pbData, out.cbData))
	return outBytes, nil
}

func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("dpapi unprotect: empty blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	// CryptUnprotectData(pDataIn, ppszDataDescr, pOptionalEntropy,
	//                     pvReserved, pPromptStruct, dwFlags, pDataOut)
	hr, _, _ := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags
		uintptr(unsafe.Pointer(&out)),
	)
	if hr == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed")
	}
	defer LocalFree(out.pbData)
	outBytes := make([]byte, out.cbData)
	copy(outBytes, unsafe.Slice(out.pbData, out.cbData))
	return outBytes, nil
}

func LocalFree(p *byte) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	proc := kernel32.NewProc("LocalFree")
	proc.Call(uintptr(unsafe.Pointer(p)))
}
