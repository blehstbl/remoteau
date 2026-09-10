# RemoteAU — Progress

Windows system audio → iPhone → AirPods over the local network. Fork of
[leaperone/remote-au](https://github.com/leaperone/remote-au) (AGPL-3.0) with
a native SwiftUI receiver, QUIC v2 stack, pairing, Opus, adaptive quality,
GUIs, relay, per-app capture and optional jailbreak extras.

**Authoritative status: `docs/PLAN_VERIFICATION.md`.** There, every item is
tagged implemented / auto-tested / live-validated (Windows) / device-validated
(nothing yet) / hardware- or CI-blocked.

## Status

| Phase | Status |
|---|---|
| 1 Analysis/architecture | done |
| 2 iOS receiver for stock remote-au | implemented; **device validation pending** |
| 3 Adaptive jitter/drift/PLC/RT | implemented; auto-tested |
| 4 Protocol v2 + QUIC datagrams | implemented; auto-tested; [live] v2 serve+recv2 playback |
| 5 Pairing + identity (PIN/QR, Keychain/DPAPI, pinning) | implemented; auto-tested; [live] no re-pairing across restarts |
| 6 Opus + FEC + adaptive quality | implemented; FEC/adaptive auto-tested; Opus runtime needs libopus (CI) |
| 7 iOS GUI | implemented (source/quality/profiles/jitter-bounds/recording); **device validation pending** |
| 8 Windows tray + settings window | implemented; smoke-tested (per-app picker, QR window, profiles, auto-connect) |
| 9 Per-app capture | implemented and reaching the API; **runtime unvalidated** (needs a WASAPI-session app + session-manager driver) |
| 10 Opus release packaging | **CI green** — Windows release built with static libopus (`remote-au-windows.zip`) |
| 11 Synthetic test source | implemented; auto-tested |
| 12 Real iPhone validation | runbook ready; **not run** (all boxes unchecked) |
| 13 Jailbreak companion | source + CC toggle; **CI green** — `.deb` built |
| 14 Profiles / recording / per-app / relay | implemented; auto-tested (per-app runtime unvalidated) |
| 15 Cleanup + truth pass | done (`PLAN_VERIFICATION.md`, LICENSE, notices, PROTOCOL_V2 fix) |

## CI status (all green on `main`, repo `blehstbl/remoteau`)

- **Build iOS IPA** ✓ — builds libopus for iOS, the `RemoteAU.xcframework`
  (device + simulator, Opus), then archives **`RemoteAU.ipa`** (unsigned).
  This is the first time the Swift app has ever been compiled.
- **Release Windows** ✓ — builds libopus with MinGW-w64 and produces
  **`remote-au-windows.zip`** (`remote-au.exe`, `remote-au-tray.exe`,
  `NOTICE.txt`; Opus is statically linked, no DLL).
- **Build Jailbreak Companion** ✓ — Theos builds both tweaks and packages
  **`dev.remoteau.companion_1.0_iphoneos-arm.deb`**.
- Artifacts are downloaded locally under `dist/` (git-ignored).

CI fixes made along the way are in the git history (gomobile tool dep, module
format/runner, app module-name collision, Swift API mismatches, static libopus
link).

## What runs today

- Engine: `go build ./...`, `go vet ./...`, `runtests.ps1` → **all tests pass**.
- CLI (`engine/bin`): `devices`, `selftest`, `recv`, `send`, `serve`, `recv2`,
  `relay`; `serve` flags `--source testtone|loopback`, `--record`, `--profile`,
  `--relay`, `--app-pid`/`--exclude-pid`, `--data-dir`.
- Tray + settings window: `remote-au-tray.exe` (smoke-tested; walk window).
- **Live on this Windows PC (headphones connected), validated:**
  - WASAPI loopback → v1 receiver → playback works.
  - v2 `serve` + `recv2` + PIN pairing + playback works (`stream active`).
  - Certificate pinning survives a restart — second run reconnects with no
    re-pairing.
- Test source (no audio endpoint needed):
  `remote-au serve --source testtone --tone-mode sine|click`.
  Automated proof: `TestSyntheticSourceEndToEnd`.

## Blocked / not validated

- **iPhone validation** (`docs/DEVICE_VALIDATION.md`): all items unchecked;
  install `dist/ios/RemoteAU-unsigned-ipa/RemoteAU.ipa` (TrollStore) and run
  the checklist.
- **Per-app capture runtime**: implemented and reaching the API; live activation
  reports the documented "target process has no capturable WASAPI session" for
  the test player, and `ListAudioProcesses` fails on this PC's HyperX driver
  (`E_NOINTERFACE`). Needs a WASAPI-session app + session-manager driver.
- **Hardware-blocked**: nothing pending beyond the above for the system-audio
  path (loopback works live here).

## Next actions

1. Install the unsigned IPA on the jailbroken iPhone (TrollStore) and run
   `docs/DEVICE_VALIDATION.md` using Test Tone (then real system audio).
2. Validate per-app capture with a WASAPI-session app (browser/music player)
   and a driver that exposes the audio session manager.
3. Optionally install `dist/jb/dev.remoteau.companion_1.0_iphoneos-arm.deb`.
4. Report any failure with `%AppData%\RemoteAU\engine.log` + the checklist
   item.
