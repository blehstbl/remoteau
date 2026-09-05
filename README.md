# RemoteAU

**Windows system audio → iPhone → AirPods, over your local network.**
Native WASAPI loopback capture, a native SwiftUI receiver, encrypted QUIC
transport, and locked-screen playback — no Mac, no virtual audio cables, no
browser pages.

This is a fork/evolution of [leaperone/remote-au](https://github.com/leaperone/remote-au)
(AGPL-3.0) that adds a native iOS receiver, a modern secure transport, Opus,
pairing, adaptive quality and GUIs. See `docs/PHASE1_ANALYSIS.md` for the full
design rationale and `docs/PROGRESS.md` for status.

```
Windows                                  iPhone
┌────────────────────────┐               ┌──────────────────────────┐
│ WASAPI loopback        │               │ UDP/QUIC receive         │
│   → codec (PCM/Opus)   │   LAN         │   → reorder + PLC        │
│   → QUIC + DATAGRAM    │ ────────────► │   → adaptive jitter      │
│   (pairing, stats)     │               │   → drift correction     │
│ tray app + CLI         │               │   → AVAudioEngine        │
└────────────────────────┘               │   → AirPods (locked OK)  │
                                         └──────────────────────────┘
```

## Repo layout

| Path | What |
|---|---|
| `engine/` | Go module (forked from remote-au): capture backends, codecs, transports, session, pairing, engine, CLI, tray app, gomobile bridge |
| `ios/` | RemoteAU.app — SwiftUI receiver (XcodeGen project; builds unsigned IPA via CI) |
| `jb/` | Optional jailbreak companion (separate from the core IPA) |
| `docs/` | Phase 1 analysis, protocol v2 spec, progress tracker |
| `reference/` | Cloned remote-au + EchoWarp sources (reference only, not built) |

## Quick start (Phase 2 milestone — stock-compatible, works today)

1. **Windows**: build the CLI (`engine/runtests.ps1` passes; build with
   `go build -o bin/remote-au.exe ./cmd/remote-au` inside `engine/`), then:
   ```
   remote-au devices                                  # pick a playback device (optional)
   remote-au send --source loopback --to <iphone-ip>:47000
   ```
   or let it discover the iPhone automatically:
   ```
   remote-au send --source loopback
   ```
2. **iPhone**: install `RemoteAU.ipa` (CI builds an unsigned IPA on every
   push to `main`; install with TrollStore on jailbroken devices or sign with
   Sideloadly). Open the app — it answers discovery and starts playing.
3. Lock the phone. Audio keeps flowing to AirPods.

## v2 (encrypted, negotiated, QUIC)

- PC: `remote-au serve` (or the tray app) — prints a pairing code when a new
  device connects.
- iPhone: v2 is compiled into the IPA by CI via a Go XCFramework
  (`RemoteAU.xcframework`, see `engine/scripts/build-ios-framework.sh`).
  Tap the PC, enter the code once; future connections are authenticated
  automatically.

## Windows firewall

If prompted, allow `engine\bin\remote-au.exe` (and the tray app) on private
networks, or run `engine\scripts\add-firewall-rules.ps1` from an admin
terminal once.

## Development

- Engine tests: `powershell -File engine/runtests.ps1` (routes all test
  binaries through one path so the firewall only ever asks once).
- Opus builds: `go build -tags opus` (needs libopus + a C toolchain; the
  default build falls back to lossless PCM).
- iOS IPA: push to GitHub → Actions artifact (no Mac required), or
  `cd ios && make ipa` locally.
