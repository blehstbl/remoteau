// Command remote-au-tray is the Windows tray application for the RemoteAU
// engine (Phase 8): start/stop streaming, pairing codes, receiver list,
// quality presets, source selection, mute, autostart, diagnostics.
package main

import (
	"context"
	"encoding/hex"
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

// app carries tray-global state.
type app struct {
	mu           sync.Mutex
	host         *engine.Host
	cancel       context.CancelFunc
	running      bool
	store        pairing.Store
	backend      audio.Backend
	logger       logging.Logger
	logFile      io.Closer
	format       audio.Format
	preset       presetID
	muted        bool
	lastReceiver string
	autoStream   bool
	toneMode     audio.ToneMode
	sourceLabel  string
	lastError    string

	// Settings window (singleton) state.
	settingsMu       sync.Mutex
	settingsUI       *settingsUI
	settingsStarting bool

	// Menu items we update at runtime.
	mStatus       *systray.MenuItem
	mToggle       *systray.MenuItem
	mMute         *systray.MenuItem
	mConnect      *systray.MenuItem
	mReceivers    *systray.MenuItem
	mAutoStart    *systray.MenuItem
	qualityItems  map[presetID]*systray.MenuItem
	sourceItems   []*systray.MenuItem
	receiverItems []*systray.MenuItem
}

func main() {
	a := &app{
		format:       audio.DefaultFormat(),
		qualityItems: make(map[presetID]*systray.MenuItem),
	}
	if s, ok := loadSettings(); ok {
		a.autoStream = s.AutoStream
		a.lastReceiver = s.LastReceiver
		a.preset = presetFromName(s.Preset)
		a.muted = s.Muted
	} else {
		a.autoStream = true
		a.preset = presetAuto
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

	// --settings opens the settings window immediately (useful for shortcuts
	// and smoke testing); the tray still runs.
	for _, arg := range os.Args[1:] {
		if arg == "--settings" {
			a.openSettings()
			break
		}
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
	connectTitle := "Connect to last iPhone"
	if a.lastReceiver != "" {
		connectTitle = "Connect to " + a.lastReceiver
	}
	a.mConnect = systray.AddMenuItem(connectTitle, "Start streaming to the last receiver")
	a.mMute = systray.AddMenuItemCheckbox("Mute", "Silence all receivers", a.muted)
	if a.muted {
		a.mMute.SetTitle("Unmute")
	}
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
	// Reflect the persisted preset selection.
	for id, it := range a.qualityItems {
		if id == a.preset {
			it.Check()
		} else {
			it.Uncheck()
		}
	}

	a.mAutoStart = systray.AddMenuItemCheckbox("Start with Windows", "Launch RemoteAU at login", a.autostartEnabled())

	mSettings := systray.AddMenuItem("Open Settings", "Source, receivers, quality and diagnostics")
	diag := systray.AddMenuItem("Open log folder", "Show engine diagnostics log")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit", "Quit RemoteAU")

	// Wire handlers.
	go func() {
		for {
			select {
			case <-a.mToggle.ClickedCh:
				a.toggleStreaming()
			case <-a.mConnect.ClickedCh:
				a.connectLast()
			case <-a.mMute.ClickedCh:
				a.toggleMute()
			case <-a.mAutoStart.ClickedCh:
				a.toggleAutostart()
			case <-mSettings.ClickedCh:
				a.openSettings()
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

	// Auto-start streaming at launch (respecting the persisted preference).
	if a.autoStream {
		a.startStreaming()
	} else {
		a.setStatus("Stopped", "Start streaming")
	}
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
	toneMode := a.toneMode
	muted := a.muted
	ctx, cancel := context.WithCancel(context.Background())
	host, err := engine.NewHost(engine.HostOptions{
		Name:          hostname(),
		Store:         a.store,
		Backend:       a.backend,
		ListenAddr:    ":47010",
		CaptureSource: audio.SourceLoopback,
		ToneMode:      toneMode,
		Format:        a.format,
		OnPairingInfo: func(code, url string) {
			messageBox("RemoteAU - Pairing",
				fmt.Sprintf("Pair your iPhone:\n\nCode:  %s\n\nOr scan this URL in the app (QR):\n%s", code, url))
		},
		OnReceiversChanged: func(receivers []engine.ReceiverInfo) {
			if len(receivers) == 0 {
				return
			}
			name := receivers[0].Name
			a.mu.Lock()
			changed := name != "" && name != a.lastReceiver
			if changed {
				a.lastReceiver = name
			}
			a.mu.Unlock()
			if changed {
				if a.mConnect != nil {
					a.mConnect.SetTitle("Connect to " + name)
				}
				a.saveSettings()
			}
		},
		Logger: a.logger,
	})
	if err != nil {
		cancel()
		a.mu.Unlock()
		messageBox("RemoteAU", fmt.Sprintf("Cannot start host:\n%v", err))
		a.setError("cannot start host: " + err.Error())
		return
	}
	a.host = host
	a.cancel = cancel
	a.running = true
	a.mu.Unlock()

	if muted {
		host.MuteAll(true)
	}

	go func() {
		err := host.Run(ctx)
		if ctx.Err() != nil {
			return // canceled by stopStreaming/Exit
		}
		a.mu.Lock()
		if a.host == host {
			a.host = nil
		}
		a.running = false
		a.mu.Unlock()
		if err != nil {
			a.logger.Errorf("host stopped: %v", err)
			a.setError("engine stopped: " + err.Error())
			return
		}
		a.setStatus("Stopped", "Start streaming")
	}()

	a.setStatus("Waiting for receivers…", "Stop streaming")
}

// connectLast ensures streaming is running and surfaces the last receiver in
// the status line/tooltip.
func (a *app) connectLast() {
	a.mu.Lock()
	running := a.running
	name := a.lastReceiver
	a.mu.Unlock()
	if !running {
		a.startStreaming()
		a.mu.Lock()
		running = a.running
		a.mu.Unlock()
	}
	if !running {
		return // start failed; setError already reported it
	}
	if name == "" {
		name = "last iPhone"
	}
	systray.SetTooltip("RemoteAU — last: " + name)
	a.setStatus("Connecting to "+name+"…", "Stop streaming")
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
	a.saveSettings()
}

func (a *app) setStatus(text, toggle string) {
	if a.mStatus != nil {
		a.mStatus.SetTitle(text)
	}
	if a.mToggle != nil {
		a.mToggle.SetTitle(toggle)
	}
}

// setError records a persistent error/degraded state and surfaces it on the
// tray status item, its tooltip, and the settings window when open.
func (a *app) setError(msg string) {
	a.mu.Lock()
	a.lastError = msg
	a.mu.Unlock()
	if a.mStatus != nil {
		a.mStatus.SetTitle("Error: " + msg)
		systray.SetTooltip("RemoteAU — Error: " + msg)
	}
	a.notifySettingsError(msg)
}

// clearError drops a previously recorded error.
func (a *app) clearError() {
	a.mu.Lock()
	a.lastError = ""
	a.mu.Unlock()
}

// setSourceLabel remembers the applied capture source for diagnostics.
func (a *app) setSourceLabel(label string) {
	a.mu.Lock()
	a.sourceLabel = label
	a.mu.Unlock()
}

// setReceiverMuted mutes one live receiver.
func (a *app) setReceiverMuted(id string, muted bool) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host != nil {
		host.SetMuted(id, muted)
	}
}

// setReceiverVolume scales one live receiver's stream.
func (a *app) setReceiverVolume(id string, volume float64) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host != nil {
		host.SetVolume(id, volume)
	}
}

// forgetPeer removes a paired peer, preferring the live host (so any active
// connection is torn down) and falling back to the trust store directly.
func (a *app) forgetPeer(idHex string) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host != nil {
		if err := host.ForgetPeer(idHex); err != nil {
			a.setError("forget: " + err.Error())
			return
		}
		a.clearError()
		return
	}
	raw, err := hex.DecodeString(idHex)
	if err != nil || len(raw) != 16 {
		a.setError("forget: invalid device id")
		return
	}
	var id [16]byte
	copy(id[:], raw)
	if err := a.store.RemovePeer(id); err != nil {
		a.setError("forget: " + err.Error())
		return
	}
	a.clearError()
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
			if len(known) > 0 {
				known = map[string]bool{}
				for _, it := range a.receiverItems {
					it.Hide()
				}
				a.receiverItems = nil
			}
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
			a.setStatus(fmt.Sprintf("Streaming to %s (%s, %s, %s, %.1f/%.1f/%.1f ms)",
				r.Name, r.Codec, r.State, r.QualityMode, r.LossPct, r.JitterMs, r.RTTMs), "Stop streaming")
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
		label := fmt.Sprintf("%s — %s, %s, %s, %.1f/%.1f/%.1f ms", r.Name, r.Codec, r.State, r.QualityMode, r.LossPct, r.JitterMs, r.RTTMs)
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
	var selected presetID = presetAuto
	for id, it := range a.qualityItems {
		if it == item {
			selected = id
		} else {
			it.Uncheck()
		}
	}
	item.Check()
	a.applyPresetID(selected)
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
		a.setSourceLabel("System audio (default)")
		return
	}
	lists, err := a.backend.EnumerateDevices()
	if err != nil || index < 0 || index >= len(lists.Playback) {
		return
	}
	name := lists.Playback[index].Name
	_ = host.SetSourceDevice(name)
	a.setSourceLabel("Output: " + name)
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
