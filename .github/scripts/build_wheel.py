#!/usr/bin/env python3
"""Map a release tag to its version, and wrap a slimtree binary in a wheel.

  build_wheel.py tag v0.1.0-alpha.1
      prints version=0.1.0a1 and stage=alpha, for $GITHUB_OUTPUT
  build_wheel.py wheel --binary B --target linux-amd64 --version V \\
      --license LICENSE --license THIRD_PARTY_LICENSES --readme README.md --out dist
      writes dist/slimtree-V-py3-none-<platform tags>.whl

The wheel holds the binary as slimtree-V.data/scripts/slimtree, mode 0755,
which an installer puts in the environment's bin directory as it is.
"""

import argparse
import base64
import hashlib
import re
import sys
import zipfile
from pathlib import Path

# A release tag, without leading zeros, because PEP 440 drops them and the tag
# would then differ from the version.
TAG = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(alpha|rc)\.(0|[1-9][0-9]*))?")
PRE = {"alpha": "a", "rc": "rc"}

# The binary is static, so one Linux wheel serves glibc and musl. Go 1.27
# links darwin/arm64 binaries for macOS 13.0 and later.
PLATFORMS = {
    "linux-amd64": ("manylinux_2_17_x86_64", "manylinux2014_x86_64", "musllinux_1_1_x86_64"),
    "linux-arm64": ("manylinux_2_17_aarch64", "manylinux2014_aarch64", "musllinux_1_1_aarch64"),
    "darwin-arm64": ("macosx_13_0_arm64",),
}

SUMMARY = "Disk-light git worktrees from APFS clones"
SOURCE = "https://github.com/dsfaccini/artifact-fs"


def release(tag):
    """Return the PEP 440 version of tag and its stage: alpha, rc or final."""
    m = TAG.fullmatch(tag)
    if not m:
        raise ValueError(f"{tag!r} is not a release tag: use vX.Y.Z, vX.Y.Z-rc.N or vX.Y.Z-alpha.N")
    major, minor, patch, pre, n = m.groups()
    version = f"{major}.{minor}.{patch}"
    if pre:
        return version + PRE[pre] + n, pre
    return version, "final"


def metadata(version, licenses, readme):
    fields = [
        ("Metadata-Version", "2.4"),
        ("Name", "slimtree"),
        ("Version", version),
        ("Summary", SUMMARY),
        ("License-Expression", "Apache-2.0"),
        *[("License-File", lic.name) for lic in licenses],
        ("Project-URL", f"Source, {SOURCE}"),
        ("Project-URL", f"Issues, {SOURCE}/issues"),
        ("Description-Content-Type", "text/markdown"),
    ]
    head = "".join(f"{k}: {v}\n" for k, v in fields)
    return (head + "\n" + readme.read_text(encoding="utf-8")).encode()


def wheel(binary, target, version, licenses, readme, out):
    """Write the wheel of binary for target into out and return its path."""
    # A compressed tag set is sorted.
    tags = sorted(PLATFORMS[target])
    dist = f"slimtree-{version}"
    info = f"{dist}.dist-info"
    files = [(f"{dist}.data/scripts/slimtree", binary.read_bytes(), 0o755)]
    files.append((f"{info}/METADATA", metadata(version, licenses, readme), 0o644))
    wheel_file = "Wheel-Version: 1.0\nGenerator: build_wheel.py\nRoot-Is-Purelib: false\n"
    wheel_file += "".join(f"Tag: py3-none-{t}\n" for t in tags)
    files.append((f"{info}/WHEEL", wheel_file.encode(), 0o644))
    files += [(f"{info}/licenses/{lic.name}", lic.read_bytes(), 0o644) for lic in licenses]
    record = ""
    for name, data, _ in files:
        digest = base64.urlsafe_b64encode(hashlib.sha256(data).digest()).rstrip(b"=").decode()
        record += f"{name},sha256={digest},{len(data)}\n"
    files.append((f"{info}/RECORD", (record + f"{info}/RECORD,,\n").encode(), 0o644))

    out.mkdir(parents=True, exist_ok=True)
    path = out / f"{dist}-py3-none-{'.'.join(tags)}.whl"
    with zipfile.ZipFile(path, "w") as zf:
        for name, data, mode in files:
            # A fixed time makes the wheel depend only on its contents.
            entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.external_attr = (0o100000 | mode) << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            zf.writestr(entry, data)
    return path


def main(argv):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("tag").add_argument("tag")
    w = sub.add_parser("wheel")
    w.add_argument("--binary", type=Path, required=True)
    w.add_argument("--target", choices=sorted(PLATFORMS), required=True)
    w.add_argument("--version", required=True)
    w.add_argument("--license", type=Path, action="append", required=True)
    w.add_argument("--readme", type=Path, required=True)
    w.add_argument("--out", type=Path, required=True)
    args = parser.parse_args(argv)
    if args.command == "tag":
        try:
            version, stage = release(args.tag)
        except ValueError as e:
            print(e, file=sys.stderr)
            return 1
        print(f"version={version}\nstage={stage}")
        return 0
    print(wheel(args.binary, args.target, args.version, args.license, args.readme, args.out))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
