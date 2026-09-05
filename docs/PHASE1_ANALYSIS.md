# Phase 1 — Codebase Analysis & New Internal Structure

Status: complete. This document records what was learned from reading both
codebases and defines the internal structure of the new product built on top of
`remote-au`.

Reference copies live in `reference/remote-au` and `reference/EchoWarp`.

---

## 1. remote-au — what it is and what it does well

Module: `remote-au` (Go, AGPL-3.0). A small N-senders → 1-receiver LAN audio
mixer CLI. Roles, not platforms: `send` and `recv` run on Windows/Linux/macOS.

### 1.1 Capture (Windows WASAPI loopback) — `internal/audio`

- Uses `gen2brain/malgo` (miniaudio cgo bindings).
- `OpenCapture` (capture.go): `malgo.Loopback` device type on Windows =
  native WASAPI loopback of a playback endpoint. `malgo.Capture` for mic.
- Format: S16LE, default 48 kHz / 2ch / 480-frame periods (10 ms),
  `PerformanceProfile = LowLatency`.
- The device callback copies straight into a bounded lock-guarded ring
  (`pcmRing`, ring.go) that drops oldest-on-overflow and counts drops; the
  network goroutine polls it every 2 ms. Callback never blocks.
- Devices are enumerated via `ctx.Devices()`; selection by index or
  case-insensitive name substring (`devices.go`), playback endpoints double as
  loopback sources on Windows.
- Weaknesses: single context per device open/close, no device-change
  notification, no per-application loopback (miniaudio does not expose it),
  cgo toolchain required on Windows (MinGW-w64).

### 1.2 Wire protocol v1 — `internal/protocol`

TCP (`protocol.go`, magic `RAU1`):
- Handshake: version(1) flags(1) sampleRate(u32 BE) channels(1) format(1)
  frameSamples(u16) nameLen(u16) name. Validated against strict ranges
  (8k–192k Hz, 1–8 ch, 1–4096 frame samples, S16LE only).
- Frames: type(1) seq(u64) captureFrame(u64) len(u32) payload. Payload length
  must equal `frameSamples*channels*2`.

UDP (`udp.go`, magic `RAUU`):
- `HELLO` datagram = handshake fields, re-sent every 1 s (sender.go).
- `AUDIO` datagram: seq(u64) captureFrame(u64) len(u16) payload, payload
  capped at 960 B (below Ethernet MTU); a packet is chunked as
  `960 / bytesPerFrame` frames, partial packets are accumulated until full.
- Strengths: tiny, versioned, seq + capture-frame timestamps preserved.
- Weaknesses: no encryption, no authentication, fixed S16LE, no negotiation,
  no stats channel, no reconnect token, no reordering (receiver drops
  out-of-order as "stale").

### 1.3 Sender — `internal/transport/sender.go`

- Resolve (discovery or fixed addr) → dial → stream loop.
- UDP: 1 s HELLO ticker; reads capture ring in 960 B chunks, sends only when a
  full packet is accumulated; tolerates transient UDP write errors
  (EAGAIN/timeout); seq/captureFrame advance per frame.
- Reconnect: exponential backoff 100 ms → 5 s, reset to minimum only if the
  attempt lasted ≥ 2 s with successful writes. Clean context cancellation.
- Weaknesses: no RTT measurement, no receiver feedback at all.

### 1.4 Receiver & buffering — `internal/transport/receiver.go`, `internal/mixer`

- TCP accept loop (bounded pending handshakes) + UDP loop keyed by
  `netip.AddrPort`; sources idle-timeout after 2 s.
- `frameTracker.Accept`: detects stale seq (drop), missing packets, capture
  frame gaps (capped at 3 s); receiver writes silence for gap frames. No
  reordering window: a late packet is always "stale".
- `JitterBuffer` (jitterbuffer.go): fixed **60 ms target / 80 ms high
  watermark**, drop-oldest-on-overflow, prime-then-play, re-prime on genuine
  underrun, `TryLock` in the pull path to avoid blocking the audio callback
  (on failure it outputs silence and counts underrun).
- `Mixer`: float32 accumulation of S16LE streams, per-stream + master gain,
  serialized to the playback callback; scratch buffers reused, no allocation
  in the callback path.
- Weaknesses: fixed conservative buffer (never adapts), no drift correction
  (a slow capture clock slowly drains the buffer into periodic underruns),
  silence-based concealment only, no RTT/loss stats feedback loop.

### 1.5 Discovery — `internal/discovery`

UDP broadcast (`RAUD`), query/announce with random 16-byte instance ID,
advertised IPv4 + TCP port + name; responder rate-limited (burst 16 / 20 ms);
finder listens on ephemeral port, queries ports 47001/48001/49001, re-queries
every 300 ms, dedupes by instance ID, prefers global > private > link-local >
loopback addresses. Plaintext and trust-the-LAN by design (README warns about
it). No mDNS.

### 1.6 Playback — `internal/audio/playback.go`

malgo playback device pulls `Pull(out, frameCount)` (frameCount is the audio
clock, period size is only a hint), zero-fills on silence. Same cgo caveats.

### 1.7 CLI — `cmd/remote-au/main.go`

Subcommands: `devices [--json]`, `selftest`, `recv`, `send`; global flags
`--rate --channels --frame-ms --verbose --log-level --log-format --version`.
Clean flag/flow separation, testable `run(...)` with injected io.Writer.
Good patterns to keep.

### 1.8 Stats — `internal/stats`

Queue size / max, discarded frames, underruns, playout latency per stream +
aggregate. No network stats (loss, jitter, RTT, reordering) — receiver-side
only, computed from the same process.

---

## 2. EchoWarp — what it does better and what we take from it

Go, WebRTC (pion) transport, libopus via `hraban/opus.v2`, TUI + API server.
Feature-by-feature verdict:

| Capability | EchoWarp implementation | Verdict for this project |
|---|---|---|
| Opus encode/decode | `audio/opus.go`: float32 in/out, 20 ms frames, pool-backed buffers; encoder bitrate/complexity/DTX/FEC/loss-% setters; decoder + `DecodePLC` | **Reuse the API shape**, reimplement cleanly behind our `Codec` interface (cgo libopus on desktop; same API on iOS via embedded framework) |
| FEC / DTX / PLC | `SetInBandFEC`, `SetPacketLossPerc`, `SetDTX`, `DecodePLCFloat32` | Adopt — exposed via v2 negotiation and quality presets |
| Low-delay config | `AppRestrictedLowdelay` application mode | Adopt as an option; default stays `AppAudio` for music |
| Adaptive bitrate | `adaptive.go`: `NetworkQuality{loss,jitter,rtt}` history window (10), 2 s cooldown, classify Excellent/Good/Fair/Poor by VoIP thresholds, ±10/−15/−30 % steps | **Keep the idea, improve it**: smoothed EWMA + hysteresis, also drives FEC %, jitter target, and PCM↔Opus switching in Auto |
| Jitter buffer | `jitter.go`: frame ring, `AdaptiveAdjust(loss bool)` ±1 frame | Idea only — ours must be time-based (ms targets), fast-up/slow-down, with proper target/watermarks (EchoWarp's is too crude) |
| Encryption/auth | `auth/`: salted-password challenge, ECDH, HWID binding, IP rate limit, secure file storage | Take the *concepts* (ECDH + PIN verification + persisted peer identity); implement pairing from scratch for v2 (PAKE-style PIN verification) |
| Discovery | mDNS via `grandcat/zeroconf` (`_echowarp._tcp`) | Keep remote-au's UDP broadcast for v1; v2 discovery adds service-type/version/feature TXT-style fields over the same broadcast mechanism (no new dependency, IPv4+IPv6 friendly) |
| Reconnect | `reconnect.go` exponential backoff (cap 60 s) + circuit breaker; treats kick/ban as terminal | Adopt pattern: backoff + terminal-state handling + "resume" instead of full re-handshake |
| Device changes | `device_monitor.go` polls device lists, diff → events | Adopt polling approach on Windows (no event port in pure Go); default-endpoint tracking for loopback |
| Session/control | signaling over TCP + data channels | v2 gets a real control stream (see protocol v2 below) |
| Profiles | server presets + runtime presets in TUI | Adopt as the Quality presets (Auto / Lowest Latency / Lossless / Robust / Advanced) |
| Multi-receiver | per-client encoders, shared capture fanout (`shared_capture.go`) | Adopt: one capture, N encode/transport pipelines with independent adaptation |
| Recording | WAV writer | Later phase (Phase 14) |
| WAN/ICE | pion ICE | Do **not** adopt WebRTC; v2 uses QUIC — direct when reachable, relay fallback later (Phase 12) |

Things deliberately **not** taken from EchoWarp: WebRTC stack (complexity,
SDP/ICE machinery, head-of-line risk inside SCTP), TUI as a primary interface,
conference/chat/ban subsystems, virtual-microphone machinery.

---

## 3. New product structure

Product name: **RemoteAU** (fork of remote-au; engine keeps the Go module
path `remote-au`). Repo layout:

```
engine/            Go module "remote-au" (forked from leaperone/remote-au)
  cmd/remote-au/         CLI (v1 send/recv/devices/selftest kept + v2 commands)
  cmd/remote-au-tray/    Windows tray/GUI app (pure Go, cgo-free)
  internal/
    audio/               format, ring, device models (pure Go)
      wasapi/            pure-Go WASAPI loopback/render (Windows, cgo-free)
      malgo/             miniaudio capture/playback (cgo; non-Windows default)
    codec/               Codec interface; pcm passthrough; opus (FEC/DTX/PLC)
    protocol/
      v1/                stock remote-au wire format (legacy mode)
      v2/                negotiated, versioned packet + control messages
    transport/
      v1/                stock UDP/TCP sender+receiver (legacy mode)
      v2/                QUIC + QUIC-DATAGRAM transport (control stream + media dgrams)
    discovery/           v1 broadcast + v2 discovery (pairing/version fields)
    pairing/             PIN pairing, identity keys, trust stores (DPAPI/Keychain)
    session/             control state machine, resume tokens, stats exchange
    engine/              orchestration: capture→codec→fanout→transport, adapters
    stats/               network + buffer + codec statistics, EWMA
    logging/             (unchanged from remote-au)
ios/               RemoteAU.app (SwiftUI)
  RemoteAU/             app sources (UI, engine, audio, net, pairing, stats)
  project.yml           XcodeGen project definition
  Makefile / CI         IPA build without a local Mac (GitHub Actions macOS runner)
jb/                optional jailbreak companion (separate from core IPA)
docs/              this analysis, architecture, protocol spec
reference/         cloned remote-au + EchoWarp sources (reference only)
```

### 3.1 Concern separation (per plan)

- **capture** — `audio` packages; engine depends on a `Capture` interface
  (already exists in v1 transport, kept).
- **codec** — `codec.Codec` interface: `EncodeFrame(pcm []byte) ([]byte, bool)`,
  `DecodeFrame(payload []byte, lost bool) ([]byte, error)`; PCM passthrough is
  the trivial implementation; Opus brings FEC/DTX/PLC.
- **transport** — `transport.Sender`/`transport.Listener` interfaces;
  `v1` and `v2` implementations; the engine selects at runtime.
- **discovery** — v1 responder/finder untouched for legacy mode; v2 announces
  protocol version + pairing availability so the iPhone can offer pairing.
- **session/control** — v2 control stream messages (hello/caps/pair/start/
  stop/format/codec/volume/stats/resume).
- **buffering** — iOS receiver owns the adaptive jitter buffer (Phase 3);
  desktop receiver keeps v1 jitter buffer for legacy mode.
- **playback** — iOS: AVAudioEngine source node; Windows desktop recv: WASAPI
  render (pure Go) or malgo.
- **statistics** — `stats` package: EWMA loss/jitter/RTT, buffer depth,
  drift ratio, codec bitrate; feeds Auto mode and both UIs.
- **UI** — iOS SwiftUI app; Windows tray app; both drive `engine` through a
  small control API; real-time paths never touch UI.

### 3.2 Transport decision (v2)

**QUIC + QUIC DATAGRAM** (quic-go on Windows/Go; the same Go stack compiled
into the iOS app via a c-archive/XCFramework so both ends share one protocol
implementation):

- one QUIC connection: bidirectional control stream (reliable, ordered) +
  unreliable datagrams for media (no head-of-line blocking);
- TLS 1.3 with client+server certs = device identity; pairing binds them;
- datagram payload: stream/session id, seq, capture ts, format generation,
  flags, payload (see `docs/PROTOCOL_V2.md`);
- iOS deployment: quic-go runs under iOS arm64 (gomobile/c-archive); Network
 .framework is used only for local UDP discovery in Swift.

Fallback if QUIC datagrams prove unusable on iOS at integration time: the
transport interface also has a plain UDP+DTLS-equivalent implementation path
(same packet format, AES-GCM per packet with keys from the pairing-derived
secret). The abstraction keeps this swappable (plan requirement).

### 3.3 Pairing (v2, Phase 5)

- First connect: sender shows a 6-digit code derived at pairing time;
  receiver (iPhone) user confirms it; both derive the transport keys via
  ECDH P-256 + HKDF-SHA256 with the PIN as a low-entropy verifier
  (commit-reveal check, constant-time compare) — reconnects authenticate with
  persisted identity keys, no PIN.
- iOS: identity private key + peer records in Keychain (kSecAttrAccessible
  AfterFirstUnlockThisDeviceOnly).
- Windows: peer records + private key DPAPI-encrypted under the user profile.
- Discovery only locates; it never conveys trust.

### 3.4 Real-time rules (both platforms)

Render callback must not allocate, lock contended, or touch the network:
iOS uses a lock-free SPSC ring between the network goroutine/thread and the
AVAudioEngine render callback; the same discipline remote-au uses (TryLock +
bounded ring + silence on miss) is preserved on desktop.

### 3.5 Licensing

Engine is a fork of AGPL-3.0 remote-au → stays AGPL-3.0; remote-au copyright
notice preserved. iOS app and tray app share the same license. EchoWarp code
is not copied; only ideas (documented above) are used.
