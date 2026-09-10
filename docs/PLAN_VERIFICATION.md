# PLAN_VERIFICATION.md — status

Verification of the current repository against `remote_au_ios_codex_plan.md`,
updated after the **Section-2 completion pass** and the **first live run on the
real Windows PC with headphones connected** (2026-09-10). Supersedes the earlier
audits (preserved in git history: audit `de77e8c`, resolution pass `122224f`).

**How to read the status tags:**

- **[impl]** — implemented in code. "Implemented" means the code exists and
  compiles (Go verified locally; Swift verified by the CI iOS build), **not**
  that it is hardware-validated.
- **[auto]** — exercised automatically by tests/CI, run locally where noted.
- **[live]** — validated by running on the real Windows PC with audio hardware
  (v1/v2 playback, pairing, pinning). This is **not** device validation.
- **[device]** — validated on the physical iPhone. **Nothing is tagged
  [device] yet**; `docs/DEVICE_VALIDATION.md` is entirely unchecked.
- **[hw]** — blocked by environment/hardware.

Evidence: `go build ./...` ✅, `go vet ./...` ✅ (one documented
`unsafe.Pointer` exception in `processloopback_windows.go`), and
`powershell -File engine/runtests.ps1` → **All tests passed** ✅ (all Go
packages). Swift/iOS is source-audited only locally (no Mac); iOS builds run in
CI.

---

## Section-2 gaps — now closed

The earlier "not implemented" list has been addressed in code:

| Gap | Status |
|---|---|
| iOS source selection UI (System / Test-tone / custom device / per-app → SET_SOURCE) | [impl] `ios/RemoteAU/App/RootView.swift`, `RemoteAUCore.swift` |
| iOS quality-mode sync to host | [impl] `SetQualityMode` over the bridge |
| iOS **v2 live stats** (real counters, not zeros) | [impl] `SetStatsSink` + `LatestStats`; consumed by `StatsView.swift` |
| iOS named profiles | [impl] profile picker in the receiver UI |
| iOS advanced min/max jitter bounds | [impl] `AdaptiveJitterPolicy.swift` |
| iOS session recording to WAV | [impl] `SessionRecorder.swift` |
| Windows per-app process picker (Only / Exclude) | [impl] `settings.go` source picker |
| Windows QR pairing window | [impl] `pairing.go` (go-qrcode → walk) |
| Windows profiles + selected auto-connect | [impl] `presets.go`; `auto_receiver_id` in `settings_store.go` |
| Go bridge: `SetSource`, `SetQualityMode`, `SetStatsSink`, `StatsJSON`, `ConnectViaRelay` | [impl] `engine/mobile/mobile.go`, `engine/internal/engine/client.go` |
| Stats JSON carries `sample_rate` / `channels` | [impl] `ClientStatsSnapshot` |
| Jailbreak Control Center toggle tweak | [impl] `jb/RemoteAUControlCenter.xm` (+ headers, plist) — confidence low/medium, **uncompiled** |
| `PROTOCOL_V2.md` fallback-transport overstatement | [impl] corrected — QUIC (TLS 1.3 + DATAGRAM) is the only implemented transport |

---

## Goal (top of plan)

| Requirement | Status |
|---|---|
| Open source | [impl] AGPL-3.0 (`LICENSE`, `engine/LICENSE`) |
| Native WASAPI loopback capture | [impl] [live] (v1 loopback → receiver → playback on the real PC) |
| Native iPhone receiver | [impl] [device] pending |
| Works locked/backgrounded | [impl] [device] pending |
| Plays through AirPods | [impl] [device] pending |
| Low latency on LAN | [impl] (10–20 ms adaptive targets) |
| Robust on imperfect Wi-Fi | [impl] (reorder+PLC+FEC+adaptive buffer) |
| Secure | [impl] [live] (QUIC/TLS + PIN/QR pairing + **public-key** pinning; second run reused the pin with no re-pairing) |
| PCM **and** Opus | [impl] Opus is a normal release feature (CI builds libopus for Windows + iOS); locally only PCM is [live]-exercised (no libopus toolchain here) |
| Polished GUIs (SwiftUI + Windows tray/window) | [impl] (Windows window smoke-tested; iOS visual polish unverified locally) |
| CLI secondary | [impl] `devices/selftest/recv/send/serve/recv2/relay` |
| No Mac required | [impl] CI builds IPA + framework (artifacts pending first CI run) |
| No SonoBus | [impl] never referenced |

## Architecture direction

| Item | Status |
|---|---|
| Windows: WASAPI → engine → PCM/Opus → secure transport | [impl] [live] (v1 + v2) |
| iPhone: receive → reorder/jitter → loss recovery → drift → decode → AVAudioEngine → AirPods | [impl] [device] pending (v1 Swift engine; v2 path via Go client + shared ring) |
| Encrypted, authenticated, persistently paired | [impl] [auto] [live] (`TestClientRejectsUntrustedHost`, relay/pairing tests; pubkey pin survives restart) |
| Reliable control stream + unreliable datagrams, no HOL blocking | [impl] [auto] (QUIC host↔client tests) |
| QUIC + QUIC DATAGRAM, transport abstract | [impl] [auto] (quic-go; `transport/v2` seam) |
| No WebRTC | [impl] not used |

## Phase-by-phase

### Phase 1 — Analysis + structure
[impl] `docs/PHASE1_ANALYSIS.md`; concerns split into `audio`/`codec`/
`protocol`(+v2)/`transport`(+v2)/`discovery`/`session`/`pairing`/`quality`/
`engine`/`stats`. v1 behaviour preserved (v1 tests pass). [auto]

### Phase 2 — Native iOS receiver for stock remote-au
[impl] SwiftUI app: v1 `RAUU` parsing, discovery **responder** (byte-exact v1
announce) and a **finder** for PCs, AVAudioEngine + RT-safe ring,
background/locked audio, interruptions, route changes, watchdog reconnect.
Interop bugs B1/B2/B3 fixed; v2 hosts also emit a type-3 announce.
[auto] Go-side discovery/announce tests. [device] pending.

### Phase 3 — Better receiver engine
[impl] Adaptive jitter policy (Auto/Lowest/Lossless/Robust/Advanced) with
fast-up/slow-down + hysteresis; burst-aware target; FEC advisory; continuous
clock-drift micro-resampling (±0.1 %); capture-timestamp discontinuity
detection with controlled re-prime; PCM PLC = decay-hold **plus crossfade
into the next good packet**; separate overrun accounting; RT-safe
try-lock/alloc-free render path. Advanced mode now exposes explicit **min/max
jitter bounds**. [auto] `clientMedia` reorder/PLC/stats tests, ring/converter
tests, synthetic end-to-end. Device behaviour [device] pending.

### Phase 4 — Protocol v2 + secure transport
[impl] Negotiated caps (codec, rate, channels, frame duration, FEC, DTX,
complexity, appID, formatGen, stream id, quality mode, source selection,
receiver state), HELLO/HELLO_OK, STREAM_START/ACK/STOP, FORMAT_UPDATE/ACK,
STATS, VOLUME, PING/PONG, RESUME/RESUME_OK, plus MSG_QUALITY_MODE,
MSG_SET_SOURCE(+ACK), MSG_RECEIVER_STATE. v1 kept as legacy.
[auto] protocol round-trip tests; QUIC host↔client; resume; volume/mute;
multi-receiver fanout; relay end-to-end. [live] v2 `serve`+`recv2`.
`PROTOCOL_V2.md` now states plainly that QUIC is the only implemented carrier
(per-packet UDP+AES is design-only).

### Phase 5 — Secure pairing + identity
[impl] ECDH P-256 + HKDF, mutual PIN verification, transcript-bound,
constant-time compares; DPAPI store (Windows, with correct
`CryptUnprotectData` arity + self-heal on unreadable blobs); **Keychain**
trust store on iOS with migration from the old file store; host pinning on the
client after first pairing. Pinning now uses the **stable public-key
fingerprint** (`PublicKeyFingerprint`, `SubjectPublicKeyInfo`), not the
certificate DER, so regenerated certs still match. Discovery never conveys
trust. [auto] pairing exchange tests (success/wrong-PIN), identity round-trip,
DPAPI round-trip + self-heal, untrusted-host rejection, pubkey-pin stability
test. [live] second run reconnected with **no re-pairing**. [device] Keychain
runtime pending.

### Phase 6 — Opus + resilience
[impl] PCM passthrough + Opus (bitrate/FEC/DTX/PLC/complexity/low-delay
app), in-band **FEC recovery** from the next held packet, adaptive quality
controller (smoothing + cooldown) driving bitrate/FEC and PCM↔Opus switches
via FORMAT_UPDATE with a dwell timer; smart decoder rebuild only when the
negotiated format actually changes.
[auto] FEC/PLC unit tests; adaptive-quality tests. Opus code paths compile
under `-tags opus` in CI; **not executable here** (no libopus toolchain) —
honest limitation.

### Phase 7 — Polished iOS GUI
[impl] Device cards (v2 sorted first, legacy greyed), pairing sheet (PIN +
QR scanner), manual-host fallback, paired-device list with Forget, quality
presets, advanced codec controls, live stats (incl. FEC advisory, overruns,
discontinuities, drift, burst, codec), persistence, empty/reconnect/error
states, dark/light, accessibility labels. Now also: **source selection
(System / Test-tone / custom device / per-app)**, quality-mode sync, **live v2
stats from the Go bridge**, **named profiles**, **min/max jitter bounds**, and
**session recording to WAV**.
[device] visual + runtime validation pending.

### Phase 8 — Windows tray + settings GUI
[impl] Tray (start/stop, mute, receivers, source, quality, autostart, open
log folder, open settings, connect-to-last) **plus a native walk settings
window** (source picker incl. **per-app Only/Exclude process picker**, paired +
live receiver lists with Forget/Mute/volume, **selected auto-connect via
`auto_receiver_id`**, quality radios, **named profiles**, diagnostics, error
line), and a **QR pairing window** (go-qrcode rendered into a walk image).
[auto] build/vet/gofmt; smoke test (process runs; window opens, title
verified). [device] user validation pending.

### Phase 9 — Per-application capture
[impl] Pure-Go process loopback (`ActivateAudioInterfaceAsync` +
`AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`, agile completion handler via
`GetActivateResult` + `VT_BLOB` + 12-byte activation params, multi-PID mixing,
exclude mode), audio-session enumeration for the picker, host integration
(`SetCaptureApps`, SET_SOURCE kind 3 `p:`/`x:` names), documented in
`PROTOCOL_V2.md`.
[auto] name-parsing + validation tests only. **Runtime not validated** — see
below: live activation reports the documented *"target process has no
capturable WASAPI session"* for the test player (winmm/`SoundPlayer`), and
`ListAudioProcesses` fails on this PC's HyperX driver with `E_NOINTERFACE`
(no session manager). The picker and runtime remain unvalidated ([hw]-adjacent:
needs a WASAPI-session app plus a driver exposing the session manager).

### Phase 10 — Opus as a normal Windows release feature
[impl] `release-windows.yml` builds pinned libopus (SHA-verified) with
MinGW-w64 and produces release binaries with `-tags opus,nolibopusfile`; a
PCM-only build is a separate debug artifact; **missing libopus fails the
release loudly**. `scripts/build-windows.ps1` is the local equivalent.
Artifacts are **pending CI** — not produced or verified here.

### Phase 11 — Built-in synthetic test source
[impl] Engine-internal Test Tone (sine @ −12 dBFS with slow pan; click/
impulse mode) bypassing WASAPI; `serve --source testtone --tone-mode
sine|click`; documented as a diagnostic, never the default.
[auto] **`TestSyntheticSourceEndToEnd` proves real sine audio flows
source→codec→QUIC→client (peak amplitude asserted).**

### Phase 12 — Real iPhone validation
[impl] Runbook `docs/DEVICE_VALIDATION.md` with the full unchecked checklist
(basic / background / AirPods / network / sender / relay) and the exact
artifact + install steps. [device] **Not performed** — every box is unchecked;
this environment cannot drive the phone.

### Phase 13 — Optional jailbreak companion
[impl] Source + package layout (Theos tweak, plist, LaunchDaemon under
`layout/`, postinst/prerm, Makefile fixed for Theos), a **Control Center
toggle** (`RemoteAUControlCenter.xm`, confidence low/medium, uncompiled), and a
`.github/workflows/build-jb.yml` that builds the `.deb` on Ubuntu and uploads
it. Core IPA never depends on it. [auto] CI-only, **pending first CI run**.
[device] optional.

### Phase 14 — Additional features
[impl] profiles (named, persisted, CLI `--profile`), recording (per-stream
WAV, pre-volume, `serve --record`), per-app capture (Phase 9), WAN relay
(`remote-au relay`, sealed envelopes), plus the iOS-side counterparts (named
profiles, source/quality controls, WAV session recording). Reverse-microphone/
full-duplex: **explicit non-goal**.
[auto] WAV/profile/relay tests.

### Phase 15 — Cleanup + truth pass
[impl] root `LICENSE` (AGPL-3.0), `THIRD_PARTY_NOTICES.md` (licenses verified
from disk: quic-go MIT, systray Apache-2.0, lxn MIT/BSD, hraban/opus MIT text,
go-qrcode MIT, opus BSD-3, x/* BSD-3, EchoWarp MIT — ideas only, no code
bundled), stale TODOs removed, release workflow NOTICE corrected, and the
`PROTOCOL_V2.md` fallback-transport overstatement fixed. This document.

---

## Automated test inventory (what [auto] actually covers)

Engine Go suite (all passing): v1 protocol/UDP/TCP; discovery (incl. the fixed
finder race); mixer/jitter/ring; **v2 protocol** (media + control round-trips
incl. new control messages); **pairing** (exchange, wrong PIN, identity,
DPAPI round-trip + self-heal, **public-key pin stability**); **engine**
(publish/capture-free QUIC host↔client; PCM media delivery; **synthetic
test-tone end-to-end**; resume-after-reconnect; untrusted-host rejection;
per-receiver volume/mute; multi-receiver fanout; **FEC/PLC**; **relay
end-to-end**; pairing URL format); WASAPI converters + per-app name parsing;
**mobile bridge** (SetSource/QualityMode guards, **live stats flow incl.
`sample_rate`/`channels`**, Keychain-store write-through/migration with a fake
source); quality mappings.

Not covered automatically: Swift (uncompiled here), Opus runtime (no libopus
here), live WASAPI/process capture ([live] only for system loopback), GUI
visuals, CI pipelines, and anything on the physical iPhone.

## Live-validated (real Windows PC, headphones connected)

Performed this session against a real endpoint (all **[live]**, not [device]):

- **WASAPI loopback capture → v1 receiver → playback**: works.
- **v2 `serve` + `recv2` + PIN pairing + playback**: works (`stream active`,
  audio played).
- **Certificate pinning across restarts**: a second run reconnected with **no
  re-pairing** (public-key pin is stable).
- New CLI/UX: `serve --source testtone`, `--record`, `--profile`, `--relay`,
  `--app-pid`/`--exclude-pid`, `--data-dir`; `recv2` `RAU_PIN`/`RAU_PIN_FILE`
  automation hooks.

These runs found and fixed **five real bugs**:

1. Empty device friendly names (PROPVALUE read before clear).
2. COM apartment lifetime in capture / render / process-loopback.
3. A render-path **buffer overflow** in the S16→float conversion.
4. **Unstable certificate pinning** — now pins the public key, not the cert DER.
5. Process-loopback activation API (`GetActivateResult` + `VT_BLOB` + 12-byte
   params).

## Device-validation pending

`docs/DEVICE_VALIDATION.md` exists with 28 unchecked items (basic /
background / AirPods / network / sender / relay). The iOS Swift code has never
been compiled locally. No item anywhere is claimed [device]-validated.

## Hardware / CI-blocked

- **[hw] Per-app (process-loopback) capture runtime** — activation returns the
  documented "target process has no capturable WASAPI session" for the test
  player (winmm/`SoundPlayer`), and `ListAudioProcesses` fails on this PC's
  HyperX driver with `E_NOINTERFACE`. Needs a WASAPI-session app plus a driver
  that exposes the session manager.
- **[hw]** Capture-device hot-switch following a real endpoint change.
- **CI-blocked artifacts** — IPA + Opus XCFramework, Windows release (libopus),
  and the jailbreak `.deb` workflows exist but have **not run**: the GitHub
  remote is only now being created. Marked **pending CI**, not done.

System loopback itself is now [live]-validated, so the old blanket "[hw] no
active audio endpoint" limitation no longer applies to the main path.

## Explicit non-goals (not "incomplete")

Full-duplex / reverse microphone; non-iPhone receiver GUIs (Windows/macOS/
Linux clients); conference calling; cloud accounts; telemetry; virtual audio
drivers; UDP+AES fallback transport (design only); NAT traversal / ICE.

## Honest caveat

"Implemented" means the code exists and compiles (Go verified locally; Swift
verified by the CI iOS build). It does **not** mean hardware-validated: the
iPhone runbook is unrun, the per-app path is unvalidated, and CI artifacts are
pending. Live Windows validation of the core audio path and pairing/pinning is
real but is tagged **[live]**, never **[device]**.
