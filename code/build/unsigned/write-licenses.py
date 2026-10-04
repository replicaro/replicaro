#!/usr/bin/env python3
"""Assemble first-party license and exact third-party notices for a package."""

import argparse
import pathlib
import sys

sys.dont_write_bytecode = True

CODE = pathlib.Path(__file__).resolve().parents[2]
REPLICARO_LICENSE = CODE.parent / "LICENSE"
LICENSES = CODE / "backend/licenses"
NOTICES = {
    "Restic": CODE / "backend/engines/assets/licenses/restic-LICENSE.txt",
    "Kopia": CODE / "backend/engines/assets/licenses/kopia-LICENSE.txt",
    "rclone": CODE / "backend/rclone/assets/licenses/rclone-LICENSE.txt",
    "godbus/dbus": CODE / "backend/desktop/assets/godbus-LICENSE.txt",
    "robfig/cron": LICENSES / "robfig-cron-LICENSE.txt",
    "AppImage type 2 runtime": pathlib.Path(__file__).with_name("appimage-runtime-LICENSE.txt"),
    # Everything below is compiled or embedded into the Replicaro executable
    # itself: the Go runtime and standard library, the Go modules linked into
    # it, the production JavaScript of the embedded web UI, and the web fonts
    # the UI serves. Their BSD/MIT/OFL terms require the notice to travel with
    # binary copies, so each one gets its own full section, even when the text
    # is byte-identical to another section (the three Go BSD files; react,
    # react-dom and scheduler; react-router and react-router-dom). Keeping
    # them separate is deliberate: the file stays small, and a reader can find
    # every component by name without having to know which texts happen to
    # match.
    #
    # Every file in backend/licenses/ is an unmodified copy of the upstream
    # license text. The labels below name the exact version it was taken from
    # (the older robfig/cron label above has none; its version is the one in
    # go.mod). When go.mod, the pinned Go toolchain
    # (build/unsigned/inputs.json), frontend/package-lock.json or the bundled
    # fonts change, refresh the copied text and the label together.
    "Go standard library (go1.26.7)": LICENSES / "go-LICENSE.txt",
    "github.com/google/uuid v1.6.0": LICENSES / "google-uuid-LICENSE.txt",
    "golang.org/x/sys v0.44.0": LICENSES / "golang-x-sys-LICENSE.txt",
    "golang.org/x/text v0.39.0": LICENSES / "golang-x-text-LICENSE.txt",
    # The modernc modules ship more than one license file because parts of
    # them are translated or derived from other projects (SQLite itself, musl
    # and Go for libc, Go and mmap-go for memory). Each upstream file is its
    # own section, labelled with its upstream file name.
    "modernc.org/sqlite v1.53.0": LICENSES / "modernc-sqlite-LICENSE.txt",
    "modernc.org/sqlite v1.53.0 (SQLITE-LICENSE)": LICENSES / "modernc-sqlite-SQLITE-LICENSE.txt",
    "modernc.org/libc v1.73.4": LICENSES / "modernc-libc-LICENSE.txt",
    "modernc.org/libc v1.73.4 (LICENSE-3RD-PARTY.md)": LICENSES / "modernc-libc-LICENSE-3RD-PARTY.txt",
    "modernc.org/mathutil v1.7.1": LICENSES / "modernc-mathutil-LICENSE.txt",
    "modernc.org/memory v1.11.0": LICENSES / "modernc-memory-LICENSE.txt",
    "modernc.org/memory v1.11.0 (LICENSE-GO)": LICENSES / "modernc-memory-LICENSE-GO.txt",
    "modernc.org/memory v1.11.0 (LICENSE-MMAP-GO)": LICENSES / "modernc-memory-LICENSE-MMAP-GO.txt",
    "github.com/dustin/go-humanize v1.0.1": LICENSES / "dustin-go-humanize-LICENSE.txt",
    "github.com/mattn/go-isatty v0.0.20": LICENSES / "mattn-go-isatty-LICENSE.txt",
    "github.com/ncruces/go-strftime v1.0.0": LICENSES / "ncruces-go-strftime-LICENSE.txt",
    "github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec": LICENSES / "remyoudompheng-bigfft-LICENSE.txt",
    # Only these npm packages end up in the production bundle.
    # react-router-dom only re-exports react-router; it is listed because the
    # app imports it by that name. react-router's cookie and set-cookie-parser
    # dependencies are server-side helpers that tree-shaking drops from the
    # browser build, so they are not listed.
    "react 19.2.7": LICENSES / "react-LICENSE.txt",
    "react-dom 19.2.7": LICENSES / "react-dom-LICENSE.txt",
    "react-router 7.18.2": LICENSES / "react-router-LICENSE.txt",
    "react-router-dom 7.18.2": LICENSES / "react-router-dom-LICENSE.txt",
    "scheduler 0.27.0": LICENSES / "scheduler-LICENSE.txt",
    # The fonts are the Google Fonts builds (unmodified woff2 subsets), so the
    # OFL texts are the copies google/fonts ships beside those builds.
    "JetBrains Mono font (Google Fonts, API revision v24)": LICENSES / "jetbrains-mono-OFL.txt",
    "Schibsted Grotesk font (Google Fonts, API revision v7)": LICENSES / "schibsted-grotesk-OFL.txt",
}

# Notices for code that is part of every product target.
COMMON = [
    "Restic",
    "Kopia",
    "rclone",
    "robfig/cron",
    "Go standard library (go1.26.7)",
    "github.com/google/uuid v1.6.0",
    "golang.org/x/sys v0.44.0",
    "golang.org/x/text v0.39.0",
    "modernc.org/sqlite v1.53.0",
    "modernc.org/sqlite v1.53.0 (SQLITE-LICENSE)",
    "modernc.org/libc v1.73.4",
    "modernc.org/libc v1.73.4 (LICENSE-3RD-PARTY.md)",
    "modernc.org/mathutil v1.7.1",
    "modernc.org/memory v1.11.0",
    "modernc.org/memory v1.11.0 (LICENSE-GO)",
    "modernc.org/memory v1.11.0 (LICENSE-MMAP-GO)",
    "github.com/dustin/go-humanize v1.0.1",
    "github.com/mattn/go-isatty v0.0.20",
    "github.com/ncruces/go-strftime v1.0.0",
    "github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec",
    "react 19.2.7",
    "react-dom 19.2.7",
    "react-router 7.18.2",
    "react-router-dom 7.18.2",
    "scheduler 0.27.0",
    "JetBrains Mono font (Google Fonts, API revision v24)",
    "Schibsted Grotesk font (Google Fonts, API revision v7)",
]
LINUX = ["godbus/dbus", "AppImage type 2 runtime"]


def assemble(platform, openssh=None, tini=None):
    if platform not in {"windows", "macos", "linux", "container"}:
        raise ValueError("unsupported notice platform")
    if (platform == "container") != (openssh is not None) or (platform == "container") != (tini is not None):
        raise ValueError("OpenSSH and tini notices are required only for the container")
    names = list(COMMON)
    if platform in {"linux", "container"}:
        names += LINUX
    license_content = REPLICARO_LICENSE.read_bytes()
    if not license_content or b"\x00" in license_content:
        raise ValueError("invalid Replicaro license source")
    sections = [b"===== Replicaro (Apache-2.0) =====\n" + license_content.rstrip(b"\n") + b"\n"]
    for name in names:
        content = NOTICES[name].read_bytes()
        if not content or b"\x00" in content:
            raise ValueError(f"invalid notice source: {name}")
        sections.append(f"===== {name} =====\n".encode() + content.rstrip(b"\n") + b"\n")
    if openssh is not None:
        content = pathlib.Path(openssh).read_bytes()
        if not content or b"\x00" in content:
            raise ValueError("invalid OpenSSH notice source")
        sections.append(b"===== OpenSSH client =====\n" + content.rstrip(b"\n") + b"\n")
    if tini is not None:
        content = pathlib.Path(tini).read_bytes()
        if not content or b"\x00" in content:
            raise ValueError("invalid tini notice source")
        sections.append(b"===== tini =====\n" + content.rstrip(b"\n") + b"\n")
    return b"Replicaro licenses and third-party notices\n\n" + b"\n".join(sections)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", choices=("windows", "macos", "linux", "container"), required=True)
    parser.add_argument("--openssh-license", type=pathlib.Path)
    parser.add_argument("--tini-license", type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    output = args.output
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        parser.error("notice output must be a new file in an existing directory")
    output.write_bytes(assemble(args.platform, args.openssh_license, args.tini_license))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        raise SystemExit(str(error))
