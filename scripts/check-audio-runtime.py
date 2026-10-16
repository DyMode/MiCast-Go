"""Exercise the trimmed native runtime against fixtures from a full FFmpeg."""
import argparse
import hashlib
import json
import math
from pathlib import Path
import shutil
import struct
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def run(binary, args, data=None):
    result = subprocess.run([str(binary), "-hide_banner", "-loglevel", "error", "-nostdin", *args],
                            input=data, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(result.stderr.decode("utf-8", errors="replace"))
    return result.stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--runtime", type=Path, default=ROOT / ".tools/audio-build/work-windows-amd64/output/ffmpeg.exe")
    parser.add_argument("--reference", default=shutil.which("ffmpeg"))
    args = parser.parse_args()
    if not args.reference:
        raise SystemExit("A reference FFmpeg is required to produce fixtures")
    (ROOT / ".run").mkdir(exist_ok=True)
    (ROOT / ".run/reports").mkdir(parents=True, exist_ok=True)
    pcm = b"".join(struct.pack("<hh", int(6000 * math.sin(i * 2 * math.pi * 440 / 44100)),
                               int(4000 * math.sin(i * 2 * math.pi * 660 / 44100))) for i in range(22050))
    fixtures = [("mp3", "libmp3lame", "mp3"), ("aac", "aac", "adts"), ("m4a", "aac", "ipod"),
                ("alac", "alac", "ipod"), ("flac", "flac", "flac"), ("wav", "pcm_s16le", "wav"),
                ("opus", "libopus", "ogg"), ("vorbis", "libvorbis", "ogg")]
    results = []
    # Compare enabled codec/container/filter feature sets across all three builds.
    feature_sets = {}
    for target in ["amd64", "arm64", "windows-amd64"]:
        text = (ROOT / (".tools/audio-build/work-" + target + "/ffmpeg/config_components.h")).read_text(encoding="utf-8")
        feature_sets[target] = sorted(line for line in text.splitlines() if line.endswith(" 1"))
    assert feature_sets["amd64"] == feature_sets["arm64"] == feature_sets["windows-amd64"], "Target feature sets differ"
    with tempfile.TemporaryDirectory(prefix="runtime-check-", dir=ROOT / ".run") as directory:
        directory = Path(directory)
        for name, codec, container in fixtures:
            path = directory / (name + "." + ("m4a" if container == "ipod" else container))
            run(args.reference, ["-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
                                 "-c:a", codec, "-f", container, str(path)], pcm)
            decoded = run(args.runtime, ["-protocol_whitelist", "file,pipe", "-i", str(path),
                                         "-ar", "44100", "-ac", "2", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1"])
            assert abs(len(decoded) - len(pcm)) < 16384, name + " duration mismatch"
            if name in ("alac", "flac", "wav"):
                assert decoded == pcm, name + " lossless decode mismatch"
            results.append(dict(input=name, decoded_bytes=len(decoded)))
        for output, codec, container in [("mp3", "libmp3lame", "mp3"), ("flac", "flac", "flac"),
                                          ("wav", "pcm_s16le", "wav"), ("pcm", "pcm_s16le", "s16le")]:
            encoded = run(args.runtime, ["-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
                                         "-c:a", codec, "-f", container, "pipe:1"], pcm)
            raw = encoded if output == "pcm" else run(args.reference, ["-i", "pipe:0", "-f", "s16le", "-c:a", "pcm_s16le", "pipe:1"], encoded)
            assert abs(len(raw) - len(pcm)) < 16384
            if output != "mp3":
                assert raw == pcm, output + " lossless roundtrip mismatch"
            results.append(dict(output=output, encoded_bytes=len(encoded), decoded_bytes=len(raw)))
        resampled = run(args.runtime, ["-f", "s16le", "-ar", "44100", "-ac", "2", "-i", "pipe:0",
                                      "-ar", "48000", "-ac", "1", "-f", "s16le", "-c:a", "pcm_s16le", "pipe:1"], pcm)
        assert abs(len(resampled) - 48000) < 100
        results.append(dict(resample="44100 stereo -> 48000 mono", bytes=len(resampled)))
    report = dict(runtime_sha256=hashlib.sha256(args.runtime.read_bytes()).hexdigest(),
                  execution_target="windows-amd64", target_feature_sets_equal=True,
                  linux_execution_verified=False, checks=results)
    (ROOT / ".run/reports").mkdir(parents=True, exist_ok=True)
    (ROOT / ".run/reports/audio-runtime-results.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
