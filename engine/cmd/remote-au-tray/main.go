// Command remote-au-tray is the Windows tray application for the RemoteAU
// engine (Phase 8): start/stop streaming, pairing codes, receiver status,
// quality presets, start-with-Windows.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

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
	mu       sync.Mutex
	host     *engine.Host
	cancel   context.CancelFunc
	running  bool
	store    pairing.Store
	backend  audio.Backend
	logger   logging.Logger
	format   audio.Format

	// Menu items we update at runtime.
	mStatus   *systray.MenuItem
	mToggle   *systray.MenuItem
	mReceivers *systray.MenuItem
	mAutoStart *systray.MenuItem
}

func main() {
	logger, _ := logging.New(os.Stderr, "info", "text")

	a := &app{
		logger: logger,
		format: audio.DefaultFormat(),
	}

	backend, err := audio.OpenBackend(audio.DefaultBackendName())
	if err != nil {
		messageBox("RemoteAU", fmt.Sprintf("No audio backend available:\n%v", err))
		os.Exit(1)
	}
	a.backend = backend

	store, err := pairing.NewWindowsStore()
	if err != nil {
		messageBox("RemoteAU", fmt.Sprintf("Cannot open trust store:\n%v", err))
		os.Exit(1)
	}
	a.store = store

	systray.Run(a.onReady, a.onExit)
}

func (a *app) onReady() {
	systray.SetIcon(iconBytes())
	systray.SetTitle("RemoteAU")
	systray.SetTooltip("RemoteAU — Windows audio to your iPhone")

	a.mStatus = systray.AddMenuItem("Stopped", "Current state")
	a.mStatus.Disable()

	a.mToggle = systray.AddMenuItem("Start streaming", "Start/stop the v2 host")
	systray.AddSeparator()

	a.mReceivers = systray.AddMenuItem("Receivers", "Connected receivers")
	a.mReceivers.Disable()

	quality := systray.AddMenuItem("Quality", "Streaming quality preset")
	presetAuto := quality.AddSubMenuItemCheckbox("Auto", "Adaptive (recommended)", true)
	presetLow := quality.AddSubMenuItem("Lowest Latency", "PCM, tiny buffer")
	presetLossless := quality.AddSubMenuItem("Lossless", "PCM, safer buffer")
	presetRobust := quality.AddSubMenuItem("Robust", "Opus + FEC for weak Wi-Fi")

	a.mAutoStart = systray.AddMenuItemCheckbox("Start with Windows", "Launch RemoteAU at login", a.autostartEnabled())

	diag := systray.AddMenuItem("Open log folder", "Show engine logs")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("Exit", "Quit RemoteAU")

	// Wire handlers.
	go func() {
		for {
			select {
			case <-a.mToggle.ClickedCh:
				a.toggleStreaming()
			case <-a.mAutoStart.ClickedCh:
				a.toggleAutostart()
			case <-diag.ClickedCh:
				openLogFolder()
			case <-mExit.ClickedCh:
				systray.Quit()
				return
			case <-presetAuto.ClickedCh:
				presetAuto.Check()
				presetLow.Uncheck()
				presetLossless.Uncheck()
				presetRobust.Uncheck()
			case <-presetLow.ClickedCh:
				presetLow.Check()
				presetAuto.Uncheck()
			case <-presetLossless.ClickedCh:
				presetLossless.Check()
				presetAuto.Uncheck()
			case <-presetRobust.ClickedCh:
				presetRobust.Check()
				presetAuto.Uncheck()
			}
		}
	}()

	// Auto-start streaming at launch (auto-stream to trusted iPhones).
	a.startStreaming()
}

func (a *app) onExit() {
	a.stopStreaming()
}

// MARK: streaming lifecycle

func (a *app) startStreaming() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
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

	go func() {
		if err := host.Run(ctx); err != nil {
			a.logger.Errorf("host stopped: %v", err)
		}
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
		a.setStopped()
	}()

	a.setStatus("Streaming (waiting for receivers)…", "Stop streaming")
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
	host := a.host
	a.host = nil
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	_ = host
	a.setStopped()
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

func (a *app) setStatus(text, toggle string) {
	if a.mStatus != nil {
		a.mStatus.SetTitle(text)
	}
	if a.mToggle != nil {
		a.mToggle.SetTitle(toggle)
	}
}

func (a *app) setStopped() {
	a.setStatus("Stopped", "Start streaming")
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

func openLogFolder() {
	cmd := osCommand("explorer.exe", executableDir())
	_ = cmd.Start()
}

// iconBytes returns the tray icon.
func iconBytes() []byte {
	return trayIconICO()
}
