"""Independently verify both FPKs against the current service build."""
import hashlib
import io
import json
from pathlib import Path
import re
import struct
import tarfile

ROOT = Path(__file__).resolve().parents[1]
VERSION = re.search(r'const Version = "([^"]+)"', (ROOT / "internal/version/version.go").read_text(encoding="utf-8"))[1]
results = []
runtime_locks = json.loads((ROOT / "packaging/fnos/audio-runtime-lock.json").read_text(encoding="utf-8"))
for locked in runtime_locks:
    source = ROOT / "dist/fnos" / locked["source_archive"]
    assert hashlib.sha256(source.read_bytes()).hexdigest() == locked["source_sha256"], "Audio source attachment mismatch"
for platform, arch, machine in [("x86", "amd64", 62), ("arm", "arm64", 183)]:
    path = ROOT / "dist/fnos" / ("micast-go-" + platform + "-" + VERSION + ".fpk")
    raw = path.read_bytes()
    digest = hashlib.sha256(raw).hexdigest()
    assert digest in Path(str(path) + ".sha256").read_text(encoding="utf-8"), "FPK SHA256 mismatch"
    with tarfile.open(fileobj=io.BytesIO(raw)) as outer:
        manifest = outer.extractfile("manifest").read().decode("utf-8")
        expected_manifest = (ROOT / "packaging/fnos/manifest").read_text(encoding="utf-8")
        expected_manifest = re.sub(r"(?m)^platform=.+$", "platform=" + platform, expected_manifest)
        # All reviewed description lines, including multiline delimiters, must survive packaging.
        packaged_manifest = re.sub(r"(?m)^checksum=.*\n?", "", manifest)
        assert packaged_manifest.rstrip() == expected_manifest.rstrip(), "Packaged manifest differs from reviewed source"
        assert "version=" + VERSION + "\n" in manifest
        assert "platform=" + platform + "\n" in manifest
        license_text = outer.extractfile("LICENSE").read()
        assert license_text.replace(b"\r\n", b"\n") == (ROOT / "packaging/fnos/LICENSE").read_bytes().replace(b"\r\n", b"\n"), "Packaged LICENSE differs from reviewed source"
        wizard = outer.extractfile("wizard/install").read()
        assert wizard.replace(b"\r\n", b"\n") == (ROOT / "packaging/fnos/wizard/install").read_bytes().replace(b"\r\n", b"\n"), "Install wizard differs from reviewed source"
        app = outer.extractfile("app.tgz").read()
        assert hashlib.md5(app).hexdigest() in manifest, "app.tgz MD5 mismatch"
        hooks = []
        for source in (ROOT / "packaging/fnos/cmd").iterdir():
            member = outer.getmember("cmd/" + source.name)
            content = outer.extractfile(member).read()
            assert member.mode & 0o111 == 0o111, source.name + " not executable"
            assert b"\r" not in content, source.name + " contains CRLF"
            assert content == source.read_bytes().replace(b"\r\n", b"\n"), source.name + " differs from reviewed source"
            hooks.append(source.name)
    binaries = []
    with tarfile.open(fileobj=io.BytesIO(app)) as inner:
        members = inner.getmembers()
        names = {member.name.removeprefix("./") for member in members}
        assert "licenses/lucide.txt" in names
        assert "licenses/ffmpeg/LICENSE.txt" in names
        assert inner.extractfile("LICENSE").read().startswith(b"MIT License"), "Application LICENSE is not the MIT text"
        build_info = json.load(inner.extractfile("licenses/ffmpeg/BUILD-INFO.json"))
        assert build_info["license"] == "LGPL-2.1-or-later"
        locked = next(item for item in runtime_locks if item["arch"] == arch)
        assert build_info["binary_sha256"] == locked["binary_sha256"]
        for member in members:
            if member.name.removeprefix("./") not in ("bin/micast-go", "audio/ffmpeg"):
                continue
            data = inner.extractfile(member).read()
            assert data[:4] == b"\x7fELF"
            assert struct.unpack("<H", data[18:20])[0] == machine, "ELF architecture mismatch"
            assert member.mode & 0o111 == 0o111
            if member.name.endswith("bin/micast-go"):
                assert data == (ROOT / "dist" / ("micast-go-linux-" + arch)).read_bytes()
            binaries.append(dict(name=member.name, bytes=member.size, mode=oct(member.mode), sha256=hashlib.sha256(data).hexdigest()))
        assert len(binaries) == 2
        results.append(dict(file=path.name, platform=platform, version=VERSION, bytes=len(raw), sha256=digest,
                            unpacked_app_bytes=sum(m.size for m in members if m.isfile()),
                            manifest_checksum_verified=True, lifecycle_hooks_verified=hooks, binaries=binaries))
(ROOT / ".run/reports").mkdir(parents=True, exist_ok=True)
(ROOT / ".run/reports/fpk-verification.json").write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(json.dumps(results, ensure_ascii=False, indent=2))
