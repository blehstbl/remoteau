# Third-Party Notices

RemoteAU as a whole is distributed under the **GNU Affero General Public
License, version 3 or later (AGPL-3.0-or-later)**. The full license text is in
[`LICENSE`](LICENSE) at the repository root and in
[`engine/LICENSE`](engine/LICENSE). RemoteAU is a fork of
[leaperone/remote-au](https://github.com/leaperone/remote-au), which is also
AGPL-3.0-or-later; the fork inherits and keeps that license.

This file lists the third-party components used by the project, their
licenses, and how they are used.

> License texts are **not duplicated here**. For components bundled into a
> release, the license text travels with the distribution (see
> [Where the license texts live](#where-the-license-texts-live)). Go module
> license texts were read from the local module cache for the versions pinned
> in [`engine/go.mod`](engine/go.mod) / `engine/go.sum`; they are not vendored
> into this repository.

## Licenses

### Project license / fork base

| Component | License | Source | How used |
|---|---|---|---|
| remote-au (leaperone/remote-au) | AGPL-3.0-or-later | https://github.com/leaperone/remote-au | Upstream project that `engine/` is forked and refactored from. Overall project license is AGPL-3.0-or-later; full text in `LICENSE` and `engine/LICENSE`. |

### Referenced designs (no code bundled)

| Component | License | Source | How used |
|---|---|---|---|
| EchoWarp (lHumaNl/EchoWarp) | MIT | https://github.com/lHumaNl/EchoWarp | **Design ideas only — no EchoWarp source code or binaries are bundled in RemoteAU.** Used as a reference for the adaptive-bitrate concept and the reconnect/terminal-state pattern (see `docs/PLAN_VERIFICATION.md`). The reference clone under `reference/EchoWarp/` is not shipped. |

### Native libraries

| Component | License | Source | How used |
|---|---|---|---|
| libopus (xiph/opus) | BSD-3-Clause (plus royalty-free patent grants) | https://github.com/xiph/opus · https://opus-codec.org/license/ | Static libopus built from the pinned `opus-1.4` tarball and linked into the Windows release binaries and the iOS XCFramework behind the `opus` build tag (see `docs/OPUS_PACKAGING.md`). The license/patent text ships as `COPYING` inside that tarball. |
| Opus (Go bindings: github.com/hraban/opus) | MIT | https://github.com/hraban/opus | cgo bindings that resolve libopus via pkg-config for tagged builds (`-tags "opus,nolibopusfile"`). |

### Go module dependencies

| Component | License | Source | How used |
|---|---|---|---|
| github.com/quic-go/quic-go | MIT | https://github.com/quic-go/quic-go | v2 transport: QUIC connection + QUIC DATAGRAM (RFC 9221) for control stream and media. |
| github.com/hraban/opus | MIT | https://github.com/hraban/opus | Opus codec bindings (see above). |
| github.com/skip2/go-qrcode | MIT | https://github.com/skip2/go-qrcode | **Not linked in the current tree** (no `go.mod`/`go.sum` entry; no source references). Listed because it is part of the local build inventory and is the intended library if pairing-QR generation is added. |
| fyne.io/systray | Apache-2.0 | https://github.com/fyne-io/systray | Windows/Linux system-tray menu for `remote-au-tray`. |
| github.com/lxn/walk | BSD-3-Clause | https://github.com/lxn/walk | Indirect dependency of systray; Windows GUI helpers. |
| github.com/lxn/win | BSD-3-Clause | https://github.com/lxn/win | Indirect dependency of systray; Windows API bindings. |
| gopkg.in/Knetic/govaluate.v3 | MIT | https://github.com/Knetic/govaluate | Indirect dependency of systray. |
| github.com/gen2brain/malgo | Unlicense (public domain) | https://github.com/gen2brain/malgo | Optional cross-platform audio backend (miniaudio bindings) selected with the `malgo` build tag; not used by the default Windows WASAPI build. |
| github.com/godbus/dbus/v5 | BSD-2-Clause | https://github.com/godbus/dbus | Indirect dependency of systray; D-Bus support on Linux. |
| golang.org/x/sys | BSD-3-Clause | https://cs.opensource.google/go/x/sys | Windows registry and low-level syscalls in the tray app. |
| golang.org/x/crypto | BSD-3-Clause | https://cs.opensource.google/go/x/crypto | Cryptographic primitives (key derivation and related helpers). |
| golang.org/x/net | BSD-3-Clause | https://cs.opensource.google/go/x/net | Indirect dependency of the networking stack. |

## Where the license texts live

- **AGPL-3.0-or-later** (project): `LICENSE` (root) and `engine/LICENSE`.
- **libopus**: `COPYING` inside the pinned `opus-<version>.tar.gz` used by both
  build workflows; the Windows artifact additionally ships a generated
  `NOTICE.txt` (see `.github/workflows/release-windows.yml`).
- **EchoWarp**: `reference/EchoWarp/LICENSE` in this repository (reference
  clone only; not shipped).
- **Go modules**: each module's own `LICENSE` file from the Go module cache
  (e.g. `…\go\pkg\mod\github.com\quic-go\quic-go@v0.62.0\LICENSE`). These files
  are not copied into this repository; the authoritative texts are available
  from the source URLs above.

## Notes and known discrepancies

- The Windows `NOTICE.txt` generated by `release-windows.yml` describes the
  hraban/opus bindings as "BSD 2-clause"; the actual bundled `LICENSE` text for
  `github.com/hraban/opus` is the MIT license text. Use the text in the
  component's `LICENSE` file as authoritative.
- `fyne.io/systray` is **Apache-2.0**, not BSD-3-Clause.
- `github.com/gen2brain/malgo` is **Unlicense** (public domain).
- `github.com/godbus/dbus/v5` is **BSD-2-Clause**.
