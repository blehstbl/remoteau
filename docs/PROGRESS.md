# RemoteAU — Progress (final)

Windows system audio → iPhone → AirPods over the local network. Fork of
[leaperone/remote-au](https://github.com/leaperone/remote-au) (AGPL-3.0) with
a native SwiftUI receiver, QUIC v2 stack, pairing, Opus, adaptive quality,
GUIs, relay, per-app capture and optional jailbreak extras.

**Authoritative status: `docs/PLAN_VERIFICATION.md` (final truth pass).**
There, every item is tagged implemented / auto-tested / device-validated /
hardware-blocked.

## What runs today

- Engine: `go build ./...`, `go vet ./...`, `runtests.ps1` → **all tests pass**.
- CLI (`engine/bin`): `devices`, `selftest`, `recv`, `send`, `serve`, `recv2`,
  `relay`.
- Tray + settings window: `remote-au-tray.exe` (smoke-tested; walk window).
- Test source (no audio endpoint needed):
  `remote-au serve --source testtone --tone-mode sine|click`.
  Automated proof: `TestSyntheticSourceEndToEnd`.

## Milestones

| Phase | Status |
|---|---|
| 1 Analysis/architecture | done |
| 2 iOS receiver for stock remote-au | implemented; **device validation pending** |
| 3 Adaptive jitter/drift/PLC/RT | implemented; auto-tested |
| 4 Protocol v2 + QUIC datagrams | implemented; auto-tested |
| 5 Pairing + identity (PIN/QR, Keychain/DPAPI, pinning) | implemented; auto-tested (Keychain runtime pending) |
| 6 Opus + FEC + adaptive quality | implemented; FEC/adaptive auto-tested; Opus runtime needs libopus (CI) |
| 7 iOS GUI | implemented; **visual/device validation pending** |
| 8 Windows tray + settings window | implemented; smoke-tested |
| 9 Per-app capture | implemented; **[hw] blocked** (no render endpoint) |
| 10 Opus release packaging | CI implemented |
| 11 Synthetic test source | implemented; auto-tested |
| 12 Real iPhone validation | runbook ready; **not run** |
| 13 Jailbreak companion | source + CI package build |
| 14 Profiles / recording / per-app / relay | implemented; auto-tested (per-app runtime [hw]) |
| 15 Cleanup + truth pass | done (`PLAN_VERIFICATION.md`, LICENSE, notices) |

## Blocked / not validated

- **iPhone validation** (`docs/DEVICE_VALIDATION.md`): all items unchecked;
  requires the user to install the CI IPA and run the checklist.
- **Live WASAPI + per-app capture** [hw]: this PC has no active playback
  endpoint. The synthetic source validates the rest of the path.
- **Opus runtime locally**: needs libopus; release CI builds it.

## Next actions for the user

1. Run the iOS build workflow (Actions) → install the unsigned IPA
   (TrollStore) → follow `docs/DEVICE_VALIDATION.md` using Test Tone.
2. When the PC has an audio device, repeat the WASAPI/per-app items.
3. Report any failure with `%AppData%\RemoteAU\engine.log` + the checklist
   item; fixes land in the engine/tray or iOS UI as needed.
