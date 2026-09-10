# PLAN_VERIFICATION.md — FINAL status

Verification of the finished repository against `remote_au_ios_codex_plan.md`,
written after the completion pass (2026-09-10). Supersedes the earlier audits
(preserved in git history: the original audit commit `de77e8c` and the
resolution pass `122224f`).

**How to read the status tags** (per the plan's final-cleanup requirement):

- **[impl]** — implemented in code.
- **[auto]** — exercised automatically (unit/integration tests or CI, run
  locally where noted).
- **[device]** — validated on the physical iPhone. **Nothing is tagged
  [device] yet**; the runbook is `docs/DEVICE_VALIDATION.md` and all its
  boxes are unchecked.
- **[hw]** — blocked by this Windows PC having **zero active audio
  endpoints**. This is an environment limitation, not an app defect.

Evidence commands: `go build ./...` ✅, `go vet ./...` ✅ (one documented
`unsafe.Pointer` exception in `processloopback_windows.go`), and
`powershell -File engine/runtests.ps1` → **All tests passed** ✅ (all Go
packages). Swift/iOS is source-audited only (no Mac locally); iOS builds run
in CI.

---

## Goal (top of plan)

| Requirement | Status |
|---|---|
| Open source | [impl] AGPL-3.0 (`LICENSE`, `engine/LICENSE`) |
| Native WASAPI loopback capture | [impl] [hw] live audio pending |
| Native iPhone receiver | [impl] [device] pending |
| Works locked/backgrounded | [impl] [device] pending |
| Plays through AirPods | [impl] [device] pending |
| Low latency on LAN | [impl] (10–20 ms adaptive targets) |
| Robust on imperfect Wi-Fi | [impl] (reorder+PLC+FEC+adaptive buffer) |
| Secure | [impl] (QUIC/TLS + PIN/QR pairing + fingerprint pinning both sides) |
| PCM **and** Opus | [impl] Opus is a normal release feature (CI builds libopus for Windows + iOS); locally only PCM is exercised (no libopus toolchain) |
| Polished GUIs (SwiftUI + Windows tray/window) | [impl] (Windows window smoke-tested; iOS visual polish unverified locally) |
| CLI secondary | [impl] `devices/selftest/recv/send/serve/recv2/relay` |
| No Mac required | [impl] CI builds IPA + framework |
| No SonoBus | [impl] never referenced |

## Architecture direction

| Item | Status |
|---|---|
| Windows: WASAPI → engine → PCM/Opus → secure transport | [impl] [hw] |
| iPhone: receive → reorder/jitter → loss recovery → drift → decode → AVAudioEngine → AirPods | [impl] [device] pending (v1 Swift engine; v2 path via Go client + shared ring) |
| Encrypted, authenticated, persistently paired | [impl] [auto] (`TestClientRejectsUntrustedHost`, relay/pairing tests) |
| Reliable control stream + unreliable datagrams, no HOL blocking | [impl] [auto] (QUIC host↔client tests) |
| QUIC + QUIC DATAGRAM, transport abstract | [impl] [auto] (quic-go; `transport/v2` seam) |
| No WebRTC | [impl] not used |

## Phase-by-phase (final)

### Phase 1 — Analysis + structure
[impl] `docs/PHASE1_ANALYSIS.md`; concerns split into `audio`/`codec`/
`protocol`(+v2)/`transport`(+v2)/`discovery`/`session`/`pairing`/`quality`/
`engine`/`stats`. v1 behaviour preserved (v1 tests pass). [auto]

### Phase 2 — Native iOS receiver for stock remote-au
[impl] SwiftUI app: v1 `RAUU` parsing, discovery **responder** (byte-exact v1
announce now) and a **finder** for PCs, AVAudioEngine + RT-safe ring,
background/locked audio, interruptions, route changes, watchdog reconnect.
Interop bugs B1/B2/B3 fixed; v2 hosts also emit a type-3 announce.
[auto] Go-side discovery/announce tests. [device] pending.

### Phase 3 — Better receiver engine
[impl] Adaptive jitter policy (Auto/Lowest/Lossless/Robust/Advanced) with
fast-up/slow-down + hysteresis; burst-aware target; FEC advisory; continuous
clock-drift micro-resampling (±0.1 %); capture-timestamp discontinuity
detection with controlled re-prime; PCM PLC = decay-hold **plus crossfade
into the next good packet**; separate overrun accounting; RT-safe
try-lock/alloc-free render path.
[auto] `clientMedia` reorder/PLC/stats tests, ring/converter tests, synthetic
end-to-end. Device behaviour [device] pending.

### Phase 4 — Protocol v2 + secure transport
[impl] Negotiated caps (codec, rate, channels, frame duration, FEC, DTX,
complexity, appID, formatGen, stream id, quality mode, source selection,
receiver state), HELLO/HELLO_OK, STREAM_START/ACK/STOP, FORMAT_UPDATE/ACK,
STATS, VOLUME, PING/PONG, RESUME/RESUME_OK, plus MSG_QUALITY_MODE,
MSG_SET_SOURCE(+ACK), MSG_RECEIVER_STATE. v1 kept as legacy.
[auto] protocol round-trip tests; QUIC host↔client; resume; volume/mute;
multi-receiver fanout; relay end-to-end.

### Phase 5 — Secure pairing + identity
[impl] ECDH P-256 + HKDF, mutual PIN verification, transcript-bound,
constant-time compares; DPAPI store (Windows, now with correct
`CryptUnprotectData` arity + self-heal on unreadable blobs); **Keychain**
trust store on iOS with migration from the old file store; host-certificate
pinning on the client after first pairing; discovery never conveys trust.
[auto] pairing exchange tests (success/wrong-PIN), identity round-trip,
DPAPI round-trip + self-heal, untrusted-host rejection. [device] Keychain
runtime behaviour pending.

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
states, dark/light, accessibility labels.
[device] visual + runtime validation pending.

### Phase 8 — Windows tray + settings GUI
[impl] Tray (start/stop, mute, receivers, source, quality, autostart, open
log folder, open settings, connect-to-last) **plus a native walk settings
window** (source picker, paired + live receiver lists with Forget/Mute/
volume, auto-connect, quality radios, diagnostics, error line).
[auto] build/vet/gofmt; smoke test (process runs; window opens, title
verified). [device] user validation pending.

### Phase 9 — Per-application capture
[impl] Pure-Go process loopback (`ActivateAudioInterfaceAsync` +
`AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`, agile completion handler,
multi-PID mixing, exclude mode), audio-session enumeration for the picker,
host integration (`SetCaptureApps`, SET_SOURCE kind 3 `p:`/`x:` names),
documented in `PROTOCOL_V2.md`.
[auto] name-parsing + validation tests only. **Live capture is [hw] blocked**
(no render endpoint) — the code is complete and compiles, but is explicitly
*not* claimed as validated.

### Phase 10 — Opus as a normal Windows release feature
[impl] `release-windows.yml` builds pinned libopus (SHA-verified) with
MinGW-w64 and produces release binaries with `-tags opus,nolibopusfile`; a
PCM-only build is a separate debug artifact; **missing libopus fails the
release loudly**. `scripts/build-windows.ps1` is the local equivalent.
[auto] CI-only (not run here).

### Phase 11 — Built-in synthetic test source
[impl] Engine-internal Test Tone (sine @ −12 dBFS with slow pan; click/
impulse mode) bypassing WASAPI; `serve --source testtone --tone-mode
sine|click`; documented as a diagnostic, never the default.
[auto] **`TestSyntheticSourceEndToEnd` proves real sine audio flows
source→codec→QUIC→client (peak amplitude asserted).**

### Phase 12 — Real iPhone validation
[impl] Runbook `docs/DEVICE_VALIDATION.md` with the full unchecked checklist
(basic / background / AirPods / network / sender / relay) and the exact
artifact + install steps. [device] **Not performed** — this environment
cannot drive the phone; the user must run it. Every box starts unchecked.

### Phase 13 — Optional jailbreak companion
[impl] Source + package layout (Theos tweak, plist, LaunchDaemon under
`layout/`, postinst/prerm, Makefile fixed for Theos), and a
`.github/workflows/build-jb.yml` that builds the `.deb` on Ubuntu and uploads
it. Core IPA never depends on it. [auto] CI-only. [device] optional.

### Phase 14 — Additional features
[impl] profiles (named, persisted, CLI `--profile`), recording (per-stream
WAV, pre-volume, `serve --record`), per-app capture (Phase 9), WAN relay
(`remote-au relay`, sealed envelopes). Reverse-microphone/full-duplex:
**explicit non-goal** (plan lists it as "later, if useful").
[auto] WAV/profile/relay tests.

### Phase 15 — Cleanup + truth pass
[impl] root `LICENSE` (AGPL-3.0), `THIRD_PARTY_NOTICES.md` (licenses verified
from disk: quic-go MIT, systray **Apache-2.0**, lxn MIT/BSD, hraban/opus
MIT text, go-qrcode MIT, opus BSD-3, x/* BSD-3, EchoWarp MIT — ideas only,
no code bundled), stale TODOs removed, release workflow NOTICE corrected.
This document.

---

## Automated test inventory (what [auto] actually covers)

Engine Go suite (all passing): v1 protocol/UDP/TCP; discovery (incl. the
fixed finder race); mixer/jitter/ring; **v2 protocol** (media + control
round-trips incl. new control messages); **pairing** (exchange, wrong PIN,
identity, **DPAPI round-trip + self-heal**); **engine** (publish/capture-free
QUIC host↔client; PCM media delivery; **synthetic test-tone end-to-end**;
resume-after-reconnect; untrusted-host rejection; per-receiver volume/mute;
multi-receiver fanout; **FEC/PLC**; **relay end-to-end**; **pairing URL
format**); WASAPI converters + per-app name parsing; mobile bridge (incl.
Keychain-store write-through/migration with a fake source); quality mappings.

Not covered automatically: Swift (uncompiled here), Opus runtime (no libopus
here), live WASAPI/process capture [hw], GUI visuals, CI pipelines.

## Hardware-blocked [hw] (this PC has no active render endpoint)

- Real WASAPI system-audio capture content/quality.
- Per-app (process-loopback) capture runtime.
- Capture-device hot-switch following a real endpoint change.

These are validated only when the PC has an active playback device; the
synthetic source covers the rest of the pipeline meanwhile.

## Explicit non-goals (not "incomplete")

Full-duplex / reverse microphone; non-iPhone receiver GUIs (Windows/macOS/
Linux clients); conference calling; cloud accounts; telemetry; virtual audio
drivers.

## Honest caveat

"Implemented" means the code exists and compiles (Go verified locally; Swift
verified by the CI iOS build). It does **not** mean hardware-validated: the
iPhone runbook is unrun and the WASAPI/per-app paths are untestable on this
machine. No item above is claimed [device]-validated.
