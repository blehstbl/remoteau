# RemoteAU per-application capture (Phase 14)

Status: **implemented** (pure-Go process loopback). Runtime validation is still
pending on a target application that owns a WASAPI audio session (see below).

## Mechanism: WASAPI process loopback (Windows 10 2004+)

`ActivateAudioInterfaceAsync` with `AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK`
returns an `IAudioClient` bound to a target process tree:

- `PROCESS_LOOPBACK_MODE_PROCESS_TARGET` — capture one PID (the game,
  Chrome, Spotify…). Multiple PIDs = one activation each, mixed.
- `PROCESS_LOOPBACK_MODE_EXCLUDE_PROCESS_TARGET` — system mix minus the
  excluded PID (single PID supported).

Implementation notes (all verified against Microsoft's ApplicationLoopback
sample during live debugging):

- `ActivateAudioInterfaceAsync` is resolved from `mmdevapi.dll` (with a
  `combase.dll` fallback) and requires COM initialized on the calling thread.
- The activation params are passed as a `VT_BLOB` PROPVARIANT whose blob is a
  12-byte `AUDIOCLIENT_ACTIVATION_PARAMS` (`{ActivationType, TargetProcessId,
  ProcessLoopbackMode}`).
- The completion object is `IActivateAudioInterfaceAsyncOperation`; its
  method is `GetActivateResult(HRESULT*, IUnknown**)` (not
  `IAsyncOperation::GetResult`), followed by a QueryInterface for
  `IAudioClient`.
- The stream then uses the same mix-format polling loop as normal loopback.

## Process selection UX

- Windows tray **Source → Applications**: enumerated audio processes with
  "Only" / "Exclude" actions.
- CLI: `remote-au serve --app-pid 123,456` / `--exclude-pid 789`.
- v2 control: `SET_SOURCE` kind 3 with a `p:<pids>` or `x:<pid>` name; the
  host validates and acknowledges (`SET_SOURCE_ACK`), and remains authoritative.

## Why not virtual devices

Process loopback captures without touching the render path, so normal
listening is unaffected — matching the "no virtual audio cable" requirement.

## Runtime validation status

- The code path activates and returns the documented HRESULT when the target
  has no capturable WASAPI session
  (`HRESULT_FROM_WIN32(ERROR_FILE_NOT_FOUND)`, e.g. a legacy winmm
  `SoundPlayer`), which is expected behaviour.
- `ListAudioProcesses` requires a driver that exposes
  `IAudioSessionManager2`; on a HyperX virtual-surround endpoint it returns
  `E_NOINTERFACE`, so the tray picker shows a graceful "could not list" note.
- **Still to validate**: capture *content* from a real WASAPI-session
  application (browser/music player) and exclude mode.

## Checklist

- [x] `internal/audio/wasapi`: `OpenProcessLoopback(pids, exclude, format, logger)`
      with the agile completion-handler COM object.
- [x] Session enumerator for the app picker (`ListAudioProcesses`).
- [x] Host options + `SetCaptureApps` and `SET_SOURCE` kind 3.
- [x] Tray source menu (Only/Exclude) and CLI flags.
- [ ] Live content validation with a WASAPI-session app + a driver exposing
      `IAudioSessionManager2`.
