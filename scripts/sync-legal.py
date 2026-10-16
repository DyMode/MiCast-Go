"""Keep fnOS installation disclosures identical to the embedded application text."""
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
steps = []
for name in ("user-agreement", "privacy-policy"):
    blocks = (ROOT / f"internal/server/web/{name}.txt").read_text(encoding="utf-8").strip().split("\n\n")
    heading, date = blocks[0].split("\n", 1)
    steps.append({"stepTitle": heading, "items": [{"type": "tips", "helpText": date}] +
                  [{"type": "tips", "helpText": block} for block in blocks[1:]]})
steps.append({"stepTitle": "阅读并同意后安装", "items": [{"type": "checkbox", "field": "wizard_legal_accept",
    "label": "用户协议与隐私政策", "initValue": "", "options": [{"label": "我已阅读并同意《MiCast Go 用户协议》和《MiCast Go 隐私政策》", "value": "2026-10-16"}],
    "rules": [{"required": True, "message": "请阅读并同意用户协议与隐私政策后安装"}]}]})
target = ROOT / "packaging/fnos/wizard/install"
content = json.dumps(steps, ensure_ascii=False, indent=2) + "\n"
if "--check" in sys.argv:
    if target.read_text(encoding="utf-8") != content:
        raise SystemExit("安装协议与应用正文不一致，请运行 python scripts/sync-legal.py")
else:
    target.write_text(content, encoding="utf-8", newline="\n")
