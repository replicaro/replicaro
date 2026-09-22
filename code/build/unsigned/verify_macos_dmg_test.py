#!/usr/bin/env python3
"""Focused release-gate tests for macOS DMG app content verification."""

import pathlib
import hashlib
import json
import os
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import unittest
import zipfile
from unittest import mock

import verify_macos_dmg as verifier


def write_zip(path, app, extra=None):
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for child in sorted([app, *app.rglob("*")]):
            name = child.relative_to(app.parent).as_posix()
            mode = child.lstat().st_mode
            info = zipfile.ZipInfo(name + ("/" if child.is_dir() and not child.is_symlink() else ""))
            info.create_system = 3
            info.external_attr = (mode & 0xFFFF) << 16
            if child.is_symlink():
                archive.writestr(info, child.readlink().as_posix().encode())
            elif child.is_dir():
                archive.writestr(info, b"")
            else:
                archive.writestr(info, child.read_bytes())
        if extra is not None:
            info = zipfile.ZipInfo(extra)
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | 0o644) << 16
            archive.writestr(info, b"unexpected")


def fixture(root):
    mount = root / "mount"
    app = mount / "Replicaro.app"
    (app / "Contents/MacOS").mkdir(parents=True)
    (app / "Contents/Resources").mkdir()
    (app / "Contents/Info.plist").write_bytes(b"plist")
    binary = app / "Contents/MacOS/Replicaro"
    binary.write_bytes(b"application bytes")
    binary.chmod(0o700)
    (app / "Contents/Resources/LICENSES.txt").write_bytes(b"licenses")
    provenance = app / "Contents/Resources/provenance"
    provenance.mkdir()
    (provenance / "product-provenance.json").write_text(
        '{"schema":"replicaro-product-provenance-v2","target":"darwin_arm64","version":"1.0.0",'
        '"buildTimestamp":"2026-09-21T00:00:00Z","source":"https://github.com/replicaro/replicaro",'
        '"sourceIdentity":"sha256:' + 'a' * 64 + '","standaloneExecutableSha256":"sha256:' +
        hashlib.sha256(binary.read_bytes()).hexdigest() + '","signed":false}\n', encoding="ascii")
    (app / "Contents/Resources/info-link").symlink_to("../Info.plist")
    (mount / "Applications").symlink_to("/Applications")
    (mount / ".background").mkdir()
    shutil.copyfile(verifier.LAYOUT / verifier.FINDER_TEMPLATE, mount / ".DS_Store")
    shutil.copyfile(verifier.LAYOUT / verifier.BACKGROUND,
                    mount / ".background" / verifier.BACKGROUND)
    portable = root / "Replicaro-1.0.0-darwin_arm64-portable.zip"
    write_zip(portable, app)
    return mount, app, portable


class MacOSDMGVerificationTests(unittest.TestCase):
    def verify_with_cleanup(self, dmg, portable, scratch):
        def make_temporary(**_):
            scratch.mkdir()
            return str(scratch)

        try:
            with mock.patch.object(verifier.tempfile, "mkdtemp", side_effect=make_temporary):
                verifier.verify_image(dmg, portable)
        finally:
            self.assertFalse(scratch.exists(), "verification left a temporary mount")

    def test_exact_app_tree_and_link(self):
        with tempfile.TemporaryDirectory() as directory:
            mount, _, portable = fixture(pathlib.Path(directory))
            verifier.verify_mounted_root(mount, verifier.archive_inventory(portable, "1.0.0", "darwin_arm64"))

    def test_source_controlled_layout_and_shared_staging_helper(self):
        metadata = (verifier.LAYOUT / verifier.FINDER_TEMPLATE).read_bytes()
        self.assertTrue(metadata.startswith(b"\x00\x00\x00\x01Bud1"))
        for record in (b"Iloc", b"icvp", b"bwsp"):
            self.assertIn(record, metadata)
        for name in ("Replicaro.app", "Applications"):
            self.assertIn(name.encode("utf-16-be"), metadata)
        # The Iloc records are the native Finder authority for icon centers;
        # keep artwork geometry tied to those exact encoded coordinates.
        for name, expected in (("Replicaro.app", (180, 260)), ("Applications", (640, 260))):
            marker = (len(name).to_bytes(4, "big") + name.encode("utf-16-be") +
                      b"Ilocblob" + (16).to_bytes(4, "big"))
            self.assertEqual(metadata.count(marker), 1)
            offset = metadata.index(marker) + len(marker)
            self.assertEqual(struct.unpack(">II", metadata[offset:offset + 8]), expected)
        png = (verifier.LAYOUT / verifier.BACKGROUND).read_bytes()
        self.assertEqual(png[:8], b"\x89PNG\r\n\x1a\n")
        self.assertEqual(tuple(int.from_bytes(png[offset:offset + 4], "big") for offset in (16, 20)),
                         verifier.BACKGROUND_DIMENSIONS)
        render = verifier.LAYOUT / "render-installer-background.py"
        subprocess.run([sys.executable, str(render), "--check=" + str(verifier.LAYOUT / verifier.BACKGROUND)],
                       check=True)

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            app = root / "input.app"
            (app / "Contents").mkdir(parents=True)
            (app / "Contents/item").write_bytes(b"app v1")
            output = root / "stage"
            output.mkdir()
            tools = root / "tools"
            tools.mkdir()
            ditto = tools / "ditto"
            ditto.write_text('#!/bin/sh\ncp -R "$1" "$2"\n', encoding="ascii")
            ditto.chmod(0o755)
            helper = verifier.LAYOUT.parent / "stage-macos-dmg.sh"
            environment = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"])
            subprocess.run(["bash", str(helper), "create", str(app), str(output)], env=environment, check=True)
            self.assertEqual(set(os.listdir(output)), {".DS_Store", ".background", "Applications", "Replicaro.app"})
            self.assertEqual(os.readlink(output / "Applications"), "/Applications")
            self.assertEqual((output / ".DS_Store").read_bytes(), metadata)
            self.assertEqual((output / ".background" / verifier.BACKGROUND).read_bytes(), png)
            (app / "Contents/item").write_bytes(b"app v2")
            subprocess.run(["bash", str(helper), "replace-app", str(app), str(output)], env=environment, check=True)
            self.assertEqual((output / "Replicaro.app/Contents/item").read_bytes(), b"app v2")

            for changed in (output / ".DS_Store", output / ".background" / verifier.BACKGROUND):
                changed.write_bytes(b"changed")
                result = subprocess.run(["bash", str(helper), "replace-app", str(app), str(output)],
                                        env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                self.assertNotEqual(result.returncode, 0)
                if changed.name == ".DS_Store":
                    changed.write_bytes(metadata)

    def test_changed_or_extra_content_fails_at_tree_comparison(self):
        changes = {
            "file bytes": lambda mount, app: (app / "Contents/Info.plist").write_bytes(b"different"),
            "file mode": lambda mount, app: (app / "Contents/MacOS/Replicaro").chmod(0o755),
            "extra app file": lambda mount, app: (app / "Contents/extra").write_bytes(b"extra"),
            "missing app file": lambda mount, app: (app / "Contents/Info.plist").unlink(),
            "wrong app symlink": lambda mount, app: ((app / "Contents/Resources/info-link").unlink(), (app / "Contents/Resources/info-link").symlink_to("/tmp")),
            "extra root file": lambda mount, app: (mount / "extra").write_bytes(b"extra"),
            "wrong Applications link": lambda mount, app: ((mount / "Applications").unlink(), (mount / "Applications").symlink_to("/tmp")),
        }
        for name, change in changes.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                mount, app, portable = fixture(pathlib.Path(directory))
                expected = verifier.archive_inventory(portable, "1.0.0", "darwin_arm64")
                change(mount, app)
                with self.assertRaises(ValueError):
                    verifier.verify_mounted_root(mount, expected)

    def test_unsafe_zip_entry_fails_before_mount(self):
        with tempfile.TemporaryDirectory() as directory:
            mount, app, portable = fixture(pathlib.Path(directory))
            write_zip(portable, app, "Replicaro.app/../other")
            with self.assertRaises(ValueError):
                verifier.archive_inventory(portable, "1.0.0", "darwin_arm64")

    def test_product_provenance_rejects_stale_or_mismatched_identity(self):
        cases = {
            "version": lambda value: value.update(version="1.0.1"),
            "target": lambda value: value.update(target="darwin_amd64"),
            "timestamp": lambda value: value.update(buildTimestamp="not-a-timestamp"),
            "source": lambda value: value.update(source="https://example.invalid/source"),
            "source identity": lambda value: value.update(sourceIdentity="sha256:" + "z" * 64),
            "executable digest": lambda value: value.update(standaloneExecutableSha256="sha256:" + "c" * 64),
            "signed state": lambda value: value.update(signed="false"),
        }
        for name, mutate in cases.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                _, app, portable = fixture(pathlib.Path(directory))
                path = app / "Contents/Resources/provenance/product-provenance.json"
                value = json.loads(path.read_text())
                mutate(value)
                path.write_text(json.dumps(value, separators=(",", ":")) + "\n", encoding="ascii")
                write_zip(portable, app)
                with self.assertRaises(ValueError):
                    verifier.archive_inventory(portable, "1.0.0", "darwin_arm64")

    def test_product_provenance_rejects_missing_redirected_and_oversized_records(self):
        changes = {
            "missing": lambda path: path.unlink(),
            "redirected": lambda path: (path.unlink(), path.symlink_to("../../Info.plist")),
            "oversized": lambda path: path.write_bytes(b" " * (verifier.PROVENANCE_LIMIT + 1)),
        }
        for name, change in changes.items():
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                _, app, portable = fixture(pathlib.Path(directory))
                change(app / "Contents/Resources/provenance/product-provenance.json")
                write_zip(portable, app)
                with self.assertRaises(ValueError):
                    verifier.archive_inventory(portable, "1.0.0", "darwin_arm64")

    def test_resource_fork_content_fails_at_app_inventory(self):
        with tempfile.TemporaryDirectory() as directory:
            mount, app, portable = fixture(pathlib.Path(directory))
            target = app / "Contents/Info.plist"
            with mock.patch.object(verifier, "has_resource_fork", side_effect=lambda path: path == target):
                with self.assertRaisesRegex(ValueError, "resource fork absent from portable ZIP: Replicaro.app/Contents/Info.plist"):
                    verifier.verify_mounted_root(mount, verifier.archive_inventory(portable, "1.0.0", "darwin_arm64"))

    def test_macos_xattr_probe_uses_symlink_safe_flags(self):
        path = pathlib.Path("/tmp/Replicaro.app/Contents/Info.plist")
        calls = []

        def run(command, check, stdout):
            calls.append(command)
            value = b"com.apple.ResourceFork\n" if len(calls) == 1 else b"66 6f 72 6b\n"
            return subprocess.CompletedProcess(command, 0, stdout=value)

        with mock.patch.object(verifier.sys, "platform", "darwin"), \
                mock.patch.object(verifier.subprocess, "run", side_effect=run):
            self.assertTrue(verifier.has_resource_fork(path))
        self.assertEqual(calls, [
            ["xattr", "-s", str(path)],
            ["xattr", "-p", "-x", "-s", "com.apple.ResourceFork", str(path)],
        ])

    def test_partial_attach_failure_detaches_and_cleans(self):
        for initially_mounted in (False, True):
            with self.subTest(initially_mounted=initially_mounted), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                _, _, portable = fixture(root)
                dmg = root / "Replicaro-1.0.0-darwin_arm64.dmg"
                dmg.write_bytes(b"test image")
                scratch = root / "verification"
                mounted = initially_mounted
                operations = []

                def run(command, check):
                    nonlocal mounted
                    operations.append(command[1])
                    if command[1] == "attach":
                        raise subprocess.CalledProcessError(1, command)
                    mounted = False
                    return subprocess.CompletedProcess(command, 0)

                with mock.patch.object(verifier.subprocess, "run", side_effect=run), \
                        mock.patch.object(verifier.os.path, "ismount", side_effect=lambda _: mounted):
                    with self.assertRaises(subprocess.CalledProcessError):
                        self.verify_with_cleanup(dmg, portable, scratch)
                self.assertEqual(operations, ["attach", "detach"] if initially_mounted else ["attach"])

    def test_detach_failure_and_remaining_mount_fail_without_recursive_cleanup(self):
        for outcome in ("detach failure", "still mounted"):
            with self.subTest(outcome=outcome), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                _, _, portable = fixture(root)
                dmg = root / "Replicaro-1.0.0-darwin_arm64.dmg"
                dmg.write_bytes(b"test image")
                scratch = root / "verification"
                operations = []
                mounted = True

                def make_temporary(**_):
                    scratch.mkdir()
                    return str(scratch)

                def run(command, check):
                    nonlocal mounted
                    operations.append(command[1])
                    if command[1] == "detach":
                        if outcome == "detach failure":
                            mounted = False
                            raise subprocess.CalledProcessError(1, command)
                    return subprocess.CompletedProcess(command, 0)

                with mock.patch.object(verifier.tempfile, "mkdtemp", side_effect=make_temporary), \
                        mock.patch.object(verifier.subprocess, "run", side_effect=run), \
                        mock.patch.object(verifier.os.path, "ismount", side_effect=lambda _: mounted), \
                        mock.patch.object(verifier, "verify_mounted_root"), \
                        mock.patch.object(verifier.shutil, "rmtree", wraps=verifier.shutil.rmtree) as remove:
                    expected = "detach failed" if outcome == "detach failure" else "remained mounted"
                    with self.assertRaisesRegex(RuntimeError, expected):
                        verifier.verify_image(dmg, portable)
                    if outcome == "detach failure":
                        remove.assert_called_once_with(scratch)
                    else:
                        remove.assert_not_called()
                self.assertEqual(operations, ["attach", "detach"])
                self.assertEqual(scratch.exists(), outcome == "still mounted")

    @unittest.skipUnless(sys.platform == "darwin", "native DMG mount requires macOS")
    def test_actual_hdiutil_mount_and_content_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            mount, app, portable = fixture(root)
            dmg = root / "Replicaro-1.0.0-darwin_arm64.dmg"
            subprocess.run(["hdiutil", "create", "-quiet", "-format", "UDZO", "-volname", "Replicaro",
                            "-srcfolder", str(mount), str(dmg)], check=True)
            self.verify_with_cleanup(dmg, portable, root / "verify-success")

            data = (app / "Contents/Info.plist").read_bytes()
            mode = (app / "Contents/Info.plist").stat().st_mode
            subprocess.run(["xattr", "-w", "-s", "com.apple.ResourceFork", "fork bytes",
                            str(app / "Contents/Info.plist")], check=True)
            self.assertEqual((app / "Contents/Info.plist").read_bytes(), data)
            self.assertEqual((app / "Contents/Info.plist").stat().st_mode, mode)
            fork_dir = root / "fork"
            fork_dir.mkdir()
            fork_dmg = fork_dir / dmg.name
            subprocess.run(["hdiutil", "create", "-quiet", "-format", "UDZO", "-volname", "Replicaro",
                            "-srcfolder", str(mount), str(fork_dmg)], check=True)
            with self.assertRaisesRegex(ValueError, "resource fork absent from portable ZIP: Replicaro.app/Contents/Info.plist"):
                self.verify_with_cleanup(fork_dmg, portable, root / "verify-resource-fork")

            (app / "Contents/Info.plist").write_bytes(b"changed portable content")
            write_zip(portable, app)
            with self.assertRaisesRegex(ValueError, "DMG app content or mode differs from portable ZIP: Replicaro.app/Contents/Info.plist"):
                self.verify_with_cleanup(dmg, portable, root / "verify-content-failure")


if __name__ == "__main__":
    unittest.main()
