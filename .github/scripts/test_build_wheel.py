"""Tests for build_wheel.py. Run: python3 -m unittest discover -s .github/scripts -v"""

import base64
import contextlib
import hashlib
import io
import stat
import tempfile
import unittest
import zipfile
from pathlib import Path

import build_wheel


class ReleaseTest(unittest.TestCase):
    def test_accepted_tags(self):
        for tag, want in {
            "v0.1.0": ("0.1.0", "final"),
            "v0.1.0-rc.1": ("0.1.0rc1", "rc"),
            "v0.1.0-alpha.1": ("0.1.0a1", "alpha"),
            "v1.0.0-rc.0": ("1.0.0rc0", "rc"),
            "v10.20.30-alpha.12": ("10.20.30a12", "alpha"),
        }.items():
            with self.subTest(tag=tag):
                self.assertEqual(build_wheel.release(tag), want)

    def test_refused_tags(self):
        for tag in [
            "",
            "0.1.0",
            "1.0.0-rc.10",
            "v0.1",
            "v0.1.0.0",
            "V0.1.0",
            "v0.1.0-beta.1",
            "v0.1.0-rc1",
            "v0.1.0-rc",
            "v0.1.0-alpha",
            "v0.1.0-a.1",
            "v0.1.0-dsf.1",
            "v01.1.0",
            "v0.01.0",
            "v0.1.00",
            "v0.1.0-rc.01",
            "v0.1.0+local",
            "v0.1.0-rc.1-extra",
            "v0.1.0-rc.1.alpha.1",
            "v0.1.0\n",
            " v0.1.0",
            "v١.1.0",
        ]:
            with self.subTest(tag=tag):
                with self.assertRaises(ValueError):
                    build_wheel.release(tag)

    def test_tag_command(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(build_wheel.main(["tag", "v0.1.0-alpha.1"]), 0)
        self.assertEqual(out.getvalue(), "version=0.1.0a1\nstage=alpha\n")

        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            self.assertEqual(build_wheel.main(["tag", "v0.1.0-beta.1"]), 1)
        self.assertEqual(out.getvalue(), "")
        self.assertIn("is not a release tag", err.getvalue())


class WheelTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.dir = Path(tmp.name)
        self.binary = self.dir / "slimtree-bin"
        self.binary.write_bytes(b"\x7fELF" + bytes(range(256)) * 64)
        self.binary.chmod(0o644)  # The wheel's mode must not come from the file's.
        self.licenses = [self.dir / "LICENSE", self.dir / "THIRD_PARTY_LICENSES"]
        self.licenses[0].write_text("Apache License\n")
        self.licenses[1].write_text("== golang.org/x/sys ==\nBSD\n")
        self.readme = self.dir / "README.md"
        self.readme.write_text("# slimtree\n\nDisk-light worktrees — alpha.\n", encoding="utf-8")

    def build(self, target, out="dist"):
        return build_wheel.wheel(self.binary, target, "0.1.0a1", self.licenses, self.readme, self.dir / out)

    def test_name_and_tags(self):
        for target, platform, tags in [
            (
                "linux-amd64",
                "manylinux_2_17_x86_64.manylinux2014_x86_64.musllinux_1_1_x86_64",
                ["manylinux_2_17_x86_64", "manylinux2014_x86_64", "musllinux_1_1_x86_64"],
            ),
            (
                "linux-arm64",
                "manylinux_2_17_aarch64.manylinux2014_aarch64.musllinux_1_1_aarch64",
                ["manylinux_2_17_aarch64", "manylinux2014_aarch64", "musllinux_1_1_aarch64"],
            ),
            ("darwin-arm64", "macosx_13_0_arm64", ["macosx_13_0_arm64"]),
        ]:
            with self.subTest(target=target):
                path = self.build(target, out=target)
                self.assertEqual(path.name, f"slimtree-0.1.0a1-py3-none-{platform}.whl")
                with zipfile.ZipFile(path) as zf:
                    wheel = zf.read("slimtree-0.1.0a1.dist-info/WHEEL").decode()
                self.assertEqual(
                    wheel,
                    "Wheel-Version: 1.0\nGenerator: build_wheel.py\nRoot-Is-Purelib: false\n"
                    + "".join(f"Tag: py3-none-{t}\n" for t in tags),
                )

    def test_layout_mode_and_contents(self):
        with zipfile.ZipFile(self.build("linux-amd64")) as zf:
            self.assertEqual(
                zf.namelist(),
                [
                    "slimtree-0.1.0a1.data/scripts/slimtree",
                    "slimtree-0.1.0a1.dist-info/METADATA",
                    "slimtree-0.1.0a1.dist-info/WHEEL",
                    "slimtree-0.1.0a1.dist-info/licenses/LICENSE",
                    "slimtree-0.1.0a1.dist-info/licenses/THIRD_PARTY_LICENSES",
                    "slimtree-0.1.0a1.dist-info/RECORD",
                ],
            )
            script = zf.getinfo("slimtree-0.1.0a1.data/scripts/slimtree")
            self.assertEqual(script.external_attr >> 16, stat.S_IFREG | 0o755)
            for info in zf.infolist():
                if info is not script:
                    self.assertEqual(info.external_attr >> 16, stat.S_IFREG | 0o644, info.filename)
            self.assertEqual(zf.read(script), self.binary.read_bytes())
            self.assertEqual(zf.read("slimtree-0.1.0a1.dist-info/licenses/THIRD_PARTY_LICENSES"), self.licenses[1].read_bytes())
            self.assertEqual(
                zf.read("slimtree-0.1.0a1.dist-info/METADATA").decode(),
                "Metadata-Version: 2.4\n"
                "Name: slimtree\n"
                "Version: 0.1.0a1\n"
                f"Summary: {build_wheel.SUMMARY}\n"
                "License-Expression: Apache-2.0\n"
                "License-File: LICENSE\n"
                "License-File: THIRD_PARTY_LICENSES\n"
                "Project-URL: Source, https://github.com/dsfaccini/artifact-fs\n"
                "Project-URL: Issues, https://github.com/dsfaccini/artifact-fs/issues\n"
                "Description-Content-Type: text/markdown\n"
                "\n" + self.readme.read_text(encoding="utf-8"),
            )

    def test_record_hashes_every_other_file(self):
        with zipfile.ZipFile(self.build("darwin-arm64")) as zf:
            lines = zf.read("slimtree-0.1.0a1.dist-info/RECORD").decode().splitlines()
            self.assertEqual(lines[-1], "slimtree-0.1.0a1.dist-info/RECORD,,")
            want = []
            for name in zf.namelist()[:-1]:
                data = zf.read(name)
                digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
                want.append(f"{name},sha256={digest},{len(data)}")
            self.assertEqual(lines[:-1], want)

    def test_same_inputs_same_bytes(self):
        a = self.build("linux-arm64", out="a").read_bytes()
        b = self.build("linux-arm64", out="b").read_bytes()
        self.assertEqual(a, b)

    def test_unknown_target_is_refused(self):
        with self.assertRaises(KeyError):
            self.build("darwin-amd64")


if __name__ == "__main__":
    unittest.main()
