# RemoteAU iOS App

Native SwiftUI receiver for Windows system audio streamed by the RemoteAU
engine (a fork of remote-au). Plays through AirPods or any iOS output, keeps
playing while the phone is locked, and recovers automatically from ordinary
network/Bluetooth disruptions.

## Phase 2 milestone

```
Windows: remote-au send --source loopback --to <iphone-ip>:47000
iPhone:  RemoteAU app (this folder) → AirPods
```

The iPhone runs a remote-au v1 discovery responder on UDP 47001/48001/49001,
so `remote-au send` can also find it automatically:

```
remote-au send --source loopback          # discovers the iPhone on the LAN
```

## Features implemented

- remote-au v1 UDP protocol: HELLO handshake + AUDIO datagrams (big-endian,
  seq + capture-frame preserved), exact wire compatibility (`V1Packet.swift`).
- Discovery responder compatible with remote-au's `RAUD` query/announce,
  with the same reply rate limiter.
- AVAudioEngine + AVAudioSourceNode render path fed by an RT-safe ring
  (`PCMRing.swift`): render callback never blocks (try-lock; silence on miss).
- Packet reordering window + PCM loss concealment (decay-hold) on the network
  thread (`ReorderBuffer.swift`).
- Adaptive jitter policy with presets: Auto / Lowest Latency / Lossless /
  Robust / Advanced (`AdaptiveJitterPolicy.swift`).
- Clock-drift correction: continuous micro-resampling ratio (±0.1 % max)
  driven by ring-fill EWMA — no periodic sample drops (`DriftController.swift`).
- Background + locked playback (`UIBackgroundModes: audio`), audio session
  interruption handling, route-change recovery (AirPods unplug/replug),
  media-services reset recovery.
- Sender watchdog: auto-resumes when the PC returns after Wi-Fi blips.

## Building the IPA without a Mac

1. Push this repo to GitHub — the `Build iOS IPA` workflow builds an
   **unsigned IPA** on a macOS runner (Actions → upload artifact).
2. Install on the device:
   - Jailbroken: TrollStore (accepts unsigned IPAs), or
   - Stock: sign the IPA with a free/paid Apple ID using Sideloadly /
     Xcode, then install.

Local builds need a Mac with Xcode + XcodeGen: `make ipa`.

## Notes

- iOS shows a one-time **Local Network** permission prompt when the app
  answers discovery queries; allow it.
- The latency figure shown is the software portion only. AirPods add
  Bluetooth latency that iOS does not expose to apps (also stated in the
  statistics view).
