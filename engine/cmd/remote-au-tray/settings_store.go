package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// saveMu serializes settings.json writes; saves can originate from the tray
// thread, the walk UI thread, and host callbacks concurrently.
var saveMu sync.Mutex

// persistedSettings is the on-disk schema for
// %AppData%\RemoteAU\settings.json.
type persistedSettings struct {
	AutoStream   bool   `json:"auto_stream"`
	LastReceiver string `json:"last_receiver"`
	Preset       string `json:"preset"`
	Muted        bool   `json:"muted"`
}

// settingsPath returns %AppData%\RemoteAU\settings.json, creating the
// directory if needed.
func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "RemoteAU")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// loadSettings reads settings.json. The bool reports whether the file existed
// and parsed; callers use it to apply defaults on first run.
func loadSettings() (persistedSettings, bool) {
	var s persistedSettings
	p, err := settingsPath()
	if err != nil {
		return s, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return persistedSettings{}, false
	}
	return s, true
}

// saveSettings snapshots the app's persisted state and writes it atomically.
func (a *app) saveSettings() {
	a.mu.Lock()
	s := persistedSettings{
		AutoStream:   a.autoStream,
		LastReceiver: a.lastReceiver,
		Preset:       presetName(a.preset),
		Muted:        a.muted,
	}
	a.mu.Unlock()

	p, err := settingsPath()
	if err != nil {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	saveMu.Lock()
	defer saveMu.Unlock()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
