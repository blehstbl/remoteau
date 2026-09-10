package main

import (
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/lxn/walk"

	"remote-au/internal/audio"
	"remote-au/internal/engine"
)

// Source-selection kinds used by the settings window radio group.
const (
	sourceDefault = iota
	sourceDevice
	sourceTestTone
)

type sourceChoice struct {
	radio *walk.RadioButton
	kind  int
	name  string
}

// liveRow holds the per-receiver widgets so refreshes can update them in place
// instead of rebuilding rows (which would interrupt slider/checkbox input).
type liveRow struct {
	devID    string
	row      *walk.Composite
	name     *walk.Label
	stats    *walk.Label
	mute     *walk.CheckBox
	vol      *walk.Slider
	updating bool
}

// settingsUI is the singleton settings window. All widget access happens on
// the window's own message-loop goroutine (the walk thread).
type settingsUI struct {
	app *app
	mw  *walk.MainWindow

	initializing bool

	// Source.
	sourceChoices []sourceChoice
	sourceDefault *walk.RadioButton
	sourceTone    *walk.RadioButton
	toneCombo     *walk.ComboBox

	// Quality.
	qualityRadios map[presetID]*walk.RadioButton

	// Auto-connect.
	autoStream *walk.CheckBox
	lastLabel  *walk.Label

	// Receivers.
	peersBox *walk.Composite
	liveBox  *walk.Composite
	peerRows []*walk.Composite
	peerIDs  []string
	liveRows map[string]*liveRow

	// Diagnostics + status.
	diag   *walk.TextEdit
	status *walk.Label

	stop chan struct{}
}

// newSettingsUI builds the window (on the caller's locked walk thread).
func newSettingsUI(a *app) (*settingsUI, error) {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return nil, err
	}
	ui := &settingsUI{
		app:           a,
		mw:            mw,
		initializing:  true,
		qualityRadios: map[presetID]*walk.RadioButton{},
		liveRows:      map[string]*liveRow{},
	}
	_ = mw.SetTitle("RemoteAU — Settings")
	_ = mw.SetLayout(walk.NewVBoxLayout())
	_ = mw.SetSize(walk.Size{Width: 820, Height: 900})

	// --- Source ---------------------------------------------------------
	srcGroup, _ := walk.NewGroupBox(mw)
	_ = srcGroup.SetTitle("Source")
	_ = srcGroup.SetLayout(walk.NewVBoxLayout())

	makeChoice := func(text string, kind int, name string) *walk.RadioButton {
		rb, _ := walk.NewRadioButton(srcGroup)
		_ = rb.SetText(text)
		ui.sourceChoices = append(ui.sourceChoices, sourceChoice{radio: rb, kind: kind, name: name})
		return rb
	}
	ui.sourceDefault = makeChoice("System audio (default)", sourceDefault, "")
	ui.sourceDefault.SetChecked(true)
	if lists, err := a.backend.EnumerateDevices(); err == nil {
		for _, dev := range lists.Playback {
			makeChoice("Output: "+dev.Name, sourceDevice, dev.Name)
		}
	}
	ui.sourceTone = makeChoice("Test tone (diagnostics)", sourceTestTone, "")

	toneRow, _ := walk.NewComposite(srcGroup)
	_ = toneRow.SetLayout(walk.NewHBoxLayout())
	toneLbl, _ := walk.NewLabel(toneRow)
	_ = toneLbl.SetText("Tone mode:")
	ui.toneCombo, _ = walk.NewComboBox(toneRow)
	_ = ui.toneCombo.SetModel([]string{"Sine", "Click"})
	if a.toneMode == audio.ToneClick {
		_ = ui.toneCombo.SetCurrentIndex(1)
	} else {
		_ = ui.toneCombo.SetCurrentIndex(0)
	}
	ui.toneCombo.CurrentIndexChanged().Attach(func() {
		a.mu.Lock()
		if ui.toneCombo.CurrentIndex() == 1 {
			a.toneMode = audio.ToneClick
		} else {
			a.toneMode = audio.ToneSine
		}
		a.mu.Unlock()
	})

	applySrc, _ := walk.NewPushButton(srcGroup)
	_ = applySrc.SetText("Apply source")
	applySrc.Clicked().Attach(func() { ui.applySource() })

	srcNote, _ := walk.NewLabel(srcGroup)
	_ = srcNote.SetText("Tone mode is read when streaming starts; restart streaming to switch Sine/Click.")

	// --- Receivers ------------------------------------------------------
	recvGroup, _ := walk.NewGroupBox(mw)
	_ = recvGroup.SetTitle("Receivers")
	_ = recvGroup.SetLayout(walk.NewVBoxLayout())

	pairedLbl, _ := walk.NewLabel(recvGroup)
	_ = pairedLbl.SetText("Paired devices")
	ui.peersBox, _ = walk.NewComposite(recvGroup)
	_ = ui.peersBox.SetLayout(walk.NewVBoxLayout())

	_, _ = walk.NewHSeparator(recvGroup)
	liveLbl, _ := walk.NewLabel(recvGroup)
	_ = liveLbl.SetText("Live receivers")
	ui.liveBox, _ = walk.NewComposite(recvGroup)
	_ = ui.liveBox.SetLayout(walk.NewVBoxLayout())

	// --- Auto-connect ---------------------------------------------------
	acGroup, _ := walk.NewGroupBox(mw)
	_ = acGroup.SetTitle("Auto-connect")
	_ = acGroup.SetLayout(walk.NewVBoxLayout())
	ui.autoStream, _ = walk.NewCheckBox(acGroup)
	_ = ui.autoStream.SetText("Auto-stream to trusted receivers")
	ui.autoStream.SetChecked(a.autoStream)
	ui.autoStream.CheckedChanged().Attach(func() {
		if ui.initializing {
			return
		}
		a.mu.Lock()
		a.autoStream = ui.autoStream.Checked()
		a.mu.Unlock()
		a.saveSettings()
	})
	ui.lastLabel, _ = walk.NewLabel(acGroup)
	_ = ui.lastLabel.SetText("Last connected: (none)")

	// --- Quality --------------------------------------------------------
	qGroup, _ := walk.NewGroupBox(mw)
	_ = qGroup.SetTitle("Quality")
	_ = qGroup.SetLayout(walk.NewVBoxLayout())
	addQuality := func(text string, id presetID) {
		rb, _ := walk.NewRadioButton(qGroup)
		_ = rb.SetText(text)
		rb.CheckedChanged().Attach(func() {
			if ui.initializing || !rb.Checked() {
				return
			}
			a.applyPresetID(id)
		})
		ui.qualityRadios[id] = rb
	}
	addQuality("Auto", presetAuto)
	addQuality("Lowest Latency", presetLowest)
	addQuality("Lossless", presetLossless)
	addQuality("Robust", presetRobust)
	addQuality("Advanced", presetAdvanced)
	if rb := ui.qualityRadios[a.preset]; rb != nil {
		rb.SetChecked(true)
	} else {
		ui.qualityRadios[presetAuto].SetChecked(true)
	}

	// --- Diagnostics ----------------------------------------------------
	dGroup, _ := walk.NewGroupBox(mw)
	_ = dGroup.SetTitle("Diagnostics")
	_ = dGroup.SetLayout(walk.NewVBoxLayout())
	ui.diag, _ = walk.NewTextEdit(dGroup)
	_ = ui.diag.SetReadOnly(true)
	_ = ui.diag.SetMinMaxSize(walk.Size{Width: 0, Height: 150}, walk.Size{})
	_ = ui.diag.SetText("Starting…")
	logBtn, _ := walk.NewPushButton(dGroup)
	_ = logBtn.SetText("Open log folder")
	logBtn.Clicked().Attach(func() { openLogFolder() })

	// --- Status line ----------------------------------------------------
	ui.status, _ = walk.NewLabel(mw)
	_ = ui.status.SetText("Ready")
	ui.status.SetTextColor(walk.RGB(0, 120, 0))

	ui.initializing = false
	return ui, nil
}

// openSettings opens the singleton window, or focuses it if already open.
func (a *app) openSettings() {
	a.settingsMu.Lock()
	if a.settingsUI != nil {
		ui := a.settingsUI
		a.settingsMu.Unlock()
		ui.focus()
		return
	}
	if a.settingsStarting {
		a.settingsMu.Unlock()
		return
	}
	a.settingsStarting = true
	a.settingsMu.Unlock()
	go a.runSettings()
}

// runSettings owns the walk message loop on a dedicated locked OS thread.
func (a *app) runSettings() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ui, err := newSettingsUI(a)
	if err != nil {
		a.settingsMu.Lock()
		a.settingsStarting = false
		a.settingsMu.Unlock()
		a.setError("settings window: " + err.Error())
		return
	}

	a.settingsMu.Lock()
	a.settingsUI = ui
	a.settingsStarting = false
	a.settingsMu.Unlock()

	ui.startRefresher()
	ui.mw.Show()
	ui.mw.Run()
	ui.stopRefresher()

	a.settingsMu.Lock()
	a.settingsUI = nil
	a.settingsMu.Unlock()
}

// focus brings an already-open singleton window to the foreground.
func (ui *settingsUI) focus() {
	ui.mw.Synchronize(func() {
		ui.mw.Show()
		_ = ui.mw.Activate()
	})
}

// startRefresher drives ~1s diagnostics/live-list updates on the UI thread.
func (ui *settingsUI) startRefresher() {
	ui.stop = make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ui.stop:
				return
			case <-t.C:
				ui.mw.Synchronize(ui.refresh)
			}
		}
	}()
}

func (ui *settingsUI) stopRefresher() {
	// Closing is enough; the refresher's select returns immediately on a closed
	// channel. (ui.stop is never reassigned, so there is no read/write race.)
	if ui.stop != nil {
		close(ui.stop)
	}
}

// setStatusText updates the persistent status line from any goroutine.
func (ui *settingsUI) setStatusText(text string, isErr bool) {
	ui.mw.Synchronize(func() {
		_ = ui.status.SetText(text)
		if isErr {
			ui.status.SetTextColor(walk.RGB(200, 0, 0))
		} else {
			ui.status.SetTextColor(walk.RGB(0, 120, 0))
		}
	})
}

// notifySettingsError feeds an error into the window's status line when open.
func (a *app) notifySettingsError(msg string) {
	a.settingsMu.Lock()
	ui := a.settingsUI
	a.settingsMu.Unlock()
	if ui != nil {
		ui.setStatusText("Error: "+msg, true)
	}
}

// refresh runs on the walk thread and repaints all dynamic state.
func (ui *settingsUI) refresh() {
	a := ui.app
	a.mu.Lock()
	host := a.host
	src := a.sourceLabel
	errText := a.lastError
	last := a.lastReceiver
	a.mu.Unlock()

	if errText != "" {
		_ = ui.status.SetText("Error: " + errText)
		ui.status.SetTextColor(walk.RGB(200, 0, 0))
	} else {
		_ = ui.status.SetText("Ready")
		ui.status.SetTextColor(walk.RGB(0, 120, 0))
	}

	if last == "" {
		_ = ui.lastLabel.SetText("Last connected: (none)")
	} else {
		_ = ui.lastLabel.SetText("Last connected: " + last)
	}

	_ = ui.diag.SetText(ui.diagnostics(host, src, errText))
	ui.syncLive(host)
	ui.syncPeers()
}

func (ui *settingsUI) diagnostics(host *engine.Host, src, errText string) string {
	var b strings.Builder
	state := "stopped"
	if host != nil {
		switch host.StreamState() {
		case engine.StreamWaiting:
			state = "waiting"
		case engine.StreamActive:
			state = "active"
		}
	}
	if src == "" {
		src = "System audio (default)"
	}
	fmt.Fprintf(&b, "Engine:    %s\n", state)
	fmt.Fprintf(&b, "Source:    %s\n", src)
	if errText != "" {
		fmt.Fprintf(&b, "Error:     %s\n", errText)
	}
	if host == nil {
		b.WriteString("Receivers: 0\n")
		return b.String()
	}
	rs := host.Receivers()
	fmt.Fprintf(&b, "Receivers: %d\n", len(rs))
	for _, r := range rs {
		fmt.Fprintf(&b, "  %s [%s, %s] codec=%s loss=%.1f%% jitter=%.1fms rtt=%.1fms buf=%.1fms vol=%.2f mute=%v\n",
			r.Name, r.State, r.QualityMode, r.Codec, r.LossPct, r.JitterMs, r.RTTMs, r.BufMs, r.Volume, r.Muted)
	}
	return b.String()
}

// applySource applies the checked source radio and records a label.
func (ui *settingsUI) applySource() {
	var chosen *sourceChoice
	for i := range ui.sourceChoices {
		if ui.sourceChoices[i].radio.Checked() {
			chosen = &ui.sourceChoices[i]
			break
		}
	}
	if chosen == nil {
		return
	}
	a := ui.app
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host == nil {
		a.setError("start streaming before choosing a source")
		return
	}
	switch chosen.kind {
	case sourceDevice:
		if err := host.SetSourceDevice(chosen.name); err != nil {
			a.setError("source: " + err.Error())
			return
		}
		a.setSourceLabel("Output: " + chosen.name)
	case sourceTestTone:
		if err := host.SetCaptureSource(audio.SourceTestTone); err != nil {
			a.setError("source: " + err.Error())
			return
		}
		a.setSourceLabel("Test tone")
	default:
		if err := host.SetSourceDevice(""); err != nil {
			a.setError("source: " + err.Error())
			return
		}
		a.setSourceLabel("System audio (default)")
	}
	a.clearError()
}

// syncLive creates/updates/removes one row per connected receiver.
func (ui *settingsUI) syncLive(host *engine.Host) {
	var rs []engine.ReceiverInfo
	if host != nil {
		rs = host.Receivers()
	}
	seen := make(map[string]bool, len(rs))
	for _, r := range rs {
		seen[r.DeviceID] = true
		lr := ui.liveRows[r.DeviceID]
		if lr == nil {
			lr = ui.addLiveRow(r)
			ui.liveRows[r.DeviceID] = lr
		}
		lr.update(r)
	}
	for id, lr := range ui.liveRows {
		if !seen[id] {
			lr.row.Dispose()
			delete(ui.liveRows, id)
		}
	}
}

func (ui *settingsUI) addLiveRow(r engine.ReceiverInfo) *liveRow {
	row, _ := walk.NewComposite(ui.liveBox)
	_ = row.SetLayout(walk.NewVBoxLayout())

	top, _ := walk.NewComposite(row)
	_ = top.SetLayout(walk.NewHBoxLayout())
	name, _ := walk.NewLabel(top)
	stats, _ := walk.NewLabel(top)
	_, _ = walk.NewHSpacer(top)

	bot, _ := walk.NewComposite(row)
	_ = bot.SetLayout(walk.NewHBoxLayout())
	mute, _ := walk.NewCheckBox(bot)
	_ = mute.SetText("Mute")
	volLbl, _ := walk.NewLabel(bot)
	_ = volLbl.SetText("Volume")
	vol, _ := walk.NewSlider(bot)
	vol.SetRange(0, 200)
	_ = vol.SetMinMaxSize(walk.Size{Width: 200}, walk.Size{Width: 200})

	lr := &liveRow{devID: r.DeviceID, row: row, name: name, stats: stats, mute: mute, vol: vol}
	mute.CheckedChanged().Attach(func() {
		if lr.updating {
			return
		}
		ui.app.setReceiverMuted(lr.devID, mute.Checked())
	})
	vol.ValueChanged().Attach(func() {
		if lr.updating {
			return
		}
		ui.app.setReceiverVolume(lr.devID, float64(vol.Value())/100.0)
	})
	return lr
}

func (lr *liveRow) update(r engine.ReceiverInfo) {
	lr.updating = true
	defer func() { lr.updating = false }()
	_ = lr.name.SetText(r.Name)
	_ = lr.stats.SetText(fmt.Sprintf("%s | %s | %s | loss %.1f%% jit %.1f rtt %.1f buf %.1f | %.2fx",
		r.State, r.QualityMode, r.Codec, r.LossPct, r.JitterMs, r.RTTMs, r.BufMs, r.Volume))
	if lr.mute.Checked() != r.Muted {
		lr.mute.SetChecked(r.Muted)
	}
	v := int(r.Volume*100 + 0.5)
	if lr.vol.Value() != v {
		lr.vol.SetValue(v)
	}
}

// syncPeers rebuilds the paired-device rows only when the peer set changes.
func (ui *settingsUI) syncPeers() {
	peers, err := ui.app.store.Peers()
	if err != nil {
		return
	}
	ids := make([]string, len(peers))
	for i, p := range peers {
		ids[i] = hex.EncodeToString(p.ID[:])
	}
	if equalStrings(ids, ui.peerIDs) {
		return
	}
	ui.peerIDs = ids
	for _, row := range ui.peerRows {
		row.Dispose()
	}
	ui.peerRows = nil

	for _, p := range peers {
		p := p
		row, _ := walk.NewComposite(ui.peersBox)
		_ = row.SetLayout(walk.NewHBoxLayout())
		name := p.Name
		if name == "" {
			name = "(unnamed)"
		}
		lbl, _ := walk.NewLabel(row)
		_ = lbl.SetText(fmt.Sprintf("%s  [%s]  paired %s",
			name, shortID(p.ID), time.Unix(p.PairedAt, 0).Format("2006-01-02 15:04")))
		_, _ = walk.NewHSpacer(row)
		btn, _ := walk.NewPushButton(row)
		_ = btn.SetText("Forget")
		idHex := hex.EncodeToString(p.ID[:])
		btn.Clicked().Attach(func() { ui.app.forgetPeer(idHex) })
		ui.peerRows = append(ui.peerRows, row)
	}
}

func shortID(id [16]byte) string {
	s := hex.EncodeToString(id[:])
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
