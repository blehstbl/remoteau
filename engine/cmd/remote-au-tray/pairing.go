package main

import (
	"runtime"
	"time"

	"github.com/lxn/walk"
	"github.com/skip2/go-qrcode"
)

// pairingUI is a small singleton window that shows a live pairing code and a
// scannable QR image (or an instructional placeholder before pairing starts).
// It runs its own walk message loop on a locked OS thread, mirroring
// settingsUI. Secrets (code/url) are only ever held in memory here and are
// dropped when the window closes.
type pairingUI struct {
	app      *app
	mw       *walk.MainWindow
	title    *walk.Label
	code     *walk.Label
	urlField *walk.LineEdit
	img      *walk.ImageView
	note     *walk.Label
	bitmap   *walk.Bitmap

	hasCode bool
	stop    chan struct{}
	reset   chan struct{}
}

// newPairingUI builds the window on the caller's locked walk thread.
func newPairingUI(a *app, code, url string, infoOnly bool) (*pairingUI, error) {
	mw, err := walk.NewMainWindow()
	if err != nil {
		return nil, err
	}
	ui := &pairingUI{app: a, mw: mw}
	_ = mw.SetTitle("RemoteAU — Pairing")
	_ = mw.SetLayout(walk.NewVBoxLayout())
	_ = mw.SetSize(walk.Size{Width: 400, Height: 560})

	ui.title, _ = walk.NewLabel(mw)
	_ = ui.title.SetText("Pairing")

	ui.code, _ = walk.NewLabel(mw)
	if font, err := walk.NewFont("Segoe UI", 28, walk.FontBold); err == nil {
		ui.code.SetFont(font)
	}
	_ = ui.code.SetText("------")

	ui.urlField, _ = walk.NewLineEdit(mw)
	_ = ui.urlField.SetReadOnly(true)
	_ = ui.urlField.SetText("")

	ui.img, _ = walk.NewImageView(mw)
	_ = ui.img.SetMinMaxSize(walk.Size{Width: 264, Height: 264}, walk.Size{Width: 264, Height: 264})

	ui.note, _ = walk.NewLabel(mw)

	closeBtn, _ := walk.NewPushButton(mw)
	_ = closeBtn.SetText("Close")
	closeBtn.Clicked().Attach(func() { mw.Close() })

	ui.setContent(code, url, infoOnly)
	return ui, nil
}

// setContent swaps between a live code/QR and an informational placeholder.
// It must run on the walk thread.
func (ui *pairingUI) setContent(code, url string, infoOnly bool) {
	live := !infoOnly && code != "" && url != ""
	ui.hasCode = live
	if !live {
		_ = ui.title.SetText("Show pairing QR")
		_ = ui.note.SetText("Start pairing from the phone (tap the PC in the app); " +
			"the QR will appear here.")
		_ = ui.code.SetText("------")
		_ = ui.urlField.SetText("")
		ui.img.SetVisible(false)
		return
	}
	_ = ui.title.SetText("Scan to pair")
	_ = ui.note.SetText("Enter this code on the phone, or scan the QR.")
	_ = ui.code.SetText(code)
	_ = ui.urlField.SetText(url)
	ui.img.SetVisible(true)
	ui.renderQR(url)
}

// renderQR draws the pairing URL into the ImageView. walk.NewBitmapFromImage
// exists in the pinned walk version, so no PNG temp-file fallback is needed.
func (ui *pairingUI) renderQR(url string) {
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return
	}
	bmp, err := walk.NewBitmapFromImage(qr.Image(256))
	if err != nil {
		return
	}
	if ui.bitmap != nil {
		ui.bitmap.Dispose()
	}
	ui.bitmap = bmp
	_ = ui.img.SetImage(bmp)
}

// update replaces the window contents from any goroutine.
func (ui *pairingUI) update(code, url string, infoOnly bool) {
	ui.mw.Synchronize(func() { ui.setContent(code, url, infoOnly) })
	if !infoOnly {
		ui.kickAutoClose()
	}
}

// focus brings an already-open singleton window to the foreground.
func (ui *pairingUI) focus() {
	ui.mw.Synchronize(func() {
		ui.mw.Show()
		_ = ui.mw.Activate()
	})
}

// startAutoClose closes the window 5 minutes after the latest content update.
func (ui *pairingUI) startAutoClose() {
	ui.stop = make(chan struct{})
	ui.reset = make(chan struct{}, 1)
	go func() {
		for {
			t := time.NewTimer(5 * time.Minute)
			select {
			case <-ui.stop:
				t.Stop()
				return
			case <-ui.reset:
				t.Stop()
				continue
			case <-t.C:
				ui.mw.Synchronize(func() { ui.mw.Close() })
				return
			}
		}
	}()
}

func (ui *pairingUI) kickAutoClose() {
	if ui.reset != nil {
		select {
		case ui.reset <- struct{}{}:
		default:
		}
	}
}

func (ui *pairingUI) stopAutoClose() {
	if ui.stop != nil {
		close(ui.stop)
	}
}

// showPairing displays (or refreshes) the singleton pairing window with a live
// code and QR. Called from the host's OnPairingInfo callback.
func (a *app) showPairing(code, url string) {
	a.pairingMu.Lock()
	if a.pairingStarting {
		a.pairingMu.Unlock()
		return
	}
	if ui := a.pairingUI; ui != nil {
		a.pairingLive = true
		a.pairingMu.Unlock()
		ui.update(code, url, false)
		return
	}
	a.pairingStarting = true
	a.pairingLive = true
	a.pairingMu.Unlock()
	go a.runPairing(code, url, false)
}

// showPairingInfo opens the informational pairing window (no code yet).
func (a *app) showPairingInfo() {
	a.pairingMu.Lock()
	if a.pairingStarting {
		a.pairingMu.Unlock()
		return
	}
	if ui := a.pairingUI; ui != nil {
		a.pairingMu.Unlock()
		ui.focus()
		return
	}
	a.pairingStarting = true
	a.pairingMu.Unlock()
	go a.runPairing("", "", true)
}

// closePairingIfLive closes the pairing window only when it is showing a live
// pairing attempt (i.e. not the informational placeholder).
func (a *app) closePairingIfLive() {
	a.pairingMu.Lock()
	live := a.pairingLive
	ui := a.pairingUI
	a.pairingLive = false
	a.pairingMu.Unlock()
	if !live || ui == nil {
		return
	}
	ui.mw.Synchronize(func() { ui.mw.Close() })
}

// runPairing owns the walk message loop for the pairing window.
func (a *app) runPairing(code, url string, infoOnly bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	ui, err := newPairingUI(a, code, url, infoOnly)
	if err != nil {
		a.pairingMu.Lock()
		a.pairingStarting = false
		a.pairingLive = false
		a.pairingMu.Unlock()
		a.setError("pairing window: " + err.Error())
		return
	}

	a.pairingMu.Lock()
	a.pairingUI = ui
	a.pairingStarting = false
	a.pairingMu.Unlock()

	ui.startAutoClose()
	ui.mw.Show()
	ui.mw.Run()
	ui.stopAutoClose()

	if ui.bitmap != nil {
		ui.bitmap.Dispose()
		ui.bitmap = nil
	}

	a.pairingMu.Lock()
	a.pairingUI = nil
	a.pairingLive = false
	a.pairingMu.Unlock()
}
