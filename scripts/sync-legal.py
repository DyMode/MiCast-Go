"""Keep the fnOS installation agreement identical to the embedded application text."""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sections = [
    (ROOT / f"internal/server/web/{name}.txt").read_text(encoding="utf-8").strip()
    for name in ("user-agreement", "privacy-policy")
]
target = ROOT / "packaging/fnos/LICENSE"
content = "\n\n".join(sections) + "\n"
if "--check" in sys.argv:
    if target.read_text(encoding="utf-8") != content:
        raise SystemExit("安装许可协议与应用正文不一致，请运行 python scripts/sync-legal.py")
else:
    target.write_text(content, encoding="utf-8", newline="\n")
