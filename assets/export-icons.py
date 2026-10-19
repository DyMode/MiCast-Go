"""Render MiCast Go icon exports from assets/icons/micast-go.svg.

The fnOS variant uses a Xiaomi-style n=3 superellipse whose side tangents touch
the canvas edges, with the mark centred inside it; only the background colour
differs from MiCast. Sizes differ only in supersampling and stroke weight, and
the fixed sRGB profile keeps exports reproducible.

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
    return SVG.replace('stroke-width="2.6"', f'stroke-width="{stroke}"')


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
