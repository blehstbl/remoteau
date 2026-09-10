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
| 10 Opus release packaging | workflow exists; **pending CI** |
| 11 Synthetic test source | implemented; auto-tested |
| 12 Real iPhone validation | runbook ready; **not run** (all boxes unchecked) |
| 13 Jailbreak companion | source + CC toggle + CI package build; uncompiled; **pending CI** |
| 14 Profiles / recording / per-app / relay | implemented; auto-tested (per-app runtime unvalidated) |
| 15 Cleanup + truth pass | done (`PLAN_VERIFICATION.md`, LICENSE, notices, PROTOCOL_V2 fix) |

## What runs today

- Engine: `go build ./...`, `go vet ./...`, `runtests.ps1` → **all tests pass**.
- CLI (`engine/bin`): `devices`, `selftest`, `recv`, `send`, `serve`, `recv2`,
  `relay`; `serve` flags `--source testtone|loopback`, `--record`, `--profile`,
  `--relay`, `--app-pid`/`--exclude-pid`, `--data-dir`.
- Tray + settings window: `remote-au-tray.exe` (smoke-tested; walk window).
- **Live on this Windows PC (headphones connected), validated this session:**
  - WASAPI loopback → v1 receiver → playback works.
  - v2 `serve` + `recv2` + PIN pairing + playback works (`stream active`).
  - Certificate pinning survives a restart — second run reconnects with no
    re-pairing.
- Test source (no audio endpoint needed):
  `remote-au serve --source testtone --tone-mode sine|click`.
  Automated proof: `TestSyntheticSourceEndToEnd`.

## Blocked / not validated

- **iPhone validation** (`docs/DEVICE_VALIDATION.md`): all 28 items unchecked;
  requires installing the CI IPA and running the checklist. Swift has never
  been compiled locally.
- **Per-app capture runtime**: implemented, but live activation reports the
  documented "target process has no capturable WASAPI session" for the test
  player, and `ListAudioProcesses` fails on this PC's HyperX driver
  (`E_NOINTERFACE`). Needs a WASAPI-session app + session-manager driver.
- **CI artifacts**: IPA + Opus XCFramework, Windows release (libopus), and
  jailbreak `.deb` workflows exist but have **not run** yet.
- **Opus runtime locally**: needs libopus; release CI builds it.

## Next actions

1. **Push to GitHub and run CI** (immediate next step): create the remote, push
   `master`, then run `build-ipa`, `release-windows`, and `build-jb` and verify
   the artifacts (IPA + XCFramework, Windows libopus binaries, `.deb`).
2. Install the unsigned IPA (TrollStore) and run `docs/DEVICE_VALIDATION.md`
   using Test Tone.
3. Validate per-app capture on a PC with a WASAPI-session app and a driver that
   exposes the audio session manager.
4. Report any failure with `%AppData%\RemoteAU\engine.log` + the checklist
   item.
