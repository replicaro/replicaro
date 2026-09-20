#!/usr/bin/env python3
"""Map verified target outputs to the public release asset inventory."""

import argparse
import datetime
import hashlib
import json
import pathlib
import re
import shutil


TARGETS = ("windows_amd64", "linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64")
VERSION = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+")
CHECKSUM = "sha256sums"
LINUX_KEY = "replicaro-linux-release-key.asc"
INPUTS = pathlib.Path(__file__).resolve().parent / "inputs.json"


def version(value):
    if not VERSION.fullmatch(value):
        raise ValueError("release version is invalid")
    return value


def pairs(target, release_version):
    version(release_version)
    if target not in TARGETS:
        raise ValueError("release target is invalid")
    old = f"Replicaro-{release_version}-{target}"
    lower = f"replicaro-{release_version}-{target}"
    public_platform, public_arch = {
        "windows_amd64": ("windows", "x86_64"),
        "linux_amd64": ("linux", "x86_64"),
        "linux_arm64": ("linux", "arm"),
        "darwin_amd64": ("mac", "x86_64"),
        "darwin_arm64": ("mac", "arm"),
    }[target]
    public = f"replicaro-{release_version}-{public_platform}-{public_arch}"
    if target == "windows_amd64":
        result = ((old + "-portable.zip", public + "-portable.zip"),
                  (old + "-setup.exe", public + "-setup.exe"))
    elif target.startswith("linux_"):
        result = ((old + ".AppImage", public + "-appimage.appimage"),
                  (lower + ".deb", public + "-deb.deb"),
                  (lower + ".rpm", public + "-rpm.rpm"),
                  (lower + ".tar.gz", public + "-tar.tar.gz"))
    else:
        result = ((old + "-portable.zip", public + "-app.zip"),
                  (old + ".dmg", public + "-dmg.dmg"))
    configured = json.loads(INPUTS.read_text(encoding="utf-8"))["targets"][target]["artifacts"]
    expected = [name.replace("{version}", release_version) for name in configured]
    if expected != [old_name for old_name, _ in result]:
        raise ValueError("public asset mapping differs from the unsigned build contract")
    return result


def payload_names(release_version):
    names = [new for target in TARGETS for _, new in pairs(target, release_version)]
    if len(names) != len(set(names)):
        raise ValueError("public asset name collision")
    return sorted(names)


def asset_names(release_version, kind):
    names = payload_names(release_version) + [CHECKSUM]
    if kind == "signed":
        names.append(LINUX_KEY)
        names += [new + ".sig" for target in TARGETS if target.startswith("linux_")
                  for _, new in pairs(target, release_version)]
    elif kind != "unsigned":
        raise ValueError("release kind is invalid")
    if len(names) != len(set(names)):
        raise ValueError("public release asset name collision")
    return sorted(names)


def regular(path):
    if path.is_symlink() or not path.is_file():
        raise ValueError("release asset is missing or redirected: " + path.name)
    return path


def digest(path):
    value = hashlib.sha256()
    with regular(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def checksums(root, release_version, signed=False):
    names = [name for name in asset_names(release_version, "signed" if signed else "unsigned")
             if name != CHECKSUM]
    return "".join(f"{digest(root / name)}  {name}\n" for name in names)


def stage_unsigned(source_root, output, release_version):
    if not output.is_dir() or output.is_symlink() or any(output.iterdir()):
        raise ValueError("public release staging directory must be empty")
    for target in TARGETS:
        release = source_root / target
        if not release.is_dir() or release.is_symlink():
            raise ValueError("verified target release is missing")
        for old, new in pairs(target, release_version):
            shutil.copyfile(regular(release / old), output / new)
    (output / CHECKSUM).write_text(checksums(output, release_version), encoding="ascii")


def restore_target(download, output, release_version, target):
    mapping = pairs(target, release_version)
    if not download.is_dir() or download.is_symlink() or not output.is_dir() or output.is_symlink() or any(output.iterdir()):
        raise ValueError("release download or target output is unavailable")
    expected_local = {CHECKSUM, *(new for _, new in mapping)}
    if {entry.name for entry in download.iterdir()} != expected_local:
        raise ValueError("downloaded target inventory differs from the public release")
    actual = regular(download / CHECKSUM).read_text(encoding="ascii")
    lines = actual.splitlines(keepends=True)
    names = payload_names(release_version)
    if len(lines) != len(names):
        raise ValueError("public checksum inventory has the wrong size")
    values = {}
    for line, name in zip(lines, names):
        match = re.fullmatch(r"([0-9a-f]{64})  " + re.escape(name) + "\n", line)
        if not match:
            raise ValueError("public checksum inventory is malformed or unordered")
        values[name] = match.group(1)
    for old, new in mapping:
        if digest(download / new) != values[new]:
            raise ValueError("public release asset differs from its checksum: " + new)
        restored = output / old
        shutil.copyfile(download / new, restored)
        if target.startswith("linux_") and old.endswith(".AppImage"):
            restored.chmod(0o755)


def check_release_assets(release_json, release_version, kind):
    value = json.loads(regular(release_json).read_text(encoding="utf-8"))
    assets = value.get("assets")
    if (not isinstance(assets, list) or
            any(not isinstance(item, dict) or not isinstance(item.get("name"), str) for item in assets) or
            sorted(item["name"] for item in assets) != asset_names(release_version, kind)):
        raise ValueError("public release asset inventory differs from the exact expected set")


def unsigned_notes(release_version, revision, identity, timestamp):
    if not re.fullmatch(r"[0-9a-f]{40}", revision) or not re.fullmatch(r"sha256:[0-9a-f]{64}", identity):
        raise ValueError("unsigned release identity is invalid")
    datetime.datetime.strptime(timestamp, "%Y-%m-%dT%H:%M:%SZ")
    return (f"Unsigned Replicaro {version(release_version)} builds from verified public source. "
            "macOS DMG app contents match their app ZIPs; DMG container bytes may differ between builds. "
            "These artifacts are not code-signed.\n"
            f"Public revision: {revision}\nSource identity: {identity}\nBuild timestamp: {timestamp}\n")


def notes_timestamp(release_json, release_version, revision, identity):
    value = json.loads(regular(release_json).read_text(encoding="utf-8"))
    body = value.get("body")
    if not isinstance(body, str):
        raise ValueError("unsigned release description is missing")
    body = body.replace("\r\n", "\n").rstrip("\n") + "\n"
    match = re.fullmatch(
        re.escape(f"Unsigned Replicaro {version(release_version)} builds from verified public source. ")
        + r"macOS DMG app contents match their app ZIPs; DMG container bytes may differ between builds\. "
        + r"These artifacts are not code-signed\.\n"
        + re.escape(f"Public revision: {revision}\nSource identity: {identity}\n")
        + r"Build timestamp: ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)\n", body)
    if not match:
        raise ValueError("unsigned release description differs from the selected source")
    parsed = datetime.datetime.strptime(match.group(1), "%Y-%m-%dT%H:%M:%SZ")
    if parsed.strftime("%Y-%m-%dT%H:%M:%SZ") != match.group(1):
        raise ValueError("unsigned release timestamp is invalid")
    return match.group(1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("pairs", "names", "stage-unsigned", "restore-target",
                                            "check-release", "unsigned-notes", "notes-timestamp", "checksums"))
    parser.add_argument("--version", required=True)
    parser.add_argument("--target")
    parser.add_argument("--kind", choices=("unsigned", "signed"))
    parser.add_argument("--source-root", type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path)
    parser.add_argument("--download", type=pathlib.Path)
    parser.add_argument("--release-json", type=pathlib.Path)
    parser.add_argument("--revision")
    parser.add_argument("--identity")
    parser.add_argument("--timestamp")
    args = parser.parse_args()
    release_version = version(args.version)
    if args.command == "pairs":
        for old, new in pairs(args.target, release_version):
            print(old + "\t" + new)
    elif args.command == "names":
        print("\n".join(asset_names(release_version, args.kind)))
    elif args.command == "stage-unsigned":
        stage_unsigned(args.source_root, args.output, release_version)
    elif args.command == "restore-target":
        restore_target(args.download, args.output, release_version, args.target)
    elif args.command == "check-release":
        check_release_assets(args.release_json, release_version, args.kind)
    elif args.command == "unsigned-notes":
        print(unsigned_notes(release_version, args.revision, args.identity, args.timestamp), end="")
    elif args.command == "notes-timestamp":
        print(notes_timestamp(args.release_json, release_version, args.revision, args.identity))
    elif args.command == "checksums":
        print(checksums(args.output, release_version, args.kind == "signed"), end="")


if __name__ == "__main__":
    try:
        main()
    except (OSError, UnicodeError, ValueError, TypeError, KeyError) as error:
        raise SystemExit(str(error)) from None
