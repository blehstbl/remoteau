#!/usr/bin/env python3
"""Adds the RemoteAU.xcframework dependency to ios/project.yml (v2 mode).

Usage:  python3 scripts/add-v2-dep.py [ios-dir]
Idempotent: safe to run multiple times. Remove the dependency by deleting the
lines it added (marked with a comment) or re-generating from git.
"""
import sys
import re
from pathlib import Path

marker = "# v2 engine framework (added by scripts/add-v2-dep.py)"
dep = f"""    {marker}
    dependencies:
      - framework: Frameworks/RemoteAU.xcframework
        embed: true
"""

# The gomobile framework leaves libopus symbols undefined (dynamic_lookup), so
# the app must link libopus.a itself. CI downloads it to Frameworks/libopus.a.
settings_marker = "# v2 libopus link (added by scripts/add-v2-dep.py)"
settings = f"""        {settings_marker}
        LIBRARY_SEARCH_PATHS: "$(inherited) $(PROJECT_DIR)/Frameworks"
        OTHER_LDFLAGS: "$(inherited) -lopus"
"""


def main() -> int:
    ios_dir = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parent.parent / "ios"
    yml = ios_dir / "project.yml"
    text = yml.read_text(encoding="utf-8")

    if "RemoteAU.xcframework" in text:
        print("v2 dependency already present")
        return 0

    target = "    sources:\n      - path: RemoteAU\n"
    if target not in text:
        print("unexpected project.yml layout; aborting", file=sys.stderr)
        return 1
    text = text.replace(target, target + dep, 1)

    # Inject the libopus link settings into the target's settings.base block.
    settings_anchor = "      base:\n"
    if settings_anchor not in text:
        print("unexpected project.yml settings layout; aborting", file=sys.stderr)
        return 1
    text = text.replace(settings_anchor, settings_anchor + settings, 1)

    yml.write_text(text, encoding="utf-8")
    print(f"added v2 framework dependency + libopus link to {yml}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
