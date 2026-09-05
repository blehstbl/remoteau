// Package profiles implements the Phase 14 named profiles: Home / Lossless,
// Gaming / Lowest Latency, Weak Wi-Fi / Robust, Remote / Efficient. Profiles
// map onto the quality presets so they sync naturally with the GUIs.
package profiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Profile is one named streaming configuration.
type Profile struct {
	Name       string `json:"name"`
	Preset     string `json:"preset"`       // auto | lowest | lossless | robust
	TargetMs   int    `json:"target_ms"`    // receiver jitter target (advanced)
	PreferOpus bool   `json:"prefer_opus"`  // v2 codec preference
	BitrateKbps int   `json:"bitrate_kbps"` // v2 opus bitrate
	FEC        bool   `json:"fec"`
	DTX        bool   `json:"dtx"`
	Relay      bool   `json:"relay"` // WAN mode
}

// Defaults are the four profiles from the plan.
func Defaults() []Profile {
	return []Profile{
		{Name: "Home / Lossless", Preset: "lossless", TargetMs: 25, PreferOpus: false, BitrateKbps: 128, FEC: false, DTX: false, Relay: false},
		{Name: "Gaming / Lowest Latency", Preset: "lowest", TargetMs: 10, PreferOpus: false, BitrateKbps: 128, FEC: false, DTX: false, Relay: false},
		{Name: "Weak Wi-Fi / Robust", Preset: "robust", TargetMs: 60, PreferOpus: true, BitrateKbps: 96, FEC: true, DTX: true, Relay: false},
		{Name: "Remote / Efficient", Preset: "robust", TargetMs: 90, PreferOpus: true, BitrateKbps: 64, FEC: true, DTX: true, Relay: true},
	}
}

// Store persists profiles as JSON.
type Store struct {
	path string
}

// NewStore opens (or creates) a profile store in dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "profiles.json")}, nil
}

// List returns the stored profiles, or the defaults when none exist.
func (s *Store) List() ([]Profile, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Defaults(), nil
		}
		return nil, err
	}
	var out []Profile
	if err := json.Unmarshal(data, &out); err != nil {
		return Defaults(), nil
	}
	if len(out) == 0 {
		return Defaults(), nil
	}
	return out, nil
}

// Save replaces the stored profiles.
func (s *Store) Save(list []Profile) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// ByName finds a profile (case-insensitive) from a list.
func ByName(list []Profile, name string) (Profile, bool) {
	for _, p := range list {
		if equalFold(p.Name, name) {
			return p, true
		}
	}
	return Profile{}, false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
