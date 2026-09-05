# Phase Progress Tracker

| Phase | Status | Notes |
|---|---|---|
| 1 — Analyze + architecture | ✅ done | `docs/PHASE1_ANALYSIS.md`; engine forked & refactored (audio/codec/transport/session/pairing/engine separated) |
| 2 — Native iOS receiver for stock remote-au | ✅ implemented | v1 UDP parsing, discovery responder, AVAudioEngine, background/locked playback, watchdog reconnect, interruption/route recovery. **Needs on-device validation** (no iPhone attached to this machine) |
| 3 — Better receiver engine | ✅ implemented | Adaptive jitter policy (Auto/Lowest/Lossless/Robust/Advanced), drift correction (continuous micro-resampling), PLC (decay-hold; Opus PLC comes with v2), RT-safe SPSC-style ring with try-lock discipline |
| 4 — Protocol v2 + secure transport | ✅ implemented (engine) | QUIC + QUIC DATAGRAM (quic-go), control stream + media datagrams, capability negotiation, formatGen, resume token framing. Legacy v1 kept end-to-end |
| 5 — Secure pairing + identity | ✅ implemented (engine) | ECDH P-256 + HKDF + mutual PIN verification, transcript-bound, constant-time compares; DPAPI trust store on Windows; file store for mobile (Keychain = hardening TODO); iOS Keychain TODO |
| 6 — Opus + resilience | ✅ implemented (engine) | Codec abstraction + Opus behind `-tags opus` (FEC/DTX/PLC APIs), adaptive quality controller (EWMA + hysteresis + cooldown), STATS control message wired host-side. iOS-side Opus decode arrives with the xcframework integration |
| 7 — Polished iOS GUI | ✅ core implemented | Hero card, latency/network pills, preset picker, stats disclosure, device section, dark/light, accessibility labels. Fine-polish + v2 pairing UI pending on-device testing |
| 8 — Windows tray app | ✅ implemented | Pure-Go tray: start/stop, pairing code popups (MessageBox), autostart (Run key), quality menu, open log folder, runtime-generated icon |
| 9 — Device/session behavior | 🔶 partial | iOS: interruption/route/watchdog recovery done. Windows: default-endpoint monitoring + hot recapture TODO (poll-based) |
| 10 — Rich statistics + Auto tuning | 🔶 partial | Buffer depth, jitter, loss, underruns, PLC counts, packet rate/bitrate, software-latency estimate (with Bluetooth caveat) in iOS UI; RTT via v2 Ping on engine. Drift-ratio + late/reorder on iOS engine not yet surfaced in UI |
| 11 — Multi-receiver | 🔶 partial | Host fans out per-connection codecs/quality (architecture ready + used in host loop); multi-client test TODO |
| 12 — WAN/relay | ⬜ planned | Transport is abstracted; direct QUIC works anywhere reachable; relay design documented in PROTOCOL_V2 + this tracker; relay server not implemented |
| 13 — Jailbreak companion | ⬜ scaffold | `jb/` layout planned; core IPA stays jailbreak-free |
| 14 — Per-app capture, profiles, recording | ⬜ planned | WASAPI process loopback (IAudioClient per-app) designed; not implemented |

## Validation status on this machine

- Engine: `go build ./...` ✅, full `go test` suite ✅ (incl. QUIC host↔client
  loopback with pairing + PCM media).
- Tray app: launches, idles, exits cleanly ✅.
- WASAPI capture/playback: compiled ✅; **live audio untestable here** — this
  machine currently reports zero active audio endpoints (all render endpoints
  disabled system-wide; verified with `waveOutGetNumDevs` = 0). The code
  follows the documented COM ABI and returns clear errors instead of
  guessing. Validate on the target PC.
- iOS app: source-complete for the v1 milestone; cannot compile Swift on
  Windows — the CI workflow builds the IPA on macOS runners. On-device
  validation (locked playback, AirPods recovery) pending.

## Next steps (priority order)

1. Validate Phase 2 milestone on the real PC + iPhone (needs audio endpoints
   enabled on the PC).
2. Wire the iOS v2 UI (pairing sheet using `V2Controller`) — code scaffolded
   in `RemoteAUCore.swift`.
3. Windows device monitor + hot recapture (Phase 9).
4. Multi-receiver + stats hardening (Phases 10/11).
