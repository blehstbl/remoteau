# Opus packaging (Windows + iOS)

## Summary

**Normal releases include Opus automatically.** The release pipelines build
libopus from a pinned source tarball, link the engine against it, and ship
the result — users never install a codec or see a build tag. Opus adds
in-band FEC (forward error correction), DTX and PLC on top of what the PCM
passthrough offers.

| Artifact | Where it's built | Contents |
| --- | --- | --- |
| `remote-au-windows.zip` | `.github/workflows/release-windows.yml` | `remote-au.exe`, `remote-au-tray.exe` linked with static libopus, `NOTICE.txt` |
| `remote-au-windows-pcm.zip` | same workflow | PCM-only pair, for debugging only |
| `RemoteAU.xcframework` | `.github/workflows/build-ipa.yml` (`framework` job) | Go engine with Opus for iOS device + simulator |

Local equivalents: `engine/scripts/build-windows.ps1` (Windows) and
`engine/scripts/build-ios-framework.sh` (macOS).

## Pinned source

Both platforms build **Opus 1.4** from
`https://github.com/xiph/opus/releases/download/v1.4/opus-1.4.tar.gz`,
verified against SHA-256
`c9b32b4253be5ae63d1ff16eea06b94b5f0f2951b7a02aceef58e3a3ce49c51f`.
Configure flags: `--disable-doc --disable-extra-programs --disable-shared
--enable-static`.

## Build tags (contributors only)

The Opus codec lives behind cgo (`github.com/hraban/opus`, which resolves
libopus via **pkg-config** — an `opus.pc` must be findable through
`PKG_CONFIG_PATH`; `build-windows.ps1` writes one if needed). Tagged builds
use two tags:

```
go build -tags "opus,nolibopusfile" ./cmd/remote-au
```

- `opus` — compiles `internal/codec/opus.go` (the cgo codec) and the
  `FECDecoder` implementation.
- `nolibopusfile` — drops the wrapper's `opusfile`-dependent files so only
  libopus itself (not libopusfile) needs to be provided.

The **default build has no tags**: `go build ./...` never touches libopus,
and Opus configs fail with `codec.ErrOpusUnavailable`, with callers falling
back to PCM.

## PCM-only dev builds remain available

- Local: `.\engine\scripts\build-windows.ps1 -SkipOpus` (Windows) or
  `OPUS=0 ./engine/scripts/build-ios-framework.sh` (macOS); or just
  `go build ./...` with no tags.
- CI: the Windows workflow additionally publishes the
  `remote-au-windows-pcm` artifact for debugging.

## FEC loss recovery

When the receiver's reorder window expires and a frame is missing, the
media pipeline (`internal/engine/media.go`) first tries **FEC**: if the
frame *after* the gap is already held, its wire payload is decoded through
`codec.FECDecoder` (`internal/codec`) to reconstruct the lost frame. If the
codec has no FEC support (PCM never does), the next frame isn't held, or
the decode fails, the pipeline falls back to plain PLC concealment as
before. Recovered frames are counted separately from PLC concealment
(`FECRecovered()`).
