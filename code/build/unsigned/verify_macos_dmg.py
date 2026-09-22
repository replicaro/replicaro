#!/usr/bin/env python3
"""Verify that a macOS DMG contains exactly its companion portable app."""

import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import zipfile


PACKAGE = re.compile(r"Replicaro-([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)-(darwin_amd64|darwin_arm64)\.dmg")
LAYOUT = pathlib.Path(__file__).resolve().parent / "macos-dmg-layout"
BACKGROUND = "installer-background.png"
FINDER_TEMPLATE = "FinderLayout.dsstore"
BACKGROUND_DIMENSIONS = (820, 560)
PROVENANCE_LIMIT = 64 * 1024


def fail(message):
    raise ValueError(message)


def regular_input(path, label):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or path.is_symlink():
        fail(label + " must be an ordinary file")


def digest(stream):
    value = hashlib.sha256()
    while chunk := stream.read(1024 * 1024):
        value.update(chunk)
    return value.hexdigest()


def png_dimensions(path):
    with path.open("rb") as stream:
        header = stream.read(24)
    if header[:16] != b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR":
        fail("DMG background is not a canonical PNG")
    return tuple(int.from_bytes(header[offset:offset + 4], "big") for offset in (16, 20))


def record(kind, mode, content=None):
    return kind, stat.S_IMODE(mode) if kind != "link" else None, content


def has_resource_fork(path):
    if sys.platform != "darwin":
        return False
    listed = subprocess.run(["xattr", "-s", str(path)], check=True, stdout=subprocess.PIPE).stdout
    if b"com.apple.ResourceFork" not in listed.splitlines():
        return False
    value = subprocess.run(["xattr", "-p", "-x", "-s", "com.apple.ResourceFork", str(path)],
                           check=True, stdout=subprocess.PIPE).stdout
    return bool(value.strip())


def archive_inventory(portable, release_version, target):
    entries = {}
    with zipfile.ZipFile(portable) as archive:
        for item in archive.infolist():
            name = item.filename
            components = name.rstrip("/").split("/")
            if (not name.startswith("Replicaro.app/") and name != "Replicaro.app/") or any(
                part in ("", ".", "..") for part in components
            ) or "\\" in name or name.endswith("//"):
                fail("portable ZIP has an unsafe or unexpected path: " + name)
            path = name.rstrip("/")
            if path in entries or item.create_system != 3:
                fail("portable ZIP has a duplicate or non-Unix entry: " + name)
            mode = item.external_attr >> 16
            file_type = stat.S_IFMT(mode)
            if file_type == stat.S_IFDIR and item.is_dir():
                entries[path] = record("directory", mode)
            elif file_type == stat.S_IFREG and not item.is_dir():
                with archive.open(item) as source:
                    entries[path] = record("file", mode, (item.file_size, digest(source)))
            elif file_type == stat.S_IFLNK and not item.is_dir():
                with archive.open(item) as source:
                    entries[path] = record("link", mode, source.read())
            else:
                fail("portable ZIP contains an unsupported entry: " + name)
    required = {"Replicaro.app": "directory", "Replicaro.app/Contents": "directory",
                "Replicaro.app/Contents/Info.plist": "file",
                "Replicaro.app/Contents/MacOS/Replicaro": "file",
                "Replicaro.app/Contents/Resources/LICENSES.txt": "file"}
    if any(entries.get(path, (None,))[0] != kind for path, kind in required.items()):
        fail("portable ZIP does not contain the required app bundle")
    provenance_name = "Replicaro.app/Contents/Resources/provenance/product-provenance.json"
    if entries.get(provenance_name, (None,))[0] != "file":
        fail("portable ZIP does not contain embedded product provenance")
    with zipfile.ZipFile(portable) as archive:
        info = archive.getinfo(provenance_name)
        if info.file_size > PROVENANCE_LIMIT:
            fail("portable ZIP product provenance is oversized")
        raw = archive.read(info)

    def object_fields(pairs):
        if len({name for name, _ in pairs}) != len(pairs):
            fail("portable ZIP product provenance has duplicate fields")
        return dict(pairs)

    try:
        provenance = json.loads(raw, object_pairs_hook=object_fields)
    except (UnicodeDecodeError, json.JSONDecodeError):
        fail("portable ZIP product provenance is malformed")
    fields = {"schema", "target", "version", "buildTimestamp", "source", "sourceIdentity",
              "standaloneExecutableSha256", "signed"}
    if (not isinstance(provenance, dict) or set(provenance) != fields or
            provenance.get("schema") != "replicaro-product-provenance-v2" or
            provenance.get("target") != target or provenance.get("version") != release_version or
            provenance.get("source") != "https://github.com/replicaro/replicaro" or
            not re.fullmatch(r"sha256:[0-9a-f]{64}", provenance.get("sourceIdentity", "")) or
            not re.fullmatch(r"sha256:[0-9a-f]{64}", provenance.get("standaloneExecutableSha256", "")) or
            not re.fullmatch(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z",
                             provenance.get("buildTimestamp", "")) or
            not isinstance(provenance.get("signed"), bool)):
        fail("portable ZIP product provenance contradicts the selected release")
    executable_digest = entries["Replicaro.app/Contents/MacOS/Replicaro"][2][1]
    if not provenance["signed"] and provenance["standaloneExecutableSha256"] != "sha256:" + executable_digest:
        fail("portable ZIP executable differs from unsigned product provenance")
    return entries


def mounted_inventory(root):
    entries = {}

    def visit(directory):
        for child in sorted(directory.iterdir()):
            relative = child.relative_to(root).as_posix()
            info = child.lstat()
            # ZIP entries do not carry macOS resource forks, so any fork in the
            # mounted app would be unverified file content.
            if has_resource_fork(child):
                fail("DMG app contains a resource fork absent from portable ZIP: " + relative)
            if stat.S_ISDIR(info.st_mode):
                entries[relative] = record("directory", info.st_mode)
                visit(child)
            elif stat.S_ISREG(info.st_mode):
                with child.open("rb") as source:
                    entries[relative] = record("file", info.st_mode, (info.st_size, digest(source)))
            elif stat.S_ISLNK(info.st_mode):
                entries[relative] = record("link", info.st_mode, os.fsencode(os.readlink(child)))
            else:
                fail("DMG app contains an unsupported entry: " + relative)

    visit(root)
    return entries


def verify_mounted_root(mount, expected):
    if set(os.listdir(mount)) != {".DS_Store", ".background", "Applications", "Replicaro.app"}:
        fail("DMG root inventory differs from the deterministic layout")
    applications = mount / "Applications"
    app = mount / "Replicaro.app"
    if not applications.is_symlink() or os.readlink(applications) != "/Applications":
        fail("DMG Applications link is missing or points elsewhere")
    if app.is_symlink() or not app.is_dir():
        fail("DMG Replicaro.app is missing or redirected")
    metadata = mount / ".DS_Store"
    background_directory = mount / ".background"
    background = background_directory / BACKGROUND
    if (metadata.is_symlink() or not metadata.is_file() or background_directory.is_symlink() or
            not background_directory.is_dir() or set(os.listdir(background_directory)) != {BACKGROUND} or
            background.is_symlink() or not background.is_file()):
        fail("DMG Finder layout files are missing, redirected, or not exact")
    for actual_path, source_path in ((metadata, LAYOUT / FINDER_TEMPLATE),
                                     (background, LAYOUT / BACKGROUND)):
        regular_input(source_path, "source-controlled DMG layout asset")
        with actual_path.open("rb") as actual_stream, source_path.open("rb") as source_stream:
            if digest(actual_stream) != digest(source_stream):
                fail("DMG Finder layout asset differs from source: " + actual_path.name)
    if png_dimensions(background) != BACKGROUND_DIMENSIONS:
        fail("DMG background dimensions differ from the Finder window")
    actual = mounted_inventory(mount)
    for layout_path in ("Applications", ".DS_Store", ".background", ".background/" + BACKGROUND):
        actual.pop(layout_path)
    if actual.keys() != expected.keys():
        missing = sorted(expected.keys() - actual.keys())
        extra = sorted(actual.keys() - expected.keys())
        fail("DMG app inventory differs from portable ZIP: missing=" + repr(missing) + " extra=" + repr(extra))
    for path in expected:
        if actual[path] != expected[path]:
            fail("DMG app content or mode differs from portable ZIP: " + path)


def verify_image(dmg, portable):
    regular_input(dmg, "DMG")
    regular_input(portable, "portable ZIP")
    match = PACKAGE.fullmatch(dmg.name)
    if not match or portable.name != f"Replicaro-{match[1]}-{match[2]}-portable.zip":
        fail("DMG and portable ZIP filenames do not identify the same macOS release")
    expected = archive_inventory(portable, match[1], match[2])
    temporary = pathlib.Path(tempfile.mkdtemp(prefix="replicaro-dmg-verify-"))
    mount = temporary / "mount"
    mount.mkdir()
    error = None
    try:
        subprocess.run(["hdiutil", "attach", "-quiet", "-readonly", "-nobrowse", "-mountpoint", str(mount), str(dmg)], check=True)
        if not os.path.ismount(mount):
            fail("hdiutil did not mount the DMG at the requested path")
        verify_mounted_root(mount, expected)
    except (OSError, ValueError, subprocess.CalledProcessError) as cause:
        error = cause
    finally:
        if os.path.ismount(mount):
            try:
                subprocess.run(["hdiutil", "detach", "-quiet", str(mount)], check=True)
            except (OSError, subprocess.CalledProcessError) as cause:
                error = RuntimeError(f"DMG detach failed after verification: {cause}; earlier failure: {error}")
            if os.path.ismount(mount):
                error = RuntimeError(f"DMG remained mounted after detach; earlier failure: {error}")
        if not os.path.ismount(mount):
            shutil.rmtree(temporary)
    if error is not None:
        raise error


def main():
    if len(sys.argv) != 3:
        fail("usage: verify_macos_dmg.py DMG PORTABLE_ZIP")
    if sys.platform != "darwin":
        fail("macOS is required to mount and verify a DMG")
    verify_image(pathlib.Path(os.path.abspath(sys.argv[1])), pathlib.Path(os.path.abspath(sys.argv[2])))
    print("DMG layout and app tree match the deterministic assets and companion portable ZIP")


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError, zipfile.BadZipFile) as error:
        raise SystemExit("macOS DMG verification: " + str(error)) from None
