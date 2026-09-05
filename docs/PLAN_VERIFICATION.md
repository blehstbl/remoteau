# Plan Verification — remote_au_ios_codex_plan.md

> **UPDATE 2026-09-05 (later)**: the gaps identified below were subsequently
> addressed — see the "Resolution pass" section at the bottom. The original
> audit text is preserved unchanged above that section for accuracy.

Verified item-by-item against the actual code on 2026-09-05. Every claim
below was checked by reading the implementation, not from memory. Status
legend:

- ✅ done (implemented and, where possible, verified by build/test)
- 🔶 partial (implemented but incomplete, or implemented with a deviation)
- ❌ not done
- ❓ untestable here (needs the real PC audio stack / a physical iPhone)

Verification environment facts: Go 1.27 on Windows; engine `go build ./...`
✅, `go vet` clean ✅, full test suite ✅ (discovery, transport, protocol v2,
pairing, engine incl. QUIC host↔client loopback, multi-receiver, mobile
bridge). Swift **cannot be compiled on this machine** — all iOS claims are
source-audited only. This PC currently reports **zero active audio
endpoints**, so no live WASAPI audio test was possible.

---

## 0. Goal checklist (top of the plan)

| Requirement | Status | Evidence / notes |
|---|---|---|
| Open source | 🔶 | `engine/LICENSE` = AGPL-3.0 (inherited from remote-au fork). No root-level LICENSE file; repo is local-only, never published. |
| Native Windows capture via WASAPI loopback | ✅ code / ❓ live | `engine/internal/audio/wasapi` (pure-Go COM: loopback + mic capture, render, enumeration, resampling). Live audio untestable here (no active endpoints). |
| Native iPhone receiver | ✅ code / ❓ device | `ios/RemoteAU` SwiftUI app (v1 receiver complete in source). Never compiled (no Mac locally) — CI builds IPA. |
| Works while iPhone locked/backgrounded | ✅ code / ❓ device | `UIBackgroundModes: audio` in `ios/project.yml`; background-audio session category. |
| Plays through AirPods / any iOS output | ✅ code / ❓ device | `.playback` category (A2DP default), route-change handling. |
| Very low latency on LAN | ✅ design | 10–15 ms auto jitter target, 5 ms PCM frames, drift correction. Not measured end-to-end (needs device). |
| Robust on imperfect Wi-Fi | 🔶 | v1 path: reorder+PLC+adaptive buffer ✅. v2 path: client has **no reorder/PLC** (`internal/engine/client.go` `decodeAndDeliver` — flagged TODO in code). |
| Secure | 🔶 | v2: TLS 1.3 + device certs + PIN pairing ✅. **Gap: client never pins the host certificate** (`FingerprintVerifier(nil, true)` = trust-any always, `client.go:122`) — reconnects authenticate the client to the host but not vice versa. v1 is plaintext by design. |
| Both lossless PCM and Opus | 🔶 | PCM ✅ default. Opus implemented behind `-tags opus` (cgo libopus); **not built/tested on this machine (no C toolchain)**; iOS-side Opus decode not wired yet. |
| Beautiful polished GUI (SwiftUI + Windows tray) | 🔶 | iOS UI implemented (hero, pills, presets, stats, dark/light). Tray app implemented but with dead menu wiring (see Phase 8). |
| CLI remains for debugging | ✅ | `devices/selftest/recv/send/serve/recv2`. |
| No Mac required | ✅ | CI workflow builds unsigned IPA + Go XCFramework on macOS runners. |
| Do not clone/base anything on SonoBus | ✅ | SonoBus never referenced. |
| remote-au + EchoWarp source "available" | 🔶 deviation | **Not present on this machine** — cloned from GitHub into `reference/` (this was the "genuinely blocked" case that justified the only web searches used). |

---

## Architecture direction

| Item | Status | Notes |
|---|---|---|
| Windows chain: WASAPI → engine → PCM/Opus → secure transport | ✅ | Implemented as specified. |
| iPhone chain: receive → reorder/jitter → loss recovery → drift → decode → AVAudioEngine → AirPods | 🔶 | v1 Swift path: all steps ✅ except decode (PCM only, as designed for v1). v2 Go path: reorder/PLC missing (above). |
| Encrypted connection | ✅ v2 / ❌ v1 | v1 plaintext (legacy by design). |
| Authenticated + persistently paired devices | 🔶 | Host pins client fingerprint ✅; **client does not pin host** ❌. |
| Reliable control channel + unreliable audio datagrams, no head-of-line blocking | ✅ | QUIC stream + RFC 9221 datagrams (`transport/v2`). |
| QUIC + QUIC DATAGRAM preferred if practical | ✅ | quic-go v0.62; `EnableDatagrams`, tested in loopback tests. |
| Transport abstract enough for another backend | ✅ | `transport/v2` Conn/Listener interface; only QUIC impl exists. |
| No WebRTC | ✅ | Not used. |

---

## Phase 1 — Analyze both codebases, define structure

| Item | Status | Notes |
|---|---|---|
| Understand remote-au: WASAPI capture, devices, PCM, packets, UDP/TCP, seq, capture ts, discovery, jitter, mixer, reconnect, CLI | ✅ | All read; documented in `docs/PHASE1_ANALYSIS.md` §1 with file references. |
| Understand EchoWarp: Opus/FEC/DTX/low-delay, adaptive bitrate, jitter/RTT/loss, crypto/auth, discovery, reconnect, device changes, session/control, profiles, multi-receiver, WAN | ✅ | §2 of the analysis; feature-by-feature verdict table. |
| Don't blindly copy EchoWarp; decide per feature | ✅ | Ideas adopted (adaptive logic, reconnect+terminal states); WebRTC/TUI/conference rejected. |
| Refactor into separated concerns: capture / codec / transport / discovery / session / buffering / playback / statistics / UI | ✅ | Exactly these packages exist (`engine/README.md` layout). |
| Keep existing remote-au behavior working | ✅ | v1 protocol/transport/mixer/tests untouched and passing; CLI `send/recv/devices/selftest` preserved (`--backend`/`-tags malgo` keep the original path). |

---

## Phase 2 — Native iOS receiver for stock remote-au

| Item | Status | Notes |
|---|---|---|
| Local-network permission | ✅ | `NSLocalNetworkUsageDescription` in `ios/project.yml`. |
| Discovery of the Windows sender | ❌ **buggy** | The iPhone runs the v1 **responder** (so the PC can find the *iPhone*), but the app has **no finder** that discovers PCs, and the v1 sender role doesn't announce anyway. Worse, two wire-format bugs break even responder interop (see Critical bugs B1/B2): stock `remote-au send` **cannot discover the iPhone today** despite `ios/README.md` claiming it. |
| Manual IP connection as a fallback | 🔶 | `ReceiverModel.manualIPFilter` only **filters** incoming senders (v1 receivers are passive). There is no outbound "connect to IP" — consistent with v1's data direction but weaker than the plan's intent. |
| v1 UDP packet parsing | ✅ | `V1Packet.swift` byte-for-byte matches `internal/protocol/udp.go` (RAUU envelope, HELLO fields incl. nameLen@9:11, AUDIO seq/captureFrame/len). |
| Sequence-number handling | ✅ | `ReorderBuffer` (expected seq, stale/duplicate drop, reorder window). |
| Capture-timestamp handling | 🔶 | Parsed (`captureFrame`) but **not used** — `writeConsecutive` ignores it; drift correction is ring-fill based instead. Plan Phase 10 "capture timestamp progression" not tracked anywhere. |
| PCM receive path | ✅ | S16LE → ring. |
| Audio buffering | ✅ | RT ring + adaptive policy. |
| AVAudioSession playback config | ✅ | `.playback`, preferred 48 kHz, 8 ms IO buffer. |
| AVAudioEngine | ✅ | `AVAudioSourceNode` render block. |
| AirPods / Bluetooth output | ✅ code / ❓ device | Default A2DP via playback category; no explicit option API (acceptable). |
| Output-route changes | ✅ | Route-change observer keeps streaming; route name surfaced. |
| Background playback | ✅ code / ❓ device | Background audio mode. |
| Locked-screen playback | ✅ code / ❓ device | Same mechanism; **never validated on hardware**. |
| Auto-reconnect after network loss | ✅ (v1) | 3 s watchdog drops binding; resumes on sender HELLO. |
| Recovery after AirPods disconnect/reconnect | ✅ code | Route-change handler; underrun bump to re-prime. |
| Recovery after audio-session interruption | ✅ | `.began` → stop, `.ended`+shouldResume → restart. |
| **Milestone**: unmodified remote-au → iPhone → AirPods, locked, keeps playing | ❌ not achieved yet | Blocked by B1/B2 (discovery), the manual-`--to` path works in principle, and nothing has been run on an iPhone. This plan explicitly says *do not move on until this works reliably* — that gate has not been passed. |

---

## Phase 3 — Better receiver engine

| Item | Status | Notes |
|---|---|---|
| Track packet arrival time | ✅ | `lastArrival`. |
| Track sequence gaps | ✅ | Conceal/loss counters. |
| Track reordering | ✅ | `reorderedPackets`. |
| Track late packets | ✅ | `latePackets`. |
| Track jitter | ✅ | `arrivalJitterMs` EWMA. |
| Track underruns | ✅ | `PCMRing.underruns`. |
| Track overruns | 🔶 | Only as generic `droppedFrames` (drop-oldest on overflow); no dedicated overrun counter/burst metric. |
| Track burst loss | ❌ | No burst detection — plain EWMA only. |
| Start very low on clean LAN | ✅ | Min 8 ms; auto target 15 ms; excellent level → 12 ms. |
| Increase quickly when unstable | ✅ | +60 ms/s toward required. |
| Decrease slowly when healthy | ✅ | −6 ms/s after 4 s stable window + 1 s cooldown. |
| Avoid oscillation | ✅ code / ❓ runtime | EWMA + hold windows; no long-run runtime validation. |
| Presets: Auto / Lowest Latency / Lossless / Robust / Manual-Advanced | ✅ | All five in `AdaptiveJitterPolicy` + UI picker. |
| 10–20 ms on good LAN | ✅ | 12–15 ms targets. |
| Clock-drift correction: continuous, tiny ratio, no periodic drop/insert | ✅ | `DriftController` (±0.1 % clamp) + fractional read in `PCMRing.tryDrainAdvanced`. No unit tests for drift itself. |
| PCM PLC: interpolation / smooth decay / crossfade back | 🔶 | Only the **decay-hold** variant exists (`ReorderBuffer.conceal`). No interpolation to the next good packet, no crossfade back in. |
| RT safety: no block / no network wait / no disk / no alloc / no UI in callback | ✅ | Try-lock only; drain is allocation-free; targets read via try-lock with cached fallback. |

---

## Phase 4 — Protocol v2 + secure transport

| Item | Status | Notes |
|---|---|---|
| New protocol, v1 kept as legacy | ✅ | `internal/protocol/v2`; v1 untouched. |
| Negotiate PCM/Opus | ✅ | `Caps.Codec`. |
| Negotiate sample rate / channels / frame duration | ✅ | 2/5/10/20 ms validated. |
| Negotiate codec parameters | 🔶 | Bitrate/FEC/DTX negotiated ✅; **Complexity and AppID are carried in Caps but `negotiateCaps` ignores the client's requested values** (host defaults used). |
| Negotiate stream IDs | ✅ field | `Media.StreamID` exists; only stream 0 ever used. |
| Negotiate format generation | ✅ | `formatGen` in media + ack. |
| Negotiate quality mode | ❌ | No quality-mode field. |
| Negotiate FEC / DTX | ✅ | Caps + applied on encoder. |
| Negotiate statistics reporting | ✅ | Stats message at 1 Hz (but see Phase 10 for content gaps). |
| Preserve seq + capture timestamps | ✅ | u32 seq, u40 captureTsUs. |
| Reliable control stream + unreliable datagrams + encrypted | ✅ | QUIC + TLS 1.3 device certs. |
| Control: hello/capability exchange | ✅ | HELLO/HELLO_OK. |
| Control: pairing/authentication | ✅ | PAIR_BEGIN/CHALLENGE/CONFIRM/RESULT. |
| Control: start/stop stream | 🔶 | START/ACK wired; **STOP never sent or handled**. |
| Control: selected source | ❌ | No source-selection message (host-side only). |
| Control: format negotiation | ✅ | STREAM_START/ACK. |
| Control: codec changes | 🔶 | FORMAT_UPDATE/ACK **defined but never sent/handled** — dynamic codec switching is not wired. |
| Control: volume/mute | 🔶 | `SendVolume` exists; **host never handles MsgVolume** — not wired end-to-end. |
| Control: stats | ✅ | MsgStats (+ACK type unused). |
| Control: receiver state | 🔶 | Implicit via HELLO/stats; no dedicated message. |
| Control: reconnect/resume | 🔶 | RESUME/RESUME_OK + resumeToken **defined but the flow is never exercised**. |
| Media datagram: streamID, seq, capture ts, formatGen, flags, payload | ✅ | `AppendMedia`/`DecodeMedia` (round-trip tested). |
| QUIC + QUIC DATAGRAM | ✅ | Tested in-process. |
| Legacy v1 marked insecure/legacy | ❌ | Docs imply it; no explicit "insecure/legacy" marking in CLI/UI. |

### Critical bugs found during verification (v1 interop)

- **B1 — iOS announce is malformed for stock remote-au.** `V1Packet.encodeAnnounce` (Swift) omits the 4-byte advertised-IPv4 field that remote-au's `decodeAnnounce` requires → stock `remote-au send` **cannot decode the iPhone's announce** (length mismatch). The Phase 2 README claim "remote-au send discovers the iPhone automatically" is currently **false**.
- **B2 — iOS cannot decode stock announces.** Swift `decodeDiscovery` validates announces with the *query* length rule (`count == 9 + nameLen`) instead of `9 + 20 + nameLen`.
- **B3 — v2 host announce breaks v1 parsers.** `runDiscoveryV2` appends a trailing version byte (`host.go:220`) and a `"\x02"` name suffix — every announce is 1 byte longer than the declared length → stock v1 finders reject it, and the name is corrupted. v2 hosts are invisible to v1 discovery.

---

## Phase 5 — Pairing + trusted identity

| Item | Status | Notes |
|---|---|---|
| No trust-the-LAN for v2 | ✅ | Unpaired clients must complete pairing before STREAM_START is honored. |
| First-time pairing flow (code shown/confirmed once) | ✅ engine / ❌ UI | Host generates 6-digit PIN, surfaces via callback (CLI prints, tray shows MessageBox). **No pairing UI exists in the iOS app** (`RemoteAUCore.swift` scaffold is never used by `RootView`). |
| QR confirmation | ❌ | Not implemented (code only). |
| Both sides persist identity | ✅ | Host: DPAPI store (`store_windows.go`); client (mobile): file store. |
| Future connections automatic + authenticated | 🔶 | Host side ✅ (fingerprint trusted after pairing). **Client side does not pin the host cert** (trust-any on every connect) — reconnects skip pairing but a forged host cert would be accepted. |
| iOS: Keychain storage | ❌ | File-based store with a documented hardening TODO. |
| Windows: secure storage | ✅ | DPAPI (CryptProtectData), blob copied before free. |
| Discovery never establishes trust | ✅ | Discovery only locates; pairing gates streaming. |

---

## Phase 6 — Opus + network resilience

| Item | Status | Notes |
|---|---|---|
| Opus as alternative; keep both modes | ✅ engine | PCM default; Opus behind `-tags opus`. |
| PCM: lossless, ~1.5 Mbps @48k/16/stereo, lowest latency | ✅ | 5 ms stereo frames = 1.536 Mbps. |
| Low-delay Opus mode | ✅ | `AppID=2` → `AppRestrictedLowdelay`. |
| Configurable bitrate | ✅ | Caps + `SetBitrate`. |
| In-band FEC | ✅ | `SetInBandFEC` + expected-loss %; negotiated via Caps. |
| Packet-loss concealment | 🔶 | Opus `DecodePLCFloat32` implemented, **but the v2 client never calls it with `lost=true`** — there is no reorder/loss detection in the v2 client media path. |
| Optional DTX | ✅ | Caps + `SetDTX`. |
| Complexity tuning | 🔶 | API exists; client-requested complexity not honored (see Phase 4). |
| 5/10/20 ms frames; choose best setting | ✅ | 2/5/10/20 supported; host picks per codec. |
| Adaptive quality monitors: loss/late/jitter/RTT/buffer/underruns | 🔶 | Host controller consumes all fields, **but the v2 client reports only RTT** (loss/jitter/late/buffer sent as zeros — `client.go utilityLoop`), so in practice the controller is fed placeholders. The iOS v1 engine measures these properly but doesn't speak v2. |
| Controls bitrate / FEC | ✅ | Applied per receiver. |
| Controls jitter-buffer target | 🔶 | Decision computed; **never sent to the receiver** (no control message for it). |
| PCM↔Opus switching in Auto | ❌ | Hints computed (`SwitchToPCM/Opus`) but no FORMAT_UPDATE wiring — never acts on them. |
| Smoothing/hysteresis | ✅ | EWMA + 2 s cooldown. |

---

## Phase 7 — Polished iOS GUI

| Item | Status | Notes |
|---|---|---|
| SwiftUI, clean hierarchy, minimal setup, clear state, spacing/typography, subtle animation, native conventions, dark/light, accessibility, responsive | ✅ code / ❓ visual | Implemented (hero + PulseDot, capsules, grouped backgrounds, `.secondary` styles, accessibility labels on pills, dynamic type via system fonts). No device screenshots to verify "polished". |
| Device cards: PC name / status / quality / output / connect | 🔶 | Cards exist (`DeviceCard`) but **the list is always empty** — no discovery client in the app (see Phase 2/5). Manual-IP filter section instead. |
| Connected view: PC, output, preset, codec, latency, loss, jitter | 🔶 | PC ✅, output ✅, preset ✅, latency ✅, loss+jitter ✅; **codec not displayed** (format string only). |
| Detailed diagnostics behind expandable view | ✅ | `DisclosureGroup` + `StatsView`. |
| Preset Auto / Lowest Latency / Lossless / Robust / Advanced | ✅ picker / 🔶 advanced | Advanced exposes **only a manual buffer-target slider**. Not exposed: min/max jitter bounds, frame duration, PCM/Opus choice, Opus bitrate, FEC, DTX, transport. |
| Continue playing while locked | ✅ code / ❓ device | |
| Auto-recover from BT route changes | ✅ code | |
| Reconnect after Wi-Fi interruptions | ✅ v1 code | v2 client has **no reconnect loop** (`Run` exits on error; nothing retries). |
| Remember trusted PCs | ❌ | No trusted-PC list in the iOS UI (engine store exists but unused by UI). |
| Remember last-used quality settings | ❌ | No `UserDefaults` persistence — preset resets on relaunch. |
| Avoid forcing reopen for reconnections | ✅ v1 | Watchdog + background audio. |

---

## Phase 8 — Windows tray app

| Item | Status | Notes |
|---|---|---|
| Lightweight tray app around the engine | ✅ | `cmd/remote-au-tray`, pure Go (systray + Win32 MessageBox), launches and idles (smoke-tested). |
| Start/stop streaming | ✅ | Host lifecycle in-process. |
| Show connection state | ✅ | Status item text. |
| Show connected receivers | ❌ | `Receivers` item exists but is permanently disabled — never populated (no bridge from `engine.Host` connection events to the tray). |
| Source selection (system audio / render devices / per-app later) | ❌ | Fixed loopback; no source menu. |
| Quality selection: Auto/Lowest/Lossless/Robust/Advanced | 🔶 | Menu exists; **handlers only toggle checkmarks — nothing is wired to the engine**. |
| Pairing / forgetting devices | 🔶 | Pairing-code popup ✅; **forget-device UI absent**. |
| Diagnostics | 🔶 | "Open log folder" opens the exe folder, but logs go to stderr of a windowless process — effectively no persistent logs to open. |
| Start with Windows | ✅ | HKCU Run key toggle. |
| Auto-stream to a selected trusted iPhone | 🔶 | Auto-starts the host at launch; there is no per-device target/auto-connect selection. |
| Main window (Source / Receivers / Quality) | ❌ | Tray menu only; no window. |
| Tray actions: Start/Stop ✅, Connect-to-last-iPhone ❌, Mute ❌, Current receiver ❌, Open settings ❌ | 🔶 | Only Start/Stop implemented of the five listed actions. |
| Tray must not interfere with RT threads | ✅ | Engine runs in goroutines; UI on its own thread. |
| CLI remains | ✅ | |

---

## Phase 9 — Device & session behavior

| Item | Status | Notes |
|---|---|---|
| Windows capture-device hot switching | ✅ | Default-endpoint monitor (2 s poll) closes capture; reopened on next read. Only for the *default* device (explicit-selector hot switch not exposed). |
| iPhone output-route switching | ✅ | Route-change handling. |
| Reconnect after temporary network loss | ✅ v1 / ❌ v2 | v2 client lacks a retry loop. |
| Reconnect after PC sleep/wake | 🔶 v1 | Same HELLO-return mechanism; not explicitly tested. v2: no. |
| Recover after iOS interruption | ✅ | |
| Recover after AirPods reconnect | ✅ | |
| Recover after media-services reset | ✅ | |
| Session resume without full renegotiation | ❌ | Resume token defined; flow never wired (v1 re-hello is cheap, v2 has no resume). |
| Clear error states in both GUIs | 🔶 | iOS `lastError` shown; tray shows MessageBox on failure only — no persistent status surface. |

---

## Phase 10 — Statistics + Auto tuning

| Item | Status | Notes |
|---|---|---|
| RTT | 🔶 | v2 Ping/Pong ✅ (engine). iOS v1 has no RTT (v1 protocol has no feedback channel — noted). |
| Jitter / loss / late / reordering | ✅ iOS engine / ❌ v2 Go client | Client v2 sends zeros (above). |
| Jitter-buffer depth | ✅ | Both. |
| Underruns/overruns | ✅/🔶 | Underruns ✅; overruns folded into dropped-frames. |
| Codec | ❌ UI | Not displayed anywhere (v1 is PCM-only so cosmetic). |
| Bitrate | ✅ iOS | Estimated from packet rate × frame bytes. |
| FEC state | ❌ UI | Not surfaced. |
| Drift-correction ratio | 🔶 | Measured (`PCMRing.lastDrainRatio`/`driftRatio`) but **not shown in UI**. |
| Capture timestamp progression | ❌ | Not tracked/used (see Phase 2). |
| Estimated software latency | ✅ | Buffer + 10 ms, explicitly labeled software-only. |
| Distinguish from AirPods/Bluetooth delay | ✅ | Explicit caption in `StatsView`. |
| No fake ping-based latency | ✅ | |
| Stats drive Auto | ✅ iOS v1 / 🔶 v2 host | v2 host gets placeholder inputs from the Go client. |

---

## Phase 11 — Multi-receiver

| Item | Status | Notes |
|---|---|---|
| One PC → multiple receivers | ✅ | Fanout verified by `TestMultiReceiverFanout` (2 clients). |
| Independent state/stats | ✅ | Per-connection `hostStream`. |
| Independent quality adaptation | ✅ | Per-connection encoder + `quality.Controller` (fed by placeholder client stats — see Phase 10). |
| One bad receiver doesn't degrade others | ✅ structurally | Independent encoders. |
| Later receivers: Windows/macOS/Linux | 🔶 | `recv2` v2 client works on Windows (Linux/macOS = malgo backend, untested); v1 `recv` unchanged cross-platform. |
| iPhone first | ✅ | |

---

## Phase 12 — WAN / relay

| Item | Status | Notes |
|---|---|---|
| LAN first | ✅ | |
| Direct secure QUIC when reachable | ✅ (works by design; untested off-LAN) | |
| Secure relay fallback | ❌ | Design only — `docs/WAN_RELAY.md` (relay server, envelope encryption, rendezvous checklist). No relay code. |
| Don't redesign app around WAN; keep transport abstract | ✅ | |
| NAT traversal as another transport, no ICE/WebRTC | ✅ design | |

---

## Phase 13 — Optional jailbreak companion

| Item | Status | Notes |
|---|---|---|
| Core IPA must not require jailbreak | ✅ | |
| Companion: Control Center toggle / auto-launch / persistence / AirPods-aware start-stop / SpringBoard indicator / faster controls | ❌ scaffold only | `jb/` contains README + `DEBIAN/control` only — no `postinst`, no LaunchDaemons plist, no tweak source, no dylib. |
| Keep jailbreak functionality separate | ✅ | Separate directory; core never references it. |

---

## Phase 14 — Extra features

| Item | Status | Notes |
|---|---|---|
| Per-app capture (all / only / except apps) | ❌ | Design doc only (`docs/PER_APP_CAPTURE.md` — process loopback via `ActivateAudioInterfaceAsync`). |
| Profiles (Home/Gaming/Weak Wi-Fi/Remote) | 🔶 | Quality presets exist; named profiles and preset *sync between GUIs* do not (and tray presets are unwired). |
| Recording | ❌ | Not implemented. |
| Reverse audio / mic / full duplex | ❌ | Plan says "later" — acceptable. |
| Don't let these delay the main product | ✅ | |

---

## Priorities & final-target check

| Plan statement | Status |
|---|---|
| Priority 1: reliable Windows → iPhone → AirPods | ❓ | Code complete for v1; **never demonstrated end-to-end** (no audio endpoints on this PC; no iPhone attached). |
| Priority 2: low latency | ✅ design / ❓ measured | |
| Priority 3: locked/background playback | ✅ code / ❓ device | |
| Priority 4: automatic recovery | ✅ v1 / 🔶 v2 | |
| Priority 5: audio quality | ✅ v1 / 🔶 Opus untested | |
| Priority 6: secure pairing/transport | 🔶 | Client-side cert pinning missing. |
| Priority 7: imperfect Wi-Fi behavior | ✅ v1 / 🔶 v2 | |
| Priority 8: beautiful/simple UX | 🔶 | iOS good; tray partial. |
| Priority 9: advanced features | 🔶 | See phases. |
| First setup: iPhone sees PC (step 4) | ❌ | No discovery client in app; responder interop broken (B1/B2). |
| First setup: tap PC → pairing code (steps 5–6) | ❌ UI / ✅ engine | |
| First setup: audio starts (steps 7–8) | ✅ code / ❓ device | |
| Daily use: auto-connect, lock, audio continues | 🔶 | v1 plausible once B1/B2 fixed; v2 lacks reconnect loop. |
| No Mac / BT adapter / virtual cable / OBS / browser / RDP / video | ✅ | None required or used. |

---

## Summary

- **Fully done:** Phase 1; the engine's v1 compatibility, v2 protocol/transport/pairing/codec/quality architecture; multi-receiver fanout; tray skeleton; docs/CI/build hygiene; no SonoBus, no WebRTC, no Mac requirement.
- **The single most important open item** is the plan's own Phase 2 gate: *unmodified remote-au → iPhone → AirPods while locked, working reliably*. It is blocked by three concrete discovery bugs (B1–B3), the absence of any on-device run, and this PC's disabled audio stack.
- **Notable unbuilt pieces** (promised by the plan, not present): iOS discovery/pairing UI, trusted-PC list + settings persistence, tray source selection / receiver list / mute / working quality wiring / diagnostics logs, v2 client reorder+PLC+stats measurement+reconnect loop, RESUME/STREAM_STOP/FORMAT_UPDATE/volume wiring, Keychain, QR, burst-loss & overrun metrics, Advanced-preset detailed controls, WAN relay, jailbreak companion substance, per-app capture, profiles, recording.

### Recommended fix order (cheapest first)
1. Fix B1/B2 (Swift announce encode/decode) and B3 (v2 announce marker) — small, restores the Phase 2 milestone path.
2. Validate on the real PC (enable an audio endpoint) with `remote-au send --source loopback --to <iphone>:47000`.
3. Client-side host-cert pinning (Phase 5 security gap).
4. v2 client media path: reorder + PLC + real stats + reconnect loop (closes most Phase 6/9/10 v2 gaps).
5. Wire tray menus and iOS v2 pairing UI.

---

## Resolution pass (2026-09-05, after the audit above)

Statuses after the fix pass. Items not listed here are unchanged from the
audit.

### Critical bugs � FIXED
- **B1** ? Swift `encodeAnnounce` now writes the 4-byte advertised IPv4
  (own primary address), byte-identical layout to remote-au.
- **B2** ? Swift `decodeDiscovery` parses announces correctly
  (instance+advertised+name, +1-byte v2 marker tolerated).
- **B3** ? The v2 host replies with a **clean v1 announce** (parsable by any
  stock remote-au finder) **plus** a type-3 v2 announce carrying the protocol
  version; v1 parsers reject the unknown type safely. Verified by
  `TestMultiReceiverFanout`/discovery tests.
- **Pairing concurrency** ? fixed as well: simultaneous pairings now
  serialize (mutex) instead of the second device being rejected
  (surfaced by `TestMultiReceiverFanout`).

### Phase 5 security � FIXED
- Client-side **host certificate pinning**: `ClientOptions.TrustedFingerprints`
  + mobile bridge loads stored peer fingerprints after the first pairing
  (`TestClientRejectsUntrustedHost` proves a non-matching host is rejected).
- Mobile trust store still file-based (Keychain = documented hardening).

### Phase 2/3 metrics � FIXED
- Burst-loss tracking (iOS `ReorderBuffer.maxBurstLoss` + Go clientMedia
  `maxBurstLoss`), overrun framing in the UI ("Dropped (overrun) frames"),
  drift-correction ratio, capture-clock Hz, and codec name surfaced in the
  statistics view. Capture timestamps: still parsed-not-driving (drift uses
  buffer level) � deviation documented, not a regression.

### Phase 4 control plane � WIRED
- STREAM_STOP ? (client sends on stop; host tears the stream down),
  RESUME/RESUME_OK ? (`TestClientResumeAfterReconnect`: reconnect without
  re-pairing or renegotiation), VOLUME ? (host applies per-receiver
  volume/mute � `TestHostVolumeAndMute`), FORMAT_UPDATE/ACK ? (quality
  controller switches PCM?Opus with a dwell timer; client rebuilds decoder),
  complexity/appID ? negotiated. Source selection as a control message
  remains host-local (documented).

### Phase 6 � WIRED
- v2 client media path: reorder window + PLC + real stats (loss/late/jitter/
  buffer) measured and **sent** to the host, feeding the adaptive controller
  with live data (`TestClientMediaReorderAndPLC`,
  `TestClientMediaStatsAccumulate`). Opus itself still behind `-tags opus`
  (needs a C toolchain; untested on this machine).

### Phase 9 � WIRED (v2)
- Reconnect loop with exponential backoff + resume (`ClientOptions.Reconnect`),
  covering network loss and PC sleep/wake for v2. Capture-device hot switch
  exposed to the tray (`SetSourceDevice`, `SetCaptureSource`).

### Phase 7/8 UX � WIRED
- iOS: discovery finder (v1+v2 announces), tappable PC cards, pairing sheet
  with PIN entry, paired-PC list with forget, persisted settings
  (preset/buffer/IP filter/v2 codec prefs), advanced controls, new stats rows.
- Tray: live receiver list (with per-receiver mute), wired quality presets,
  source selection submenu (default + render devices), mute, real log file
  (`%AppData%\RemoteAU\engine.log`) + open-log-folder.
- Codec display ? (hero line + stats).

### Phase 12 � IMPLEMENTED
- `remote-au relay` process (pure splice: control streams + media datagrams,
  TLS on both legs) + **end-to-end sealed envelopes**: control payloads and
  media payloads are AES-256-GCM sealed with a pairing-secret-derived key, so
  the relay only ever sees ciphertext. `TestRelayEndToEnd` runs the full
  flow through the relay. Host/client enable it via `--relay` /
  `RelayAddr`. NAT-traversal beyond the relay remains future work (design).

### Phase 13 � SUBSTANCE
- Complete companion package layout: `DEBIAN/control|postinst|prerm`,
  LaunchDaemons plist (KeepAlive receiver agent), MobileSubstrate filter +
  tweak source (AirPods-aware start/stop), Theos Makefile. Not compiled
  (no Theos here) � by design, optional.

### Phase 14 � SUBSTANCE
- Recording ?: per-stream WAV files (pre-volume) on the host
  (`serve --record <dir>`), header patched on close.
- Profiles ?: named profiles (Home/Gaming/Weak Wi-Fi/Remote) with JSON
  persistence (`serve --profile <name>`); GUI presets map onto the same
  presets.
- Per-app capture: still design-only (`docs/PER_APP_CAPTURE.md`) �
  deliberately deferred: it is untestable on this machine (no active audio
  endpoints) and the plan gates it behind a validated main path.

### Still open (unchanged)
- **On-device / live-audio validation** � requires the real PC (audio
  endpoints currently all disabled system-wide) and a physical iPhone.
- iOS-side Opus decoding arrives with the linked XCFramework build.
- Keychain-backed mobile trust store.
- QR pairing; Wi-Fi "burst" feedback loop in the adaptive jitter policy
  (burst metric now exists; the policy uses loss/jitter/underruns).
- Root repo is still local-only (not published).

### Test status after the pass
`go build ./...` ? � `go vet` clean ? � full suite ? � including new tests:
clientMedia reorder/PLC/stats, resume-after-reconnect, untrusted-host
rejection, mute, multi-receiver fanout (2 clients), full relay end-to-end.
