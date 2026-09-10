# RemoteAU jailbreak companion (OPTIONAL)

The core product is a normal, stock IPA — nothing here is required for
Windows → iPhone → AirPods playback while locked. This package exists only
because the target phone is jailbroken and can add quality-of-life
integration that stock iOS does not allow.

## What it adds

| Feature | Mechanism | State |
|---|---|---|
| Control Center toggle | `RemoteAUControlCenter` tweak: a `CCUIControlCenterButton` that writes the receiver signal file | Implemented, not compiled here |
| Auto-launch receiver at boot | launchd `KeepAlive` on the app's receiver agent | Implemented |
| Auto-reconnect persistence | Re-launch receiver when killed (KeepAlive) | Implemented |
| Start on AirPods connect | `AVAudioSession` route-change observer | Implemented |
| Pause on AirPods disconnect | Same observer, inverse action | Implemented |

## Control Center toggle

`RemoteAUControlCenter.xm` is a separate tweak that adds one Control Center
button. Tapping it flips the same signal file the AirPods logic uses:

```
/var/mobile/Library/Preferences/dev.remoteau.companion   ("YES" / "NO")
```

- `YES` = the companion should keep the receiver active (same as "AirPods
  connected").
- `NO` = stop/pause the receiver (same as "AirPods gone").
- On creation and whenever Control Center appears, the button reads the file
  and reflects it in its selected state.

**It does not start a second receiver.** The toggle is only a signal writer;
the single receiver process stays owned by the `dev.remoteau.receiver`
LaunchDaemon (`layout/Library/LaunchDaemons/dev.remoteau.receiver.plist`) and
the agent that reads this file. There is deliberately no path from a button
tap to spawning a process, so repeated taps cannot create duplicate receivers.

### Honest caveats (please validate on-device)

- The code is hand-audited: Swift/ObjC/Theos are not available in the
  authoring environment. CI (`.github/workflows/build-jb.yml`) is the first
  place either tweak is actually compiled; validate on-device before relying
  on it.
- The button class `CCUIControlCenterButton` is private
  (`ControlCenterUIKit`). The public iPhoneOS SDK has no header for it, so
  `headers/ControlCenterUIKit/CCUIControlCenterButton.h` is a minimal local
  declaration and the real class resolves at load time with
  `-Wl,-undefined,dynamic_lookup`.
- Modern Control Center is built from **modules** loaded from
  `/System/Library/ControlCenter/Bundles` and gated by the private
  `ControlCenterServices` module whitelist/provider system. The exact schema
  for a third-party module plist/bundle could **not** be verified, so this
  package does **not** ship a fabricated module plist (there is intentionally
  no `layout/Library/ControlCenter/Modules/...`). Instead the tweak injects the
  button into whichever Control Center view controller appears. That injection
  is best-effort and should be validated on a test device; the module-provider
  registration is the part most likely to need changing.

## Layout

Theos conventions: the tweak sources and their filter plists live next to the
Makefile, and anything that must be copied verbatim onto the device lives
under `layout/`.

```
jb/
  Makefile
  RemoteAUCompanion.xm            AirPods route-aware tweak source (Logos)
  RemoteAUCompanion.plist         filter (com.apple.springboard)
  RemoteAUControlCenter.xm        Control Center toggle tweak source (Logos)
  RemoteAUControlCenter.plist     filter (com.apple.springboard)
  headers/
    ControlCenterUIKit/
      CCUIControlCenterButton.h   minimal private-class declaration
  DEBIAN/
    control
    postinst
    prerm
  layout/
    Library/LaunchDaemons/
      dev.remoteau.receiver.plist KeepAlive receiver agent
  README.md
```

The built `RemoteAUCompanion.dylib` / `RemoteAUControlCenter.dylib` and their
`.plist` filters are installed by Theos to
`/Library/MobileSubstrate/DynamicLibraries/`. Anything under `layout/` is
staged at the package root (so the LaunchDaemon lands in
`/Library/LaunchDaemons/`).

## Build (on Linux/macOS with Theos, or WSL)

```
make package FINALPACKAGE=1
```

The `.deb` is written to `packages/`. CI builds this automatically in
`.github/workflows/build-jb.yml` and uploads the `remoteau-companion-deb`
artifact.

## Install

```
dpkg -i dev.remoteau.companion_1.0_iphoneos-arm.deb
```

## Guarantees

- The core IPA never imports or requires any of this.
- Removing this package leaves stock behavior fully functional.
