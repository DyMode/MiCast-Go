"""Render MiCast Go icon exports from assets/icons/micast-go.svg.

The fnOS variant uses the same 90% tile, artwork scale and stroke weights as
MiCast's fnOS export, with the mark centred inside the tile; only the
background colour differs. Sizes differ only in supersampling, and the fixed
sRGB profile keeps exports reproducible.

Requires resvg-py and Pillow: pip install resvg-py pillow
"""

from io import BytesIO
from pathlib import Path

import resvg_py
from PIL import Image

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "assets/icons/micast-go.svg"
OUT = ROOT / "assets/icons"
ICC = (OUT / "srgb.icc").read_bytes()
SVG = SOURCE.read_text(encoding="utf-8")


def fnos(stroke: float) -> str:
    svg = SVG.replace(
        'x="5" y="5" width="90" height="90" rx="22"',
        'x="0" y="0" width="100" height="100" rx="18.75"',
    )
    svg = svg.replace(
        'transform="translate(-5.0 -2.8) scale(1.08)"',
        'transform="translate(50 50) scale(1.2371) translate(-50 -50) translate(-5.0 -2.8) scale(1.08)"',
    )
    return svg.replace('stroke-width="2.6"', f'stroke-width="{stroke}"')


def render(size: int, source: str) -> Image.Image:
    factor = 8 if size <= 64 else 4
    image = Image.open(
        BytesIO(resvg_py.svg_to_bytes(svg_string=source, width=size * factor, height=size * factor))
    ).convert("RGBA")
    return image.resize((size, size), Image.Resampling.BOX)


for size in (32, 48, 64, 96, 128, 192, 256, 512):
    stroke = 3.25 if size <= 64 else 3.05
    render(size, fnos(stroke)).save(OUT / f"micast-go-{size}.png", icc_profile=ICC, optimize=True)

(ROOT / "internal/server/web/icon.svg").write_bytes(SVG.encode("utf-8"))
