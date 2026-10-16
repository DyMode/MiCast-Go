"""Package locally built runtimes and the matching source/build attachment."""
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import struct
import subprocess
import tarfile

ROOT = Path(__file__).resolve().parents[1]
TOOLS = ROOT / ".tools/audio-build"
LOCK = json.loads((ROOT / "packaging/audio/sources.lock.json").read_text(encoding="utf-8"))
OUTPUT = ROOT / "dist/audio"
EPOCH = 1781913600


def add_file(archive, name, content, executable=False):
    info = tarfile.TarInfo(name)
    info.size = len(content)
    info.mode = 0o755 if executable else 0o644
    info.mtime = EPOCH
    archive.addfile(info, io.BytesIO(content))


def pack(path, files):
    with path.open("wb") as output, gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=EPOCH) as zipped:
        with tarfile.open(fileobj=zipped, mode="w") as archive:
            for name, data, executable in sorted(files):
                add_file(archive, name, data, executable)
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    Path(str(path) + ".sha256").write_text(digest + "  " + path.name + "\n", encoding="utf-8")
    return digest


def static_elf(data, machine):
    assert data[:4] == b"\x7fELF" and data[4:6] == bytes([2, 1])
    assert struct.unpack_from("<H", data, 18)[0] == machine
    offset = struct.unpack_from("<Q", data, 32)[0]
    size, count = struct.unpack_from("<HH", data, 54)
    for index in range(count):
        header = struct.unpack_from("<IIQQQQQQ", data, offset + index * size)
        assert header[0] != 3, "Unexpected dynamic interpreter"
        if header[0] == 2:
            for at in range(header[2], header[2] + header[5], 16):
                assert struct.unpack_from("<Q", data, at)[0] != 1, "Unexpected shared-library dependency"


def main():
    OUTPUT.mkdir(parents=True, exist_ok=True)
    validation = json.loads((ROOT / ".run/reports/audio-runtime-results.json").read_text(encoding="utf-8"))
    native_binary = TOOLS / "work-windows-amd64/output/ffmpeg.exe"
    assert validation["target_feature_sets_equal"]
    assert validation["runtime_sha256"] == hashlib.sha256(native_binary.read_bytes()).hexdigest()
    zig = TOOLS / "zig-x86_64-windows-0.14.1/zig.exe"
    result = []
    source_files = []
    source_files.append(("LICENSE", (ROOT / "LICENSE").read_bytes(), False))
    source_files.append(("packaging/audio/BUILDING.txt", (ROOT / "packaging/audio/BUILDING.txt").read_bytes(), False))
    for item in LOCK["downloads"]:
        if item["name"].startswith(("ffmpeg-", "lame-")):
            data = (TOOLS / item["name"]).read_bytes()
            assert hashlib.sha256(data).hexdigest() == item["sha256"]
            source_files.append(("sources/" + item["name"], data, False))
    for filename in ["build-audio.sh", "build-audio.py", "package-audio.py", "check-audio-runtime.py"]:
        source_files.append(("scripts/" + filename, (ROOT / "scripts" / filename).read_bytes(), filename.endswith(".sh")))
    source_files.append(("packaging/audio/sources.lock.json", (ROOT / "packaging/audio/sources.lock.json").read_bytes(), False))
    source_files.append(("build-info/audio-runtime-results.json", (ROOT / ".run/reports/audio-runtime-results.json").read_bytes(), False))
    for arch, machine in [("amd64", 62), ("arm64", 183)]:
        work = TOOLS / ("work-" + arch)
        binary = work / "output/ffmpeg"
        stripped = work / "output/ffmpeg.stripped"
        subprocess.run([str(zig), "objcopy", "--strip-all", str(binary), str(stripped)], check=True)
        data = stripped.read_bytes()
        static_elf(data, machine)
        config = (work / "ffmpeg/config.h").read_text(encoding="utf-8")
        assert "#define CONFIG_GPL 0" in config and "#define CONFIG_NONFREE 0" in config
        digest = hashlib.sha256(data).hexdigest()
        info = dict(arch=arch, ffmpeg=LOCK["ffmpeg"], lame=LOCK["lame"], zig=LOCK["zig"],
                    binary_sha256=digest, binary_bytes=len(data), static=True, source_patches=[],
                    license="LGPL-2.1-or-later", sources_lock=LOCK)
        root = "ffmpeg-" + LOCK["ffmpeg"] + "-micast-" + arch
        readme = ("MiCast audio-only FFmpeg " + LOCK["ffmpeg"] + "\n"
                  "Built from unmodified FFmpeg and LAME sources using scripts/build-audio.sh.\n"
                  "LGPL-2.1-or-later; see LICENSE.txt and component notices.\n"
                  "Full source/build attachment: micast-audio-sources-" + LOCK["ffmpeg"] + ".tar.gz\n")
        notices = [("LICENSE.txt", (TOOLS / ("ffmpeg-" + LOCK["ffmpeg"]) / "COPYING.LGPLv2.1").read_bytes()),
                   ("LAME-COPYING", (TOOLS / ("lame-" + LOCK["lame"]) / "COPYING").read_bytes()),
                   ("MUSL-COPYRIGHT", (zig.parent / "lib/libc/musl/COPYRIGHT").read_bytes()),
                   ("ZIG-LICENSE", (zig.parent / "LICENSE").read_bytes())]
        files = [(root + "/ffmpeg", data, True), (root + "/readme.txt", readme.encode(), False),
                 (root + "/BUILD-INFO.json", (json.dumps(info, indent=2) + "\n").encode(), False)]
        files.extend((root + "/" + name, content, False) for name, content in notices)
        archive = OUTPUT / (root + ".tar.gz")
        archive_sha = pack(archive, files)
        result.append(dict(arch=arch, version=LOCK["ffmpeg"], archive_file=archive.name, runtime_dir=root,
                           archive_sha256=archive_sha, binary_sha256=digest, binary_bytes=len(data),
                           license_file="LICENSE.txt", local=True))
        for filename in ["config.h", "config_components.h", "ffbuild/config.mak"]:
            source_files.append(("build-info/" + arch + "/ffmpeg/" + filename, (work / "ffmpeg" / filename).read_bytes(), False))
        # Raw configure logs dump the developer's environment. Publish only
        # the invocation and generated configuration, never those raw logs.
        invocation = (work / "ffmpeg/ffbuild/config.log").read_text(encoding="utf-8").splitlines()[0] + "\n"
        source_files.append(("build-info/" + arch + "/ffmpeg/CONFIGURE.txt", invocation.encode(), False))
        for filename in ["config.h"]:
            source_files.append(("build-info/" + arch + "/lame/" + filename, (work / "lame" / filename).read_bytes(), False))
        source_files.extend(("notices/" + name, content, False) for name, content in notices)
    # Include linked libc/compiler runtime sources shipped by the pinned Zig SDK.
    for directory in ["lib/libc/musl", "lib/compiler_rt"]:
        for path in sorted((zig.parent / directory).rglob("*")):
            if path.is_file():
                source_files.append(("toolchain/" + path.relative_to(zig.parent).as_posix(), path.read_bytes(), False))
    source_files.append(("toolchain/lib/compiler_rt.zig", (zig.parent / "lib/compiler_rt.zig").read_bytes(), False))
    instructions = ("MiCast audio runtime source/build materials\n\n"
                    "Sources are unmodified upstream archives; hashes and tool versions are pinned.\n"
                    "On Windows with Python 3.12+, Git Bash and tar: python scripts/build-audio.py --clean\n"
                    "This downloads the pinned SDK and verifies all archives before building.\n"
                    "Outputs: .tools/audio-build/work-{amd64,arm64}/output/ffmpeg.\n"
                    "The full Zig SDK (including its standard library) is fetched from its pinned URL.\n"
                    "Record of tested native configuration: build-info/. Linux runtime execution requires NAS/Linux verification.\n")
    source_files.append(("README.txt", instructions.encode(), False))
    source_files = list({name: (name, data, executable) for name, data, executable in source_files}.values())
    source_archive = OUTPUT / ("micast-audio-sources-" + LOCK["ffmpeg"] + ".tar.gz")
    source_sha = pack(source_archive, source_files)
    for item in result:
        item.update(source_archive=source_archive.name, source_sha256=source_sha)
    (ROOT / "packaging/fnos").mkdir(parents=True, exist_ok=True)
    (ROOT / "packaging/fnos/audio-runtime-lock.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
