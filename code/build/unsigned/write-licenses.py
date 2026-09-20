#!/usr/bin/env python3
"""Assemble first-party license and exact third-party notices for a package."""

import argparse
import pathlib
import sys

sys.dont_write_bytecode = True

CODE = pathlib.Path(__file__).resolve().parents[2]
REPLICARO_LICENSE = CODE.parent / "LICENSE"
NOTICES = {
    "Restic": CODE / "backend/engines/assets/licenses/restic-LICENSE.txt",
    "Kopia": CODE / "backend/engines/assets/licenses/kopia-LICENSE.txt",
    "rclone": CODE / "backend/rclone/assets/licenses/rclone-LICENSE.txt",
    "godbus/dbus": CODE / "backend/desktop/assets/godbus-LICENSE.txt",
    "robfig/cron": CODE / "backend/licenses/robfig-cron-LICENSE.txt",
    "AppImage type 2 runtime": pathlib.Path(__file__).with_name("appimage-runtime-LICENSE.txt"),
}


def assemble(platform, openssh=None):
    if platform not in {"windows", "macos", "linux", "container"}:
        raise ValueError("unsupported notice platform")
    if (platform == "container") != (openssh is not None):
        raise ValueError("OpenSSH notice is required only for the container")
    names = ["Restic", "Kopia", "rclone", "robfig/cron"]
    if platform in {"linux", "container"}:
        names += ["godbus/dbus", "AppImage type 2 runtime"]
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
    return b"Replicaro licenses and third-party notices\n\n" + b"\n".join(sections)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--platform", choices=("windows", "macos", "linux", "container"), required=True)
    parser.add_argument("--openssh-license", type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    output = args.output
    if output.exists() or output.is_symlink() or not output.parent.is_dir():
        parser.error("notice output must be a new file in an existing directory")
    output.write_bytes(assemble(args.platform, args.openssh_license))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as error:
        raise SystemExit(str(error))
