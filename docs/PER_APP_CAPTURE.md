# RemoteAU per-application capture (Phase 14 — design)

Goal: stream **all Windows audio**, a **single application**, or everything
**except** chosen applications, without virtual audio devices (per plan).

## Mechanism: WASAPI process loopback (Windows 10 2004+)

`ActivateAudioInterfaceAsync` with `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`
returns an `IAudioClient` bound to a target process tree:

- `PROCESS_LOOPBACK_MODE_PROCESS_TARGET` — capture one PID (the game,
  Chrome, Spotify…).
- `PROCESS_LOOPBACK_MODE_EXCLUDE_PROCESS_TARGET` — capture the system mix
  minus the excluded PIDs (e.g. exclude Discord).

Notes from the Windows ABI:

- Requires the `AUDIOCLIENT_ACTIVATION_PARAMS` struct passed through
  `ActivateAudioInterfaceAsync`'s `PROPVARIANT` (VT_UI8 pointer to params).
- The completion arrives via an `IActivateAudioInterfaceCompletionHandler`
  callback — implement the COM callback object in pure Go (vtable-based,
  same approach as the rest of `internal/audio/wasapi`).
- The resulting stream is used exactly like normal loopback: same mix-format
  polling loop, same float→S16 converter, same ring → the codec/transport
  stack is untouched.

## Process selection UX (tray)

- Source menu: `System Audio` (default), `App…` (picker of audio-active
  processes — poll `IAudioSessionManager2` session list for names/Icons),
  `Exclude…`.
- PID changes on app restart: the monitor watches the session list and
  re-targets by name.

## Why not virtual devices

Virtual audio cables route audio AWAY from the speakers and break normal
listening; process loopback captures without touching the render path. That
matches the plan's "no virtual audio cable" requirement.

## Checklist

- [ ] `internal/audio/wasapi`: `OpenProcessLoopback(pid uint32, exclude bool)`
      with the completion-handler COM object.
- [ ] Session enumerator for the app picker.
- [ ] Host options: `ProcessTargets []uint32` / `ExcludedPIDs []uint32`.
- [ ] Tray source menu + CLI flags (`--app <name>`, `--exclude <name>`).
