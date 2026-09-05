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

```
jb/
  control                     Debian package metadata (for .deb build)
  DEBIAN/
    control
    postinst
  Library/
    MobileSubstrate/DynamicLibraries/
      RemoteAUCompanion.dylib     (placeholder; build with Theos)
      RemoteAUCompanion.plist     (filter: com.apple.springboard)
    LaunchDaemons/
      dev.remoteau.receiver.plist (KeepAlive receiver agent)
  README.md
```

## Build (on Linux/macOS with Theos, or WSL)

```
$THEOS/bin/nicify.pl RemoteAUCompanion   # tweak template
make package FINALPACKAGE=1
```

## Install

```
dpkg -i dev.remoteau.companion_1.0_iphoneos-arm.deb
```

## Guarantees

- The core IPA never imports or requires any of this.
- Removing this package leaves stock behavior fully functional.
