"""Prepare the pinned Windows cross toolchain and build selected audio targets."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TOOLS = ROOT / ".tools/audio-build"
LOCK = json.loads((ROOT / "packaging/audio/sources.lock.json").read_text(encoding="utf-8"))


def prepare():
    TOOLS.mkdir(parents=True, exist_ok=True)
    for item in LOCK["downloads"]:
        archive = TOOLS / item["name"]
        if not archive.exists():
            partial = archive.with_name(archive.name + ".part")
            with urllib.request.urlopen(item["url"], timeout=120) as response, partial.open("wb") as output:
                shutil.copyfileobj(response, output)
            partial.replace(archive)
        if hashlib.sha256(archive.read_bytes()).hexdigest() != item["sha256"]:
            raise RuntimeError("Checksum mismatch: " + item["name"])
        destination = TOOLS / item["directory"]
        if destination.exists():
            continue
        if archive.suffix == ".zip":
            with zipfile.ZipFile(archive) as content:
                content.extractall(TOOLS)
        elif archive.name.endswith(".zst"):
            destination.mkdir()
            subprocess.run(["tar", "-xf", str(archive), "-C", str(destination)], check=True)
        else:
            with tarfile.open(archive) as content:
                content.extractall(TOOLS, filter="data")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", choices=["all", "amd64", "arm64", "windows-amd64"], default="all")
    parser.add_argument("--jobs", type=int, default=4)
    parser.add_argument("--prepare-only", action="store_true")
    parser.add_argument("--clean", action="store_true", help="Remove only selected target build directories before rebuilding")
    args = parser.parse_args()
    if os.name != "nt":
        raise SystemExit("This pinned toolchain currently uses Windows Zig + Git Bash.")
    prepare()
    if args.prepare_only:
        return
    bash = os.environ.get("MICAST_TEST_BASH") or shutil.which("bash")
    if Path("C:/Tools/Git/bin/bash.exe").exists():
        bash = "C:/Tools/Git/bin/bash.exe"
    if not bash:
        raise SystemExit("Git Bash is required")
    targets = ["amd64", "arm64"] if args.target == "all" else [args.target]
    for target in targets:
        work = TOOLS / ("work-" + target)
        if args.clean and work.exists():
            if work.is_symlink() or work.resolve().parent != TOOLS.resolve():
                raise RuntimeError("Refusing to clean an unexpected build path")
            shutil.rmtree(work)
        subprocess.run([bash, "scripts/build-audio.sh", target], cwd=ROOT,
                       env=os.environ | {"JOBS": str(max(1, args.jobs))}, check=True)


if __name__ == "__main__":
    main()
