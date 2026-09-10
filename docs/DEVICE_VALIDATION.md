# Device Validation Runbook (Phase 12)

Real-device, end-to-end validation for **RemoteAU v2** (QUIC + datagrams,
6-digit PIN pairing, optional relay) on a physical jailbroken iPhone and the
target Windows PC.

> **Status: nothing in this runbook has been validated yet.** Every checkbox
> below starts unchecked. The Windows PC currently reports **zero active audio
> endpoints**, so the full path is exercised with the synthetic Test Tone
> source (`--source testtone`) rather than live WASAPI capture. See
> [Hardware-blocked items](#4-hardware-blocked-items-remain-blocked).

## 0. Pre-flight checks (confirm before starting)

These are current-tree facts to verify on the exact build under test; if a
check fails, record it as an engine gap rather than a device failure.

- [ ] **Test Tone source is accepted.** `remote-au serve --source testtone
      --tone-mode sine` must start. The synthetic source itself exists in
      `engine/internal/audio/synthetic.go`; confirm the CLI source parser
      (`engine/cmd/remote-au/main.go`, `parseCaptureSource`) accepts
      `testtone`. _If the build errors with `unknown capture source
      "testtone"`, the tone path is unavailable in that build (an engine-side
      one-line fix, not a device problem) — record it and validate what is
      reachable._
- [ ] **Pairing URL/QR is surfaced.** The v2 pairing URL scheme is
      `remoteau://pair?v=2&h=<ip>&p=<port>&id=<deviceidhex>&c=<code>`. The CLI
      prints the 6-digit code; the tray shows it in a popup. _If no URL/QR is
      emitted by the build under test, the QR item below cannot pass and should
      be recorded as blocked on that build._

## 1. Artifacts and where to get them

| Artifact | Where it comes from | Contains |
|---|---|---|
| `RemoteAU-unsigned-ipa` | GitHub Actions → **Build iOS IPA** (`.github/workflows/build-ipa.yml`, `build` job) | Unsigned `RemoteAU.ipa` (v2 engine + Opus XCFramework) |
| `remoteau-companion-deb` | GitHub Actions → **Build Jailbreak Companion** (`.github/workflows/build-jb.yml`) | Optional jailbreak tweak `.deb` |
| `remote-au-windows` | GitHub Actions → **Release Windows** (`.github/workflows/release-windows.yml`) | `remote-au-windows.zip`: `remote-au.exe`, `remote-au-tray.exe` (Opus), `NOTICE.txt` |
| `remote-au-windows-pcm` | Same workflow | PCM-only debug pair (no Opus) |
| Local engine build | `engine\scripts\build-windows.ps1` | `engine\bin\release\` (Opus) and `engine\bin\release-pcm\` (PCM) |

Download artifacts from the run's **Summary → Artifacts** section. Unzip the
IPA artifact to get `RemoteAU.ipa`.

## 2. Install

### iPhone

1. Install the unsigned IPA with **TrollStore** on the jailbroken device
   (or sign and install with Sideloadly). This is the core app and needs no
   jailbreak.
2. _Optional:_ install `remoteau-companion-deb` with the device's package
   manager (Sileo / Zebra) or `dpkg -i`. The core app must work with or without
   it; removing it must leave stock behavior intact.

### Windows

Pick one host:

- Tray app: run `remote-au-tray.exe` (starts the v2 host and shows pairing
  codes).
- CLI: run the synthetic-tone host (works with no audio endpoints):

  ```
  remote-au serve --source testtone --tone-mode sine
  ```

  Use `--tone-mode click` for one short pulse per second (latency checks).
  Add `--relay <relayAddr>` to register with a relay.

Both must be on the same LAN as the iPhone for the LAN checklist.

## 3. Validation checklist

### Basic

- [ ] **iPhone discovers the PC.** On Windows, start `remote-au-tray.exe` or
      `remote-au serve ...`. On the iPhone, open RemoteAU (auto-starts the
      receiver + finder) and confirm the PC appears under **Your PC** within a
      few seconds. _Expected:_ a tappable card showing the PC name and
      `ip:port · protocol v2`.
- [ ] **PIN pairing.** Tap the PC card; enter the 6-digit code shown on the PC
      (tray popup or CLI `*** PAIRING: enter code ... ***`) into the pairing
      sheet and tap **Connect**. _Expected:_ connects and streams; the PC
      appears under **Paired PCs** on the iPhone.
- [ ] **QR pairing.** Have the PC surface the pairing URL/QR
      (`remoteau://pair?v=2&h=<ip>&p=<port>&id=<deviceidhex>&c=<code>`; the CLI
      prints the URL, the tray shows the code). Scan the QR / open the URL on
      the iPhone. _Expected:_ pairing completes without typing the code
      (see [Pre-flight checks](#0-pre-flight-checks-confirm-before-starting) if
      the URL/QR is not produced by this build).
- [ ] **Reconnect after app relaunch.** Force-quit RemoteAU and relaunch it
      (with the host still running). _Expected:_ it reconnects to the known PC
      without re-entering the PIN (pairing persisted).
- [ ] **AirPods output.** Connect AirPods, then start a stream. _Expected:_
      audio is audible through the AirPods; the **Output route** row in Live
      statistics names the AirPods.
- [ ] **PCM plays.** With the default codec (PCM), start the tone stream.
      _Expected:_ a continuous 440 Hz sine (or 1 Hz clicks) with no dropouts.
- [ ] **Opus plays.** On the iPhone, expand **Advanced controls** and enable
      **Prefer Opus**, then reconnect. _Expected:_ audio still plays and the
      **Codec** row in Live statistics reads Opus.
- [ ] **PCM ↔ Opus switching via presets (no app restart).** While streaming,
      change the **Quality** preset so the host switches codec. _Expected:_ the
      stream continues across the switch, the **Codec** row updates, and the app
      does not restart or re-pair.

### Background

- [ ] **Lock iPhone → audio continues.** Lock the device mid-stream.
      _Expected:_ audio keeps playing.
- [ ] **Leave locked ≥ 10 minutes.** Keep the stream running with the screen
      locked for at least 10 minutes. _Expected:_ audio continues without
      stopping or being killed by iOS.
- [ ] **Reopen → state/stats sane.** Unlock and reopen the app. _Expected:_
      still streaming; Live statistics values (buffer depth, loss, jitter,
      packet rate) look plausible and keep advancing; no stuck "Reconnecting…"
      state.

### AirPods

- [ ] **Disconnect AirPods → resume on reconnect.** Disconnect the AirPods
      (or turn them off) and reconnect. _Expected:_ audio resumes on the
      AirPods without force-quitting the app.
- [ ] **Switch output route → audio resumes.** Switch between AirPods and the
      phone speaker (or another Bluetooth route). _Expected:_ the stream keeps
      working and follows the new route; the **Output route** row updates.

### Network

- [ ] **Wi-Fi off → on.** Disable Wi-Fi on the iPhone, wait a few seconds, then
      re-enable it. _Expected:_ RemoteAU reconnects automatically and audio
      resumes.
- [ ] **No re-pairing.** During the Wi-Fi reconnect above, confirm no PIN is
      requested and no pairing UI appears. _Expected:_ the existing trust is
      reused.
- [ ] **Resumed session.** Confirm playback resumes from the live stream
      (current audio), not from a stale buffer. _Expected:_ in the v2 path the
      RESUME flow re-establishes the session; audio is current, statistics
      continue.

### Sender

- [ ] **Stop/start stream.** Stop the host (`Ctrl-C` / tray **Stop streaming**)
      and start it again. _Expected:_ the iPhone reconnects and resumes.
- [ ] **Restart Windows host.** Fully quit and relaunch `remote-au`/the tray
      app. _Expected:_ the iPhone reconnects to the same PC without re-pairing.
- [ ] **Quality preset changes.** Cycle **Quality** presets (Auto, Lowest
      Latency, Lossless, Robust) while streaming. _Expected:_ each takes effect
      without an app restart; the buffer target/latency pill responds.
- [ ] **Mute/unmute.** Use the iPhone's per-stream mute (and/or the tray's
      **Mute**) and toggle it. _Expected:_ audio silences and returns; the
      stream stays connected.
- [ ] **Per-receiver volume (settings surface).** With the intended
      settings/volume surface, change the volume for one receiver. _Expected:_
      only that receiver's output changes. _Current tray surface:_ the
      **Receivers** submenu exposes per-receiver mute/unmute; record whether a
      dedicated per-receiver volume control is present in the build under test.

### Relay

- [ ] **Relay path.** On one machine run `remote-au relay` (default
      `:47020`). On the PC run `remote-au serve --source testtone --tone-mode
      sine --relay <relayAddr>`. Connect the iPhone via the
      `remoteau://pair?...` manual/relay path if the build provides it.
      _Expected:_ audio flows through the relay. _If the iPhone cannot be given
      a relay address in this build, this item is blocked:_ relay validation
      then requires a second network path — e.g. put the phone on
      cellular/hotspot while the host and relay remain reachable — and a client
      that can dial the relay. Direct NAT traversal is future work; a dumb
      relay only ever sees sealed ciphertext.

## 4. Hardware-blocked items (remain blocked)

These cannot pass until the Windows PC has at least one **active playback/
capture endpoint**. They are environment limitations, not app bugs. Leave the
boxes unchecked and record them as blocked.

- [ ] **Real WASAPI system-audio capture (content and quality).** Verifying
      that actual system audio is captured and sounds correct requires an
      active Windows render endpoint; the Test Tone source only validates the
      engine → codec → transport → receiver path downstream of capture.
- [ ] **Per-app process capture.** The process-loopback capture path (see
      `docs/PER_APP_CAPTURE.md`) needs an active endpoint and real audio
      sessions to validate; untested on this PC.
- [ ] **Capture-device hot-switch following.** Changing/adding a default
      capture device at runtime cannot be observed without an active endpoint
      to switch to.

## 5. Bug-report template

Copy this block for every failure and attach the raw files.

```
## RemoteAU device-validation bug

- Artifact/build:  <name + GitHub Actions run URL or commit SHA>
- Windows host:    <tray | CLI>   command: <exact command line>
- iPhone:          <model, iOS version, jailbroken? app build>
- Checklist item:  <paste the failing "- [ ]" line>
- Steps to reproduce:
    1.
    2.
    3.
- Expected:
- Actual:
- Logs:
    - engine.log: attached (Windows: %AppData%\RemoteAU\engine.log)
    - iOS statistics: screenshot of the "Live statistics" view; attach a JSON
      export if the build offers one
- When: <local time + timezone>
```

### What to collect

- **`%AppData%\RemoteAU\engine.log`** — the Windows host log (written by the
  tray app and by `serve`). Attach the whole file, not a snippet.
- **iOS state / statistics** — screenshot the **Live statistics** view
  (buffer depth, target, jitter, loss, packet rate, bitrate, underruns,
  concealed/PLC, late, reordered, codec, drift, output route). Include the app
  state shown in the hero card (Connected / Searching / Reconnecting).
  Attach a JSON export if one is available; otherwise transcribe the rows.
- **Which checklist item failed** — paste the exact line from Section 3.
- **Exact steps** — from a known state (host started? app relaunched?),
  numbered, so the failure can be reproduced.
