# RemoteAU jailbreak companion (OPTIONAL)

The core product is a normal, stock IPA — nothing here is required for
Windows → iPhone → AirPods playback while locked. This package exists only
because the target phone is jailbroken and can add quality-of-life
integration that stock iOS does not allow.

## What it would add

| Feature | Mechanism |
|---|---|
| Control Center toggle | LibActivator / CSJarkEvent tie-in to start/stop the receiver |
| Auto-launch receiver at boot | launchd `KeepAlive` on the app's receiver agent |
| Auto-reconnect persistence | Re-launch receiver when killed (KeepAlive) |
| Start on AirPods connect | `bluetoothd` event hook (BLTN route notification) |
| Pause on AirPods disconnect | Same hook, inverse action |
| SpringBoard status indicator | LSApplicationWorkspace badge / small banner tweak |

## Layout

Theos conventions: the tweak source and its filter plist live next to the
Makefile, and anything that must be copied verbatim onto the device lives
under `layout/`.

```
jb/
  Makefile
  RemoteAUCompanion.xm            tweak source (Logos)
  RemoteAUCompanion.plist         MobileSubstrate filter (com.apple.springboard)
  DEBIAN/
    control
    postinst
    prerm
  layout/
    Library/LaunchDaemons/
      dev.remoteau.receiver.plist KeepAlive receiver agent
  README.md
```

The built `RemoteAUCompanion.dylib` and `RemoteAUCompanion.plist` are installed
by Theos to `/Library/MobileSubstrate/DynamicLibraries/`. Anything under
`layout/` is staged at the package root (so the LaunchDaemon lands in
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
