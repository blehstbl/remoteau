// Command remote-au-tray is the Windows tray application for the RemoteAU
// engine (Phase 8): start/stop streaming, pairing codes, receiver list,
// quality presets, source selection, mute, autostart, diagnostics.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/systray"

	"golang.org/x/sys/windows/registry"

	"remote-au/internal/audio"
	"remote-au/internal/engine"
	"remote-au/internal/logging"
	"remote-au/internal/pairing"
)

var version = "dev"

type presetID int

const (
	presetAuto presetID = iota
	presetLowest
	presetLossless
	presetRobust
)

// app carries tray-global state.
type app struct {
	mu      sync.Mutex
	host    *engine.Host
	cancel  context.CancelFunc
	running bool
	store   pairing.Store
	backend audio.Backend
	logger  logging.Logger
	logFile io.Closer
	format  audio.Format
	preset  presetID
	muted   bool
	lastReceiver string

	// Menu items we update at runtime.
	mStatus    *systray.MenuItem
	mToggle    *systray.MenuItem
	mMute      *systray.MenuItem
	mReceivers *systray.MenuItem
	mAutoStart *systray.MenuItem
	qualityItems map[presetID]*systray.MenuItem
	sourceItems  []*systray.MenuItem
	receiverItems []*systray.MenuItem
}

func main() {
	a := &app{
		format:       audio.DefaultFormat(),
		qualityItems: make(map[presetID]*systray.MenuItem),
	}

	backend, err := audio.OpenBackend(audio.DefaultBackendName())
	if err != nil {
		a.fatalBox(fmt.Sprintf("No audio backend available:\n%v", err))
		os.Exit(1)
	}
	a.backend = backend

	store, err := pairing.NewWindowsStore()
	if err != nil {
		a.fatalBox(fmt.Sprintf("Cannot open trust store:\n%v", err))
		os.Exit(1)
	}
	a.store = store

	// Persistent engine log (diagnostics).
	logPath, err := a.openLogFile()
	if err == nil {
		a.logger = logPath
	} else {
		a.logger, _ = logging.New(io.Discard, "info", "text")
	}

	systray.Run(a.onReady, a.onExit)
}

// openLogFile wires the engine log to %AppData%\RemoteAU\engine.log.
func (a *app) openLogFile() (logging.Logger, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "RemoteAU")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "engine.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	a.logFile = f
	l, err := logging.New(f, "debug", "text")
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

func (a *app) onReady() {
	systray.SetIcon(iconBytes())
	systray.SetTitle("RemoteAU")
	systray.SetTooltip("RemoteAU — Windows audio to your iPhone")

	a.mStatus = systray.AddMenuItem("Starting…", "Current state")
	a.mStatus.Disable()

	a.mToggle = systray.AddMenuItem("Stop streaming", "Start/stop the v2 host")
	a.mMute = systray.AddMenuItemCheckbox("Mute", "Silence all receivers", false)
	systray.AddSeparator()

	a.mReceivers = systray.AddMenuItem("Receivers", "Connected receivers")
	a.mReceivers.Disable()

	// Source selection (render devices = loopback sources).
	srcMenu := systray.AddMenuItem("Source", "What to capture")
	srcDefault := srcMenu.AddSubMenuItemCheckbox("System audio (default)", "Follow the default output device", true)
	a.sourceItems = append(a.sourceItems, srcDefault)
	go func() {
		time.Sleep(500 * time.Millisecond)
		lists, err := a.backend.EnumerateDevices()
		if err != nil {
			return
		}
		for i, dev := range lists.Playback {
			item := srcMenu.AddSubMenuItem("Output: "+dev.Name, "Loopback from this output device")
			idx := i
			a.sourceItems = append(a.sourceItems, item)
			go func(it *systray.MenuItem) {
				for range it.ClickedCh {
					a.selectSource(idx, false, it)
				}
			}(item)
		}
	}()
	go func() {
		for range srcDefault.ClickedCh {
			a.selectSource(-1, true, srcDefault)
		}
	}()

	// Quality presets (wired to the engine).
	quality := systray.AddMenuItem("Quality", "Streaming quality preset")
	itemAuto := quality.AddSubMenuItemCheckbox("Auto", "Adaptive (recommended)", true)
	itemLow := quality.AddSubMenuItem("Lowest Latency", "PCM, tiny buffer")
	itemLossless := quality.AddSubMenuItem("Lossless", "PCM, safer buffer")
	itemRobust := quality.AddSubMenuItemCheckbox("Robust", "Opus + FEC for weak Wi-Fi", false)
	a.qualityItems = map[presetID]*systray.MenuItem{
		presetAuto:     itemAuto,
		presetLowest:   itemLow,
		presetLossless: itemLossless,
		presetRobust:   itemRobust,
	}

	a.mAutoStart = systray.AddMenuItemCheckbox("Start with Windows", "Launch RemoteAU at login", a.autostartEnabled())

	diag := systray.AddMenuItem("Open log folder", "Show engine diagnostics log")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit", "Quit RemoteAU")

	// Wire handlers.
	go func() {
		for {
			select {
			case <-a.mToggle.ClickedCh:
				a.toggleStreaming()
			case <-a.mMute.ClickedCh:
				a.toggleMute()
			case <-a.mAutoStart.ClickedCh:
				a.toggleAutostart()
			case <-diag.ClickedCh:
				openLogFolder()
			case <-mExit.ClickedCh:
				systray.Quit()
				return
			case <-itemAuto.ClickedCh:
				a.applyPreset(itemAuto)
			case <-itemLow.ClickedCh:
				a.applyPreset(itemLow)
			case <-itemLossless.ClickedCh:
				a.applyPreset(itemLossless)
			case <-itemRobust.ClickedCh:
				a.applyPreset(itemRobust)
			}
		}
	}()

	// Receiver list refresher.
	go a.receiverRefresher()

	// Auto-start streaming at launch.
	a.startStreaming()
}

func (a *app) onExit() {
	a.stopStreaming()
	if a.logFile != nil {
		_ = a.logFile.Close()
	}
}

// MARK: streaming lifecycle

func (a *app) startStreaming() {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	host, err := engine.NewHost(engine.HostOptions{
		Name:          hostname(),
		Store:         a.store,
		Backend:       a.backend,
		ListenAddr:    ":47010",
		CaptureSource: audio.SourceLoopback,
		Format:        a.format,
		OnPairingCode: func(code string) {
			messageBox("RemoteAU — Pairing",
				fmt.Sprintf("Enter this code on your iPhone to pair:\n\n    %s", code))
		},
		OnReceiversChanged: func(receivers []engine.ReceiverInfo) {
			if len(receivers) > 0 {
				a.mu.Lock()
				a.lastReceiver = receivers[0].Name
				a.mu.Unlock()
			}
		},
		Logger: a.logger,
	})
	if err != nil {
		cancel()
		messageBox("RemoteAU", fmt.Sprintf("Cannot start host:\n%v", err))
		return
	}
	a.host = host
	a.cancel = cancel
	a.running = true
	a.mu.Unlock()

	go func() {
		if err := host.Run(ctx); err != nil {
			a.logger.Errorf("host stopped: %v", err)
		}
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
		a.setStatus("Stopped", "Start streaming")
	}()

	a.setStatus("Waiting for receivers…", "Stop streaming")
}

func (a *app) stopStreaming() {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	cancel := a.cancel
	a.cancel = nil
	a.running = false
	a.host = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	a.setStatus("Stopped", "Start streaming")
}

func (a *app) toggleStreaming() {
	a.mu.Lock()
	running := a.running
	a.mu.Unlock()
	if running {
		a.stopStreaming()
	} else {
		a.startStreaming()
	}
}

func (a *app) toggleMute() {
	a.mu.Lock()
	host := a.host
	muted := !a.muted
	a.muted = muted
	a.mu.Unlock()
	if host != nil {
		host.MuteAll(muted)
	}
	if muted {
		a.mMute.Check()
		a.mMute.SetTitle("Unmute")
	} else {
		a.mMute.Uncheck()
		a.mMute.SetTitle("Mute")
	}
}

func (a *app) setStatus(text, toggle string) {
	if a.mStatus != nil {
		a.mStatus.SetTitle(text)
	}
	if a.mToggle != nil {
		a.mToggle.SetTitle(toggle)
	}
}

// MARK: receiver list

func (a *app) receiverRefresher() {
	var known = map[string]bool{}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.mu.Lock()
		host := a.host
		running := a.running
		a.mu.Unlock()
		if !running || host == nil {
			continue
		}

		// Current receivers → submenu (rebuilt on change).
		current := host.Receivers()
		changed := false
		live := map[string]bool{}
		for _, r := range current {
			live[r.DeviceID] = true
			if !known[r.DeviceID] {
				changed = true
			}
		}
		if len(live) != len(known) {
			changed = true
		}
		if changed {
			known = live
			a.rebuildReceiverItems(current)
		}

		// Update status line.
		if len(current) > 0 {
			r := current[0]
			a.setStatus(fmt.Sprintf("Streaming to %s (%s, %.0f/%.0f/%.0f ms)",
				r.Name, r.Codec, r.LossPct, r.JitterMs, r.RTTMs), "Stop streaming")
		} else {
			a.setStatus("Waiting for receivers…", "Stop streaming")
		}
	}
}

func (a *app) rebuildReceiverItems(receivers []engine.ReceiverInfo) {
	// Remove old items.
	for _, it := range a.receiverItems {
		it.Hide()
	}
	a.receiverItems = nil
	a.mu.Lock()
	hosts := a.host
	a.mu.Unlock()
	if hosts == nil {
		return
	}
	if len(receivers) == 0 {
		return
	}
	for _, r := range receivers {
		rCopy := r
		label := fmt.Sprintf("%s — %s, %.0f/%.0f/%.0f ms", r.Name, r.Codec, r.LossPct, r.JitterMs, r.RTTMs)
		item := a.mReceivers.AddSubMenuItemCheckbox(label, "Click to mute/unmute this receiver", r.Muted)
		a.receiverItems = append(a.receiverItems, item)
		go func(it *systray.MenuItem) {
			for range it.ClickedCh {
				newMuted := !rCopy.Muted
				rCopy.Muted = newMuted
				hosts.SetMuted(rCopy.DeviceID, newMuted)
				if newMuted {
					it.Check()
				} else {
					it.Uncheck()
				}
			}
		}(item)
	}
}

// MARK: quality + source

func (a *app) applyPreset(item *systray.MenuItem) {
	var selected presetID
	for id, it := range a.qualityItems {
		if it == item {
			selected = id
		} else {
			it.Uncheck()
		}
	}
	item.Check()
	a.mu.Lock()
	a.preset = selected
	host := a.host
	a.mu.Unlock()
	if host == nil {
		return
	}
	switch selected {
	case presetLowest:
		host.SetQuality(32000, 64000, false)
	case presetLossless:
		host.SetQuality(64000, 256000, false)
	case presetRobust:
		host.SetQuality(32000, 128000, true)
	default:
		host.SetQuality(32000, 256000, false)
	}
}

func (a *app) selectSource(index int, useDefault bool, item *systray.MenuItem) {
	for _, it := range a.sourceItems {
		it.Uncheck()
	}
	item.Check()
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host == nil {
		return
	}
	if useDefault {
		_ = host.SetSourceDevice("")
		return
	}
	lists, err := a.backend.EnumerateDevices()
	if err != nil || index < 0 || index >= len(lists.Playback) {
		return
	}
	_ = host.SetSourceDevice(lists.Playback[index].Name)
}

// MARK: autostart

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "RemoteAU"

func (a *app) autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValue)
	return err == nil && v != ""
}

func (a *app) toggleAutostart() {
	if a.autostartEnabled() {
		k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err == nil {
			_ = k.DeleteValue(runValue)
			k.Close()
		}
		a.mAutoStart.Uncheck()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue(runValue, fmt.Sprintf(`"%s" --minimized`, exe))
	a.mAutoStart.Check()
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "windows-pc"
	}
	return name
}

func (a *app) fatalBox(text string) {
	messageBox("RemoteAU", text)
}

// iconBytes returns the tray icon.
func iconBytes() []byte {
	return trayIconICO()
}
