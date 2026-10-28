#!/usr/bin/env python3
"""Render the icon set from internal/web/static/icon.svg.

The SVG is the single source of truth; this script only rasterises it. Run it
after editing the SVG:

    python3 -m pip install cairosvg      # once
    python3 scripts/make-icons.py

Outputs, all under internal/web/static/:

    favicon.ico              16 + 32 + 48, multi-resolution
    apple-touch-icon.png     180 (iOS home screen)
    icon-192.png             192 (web app manifest)
    icon-512.png             512 (web app manifest)
    icon-maskable-512.png    512, extra padding for Android's circular crop

Sizes below 32px are rendered from a variant with the glow filter stripped and
the viewBox cropped: at 16px the halo costs about half a pixel and turns four
tubes into a smear, and the outer margin is wasted space.
"""

from __future__ import annotations

import pathlib
import struct
import tempfile

import cairosvg
from PIL import Image

ROOT = pathlib.Path(__file__).resolve().parent.parent
STATIC = ROOT / "internal" / "web" / "static"
SVG = STATIC / "icon.svg"

# Android reserves the outer ring of a maskable icon for its own crop.
MASKABLE_INSET = 0.82


def render(svg_text: str, size: int, out: pathlib.Path) -> None:
    cairosvg.svg2png(
        bytestring=svg_text.encode(),
        write_to=str(out),
        output_width=size,
        output_height=size,
    )


def write_ico(images: list[pathlib.Path], out: pathlib.Path) -> None:
    """Pack PNG payloads into an ICO container, one entry per size.

    Pillow's ICO writer resamples everything from the largest image, which would
    throw away the crisp 16px rendering. PNG-in-ICO is understood by every browser
    that matters, so the entries are written here explicitly.
    """
    payloads = [p.read_bytes() for p in images]
    header = struct.pack("<HHH", 0, 1, len(payloads))
    offset = len(header) + 16 * len(payloads)
    entries, blobs = b"", b""
    for path, data in zip(images, payloads):
        with Image.open(path) as im:
            side = im.width
        entries += struct.pack(
            "<BBBBHHII",
            side if side < 256 else 0,  # 0 means 256
            side if side < 256 else 0,
            0,  # palette size: not used for 32-bit
            0,  # reserved
            1,  # colour planes
            32,  # bits per pixel
            len(data),
            offset,
        )
        blobs += data
        offset += len(data)
    out.write_bytes(header + entries + blobs)


def main() -> None:
    src = SVG.read_text()
    small = src.replace(' filter="url(#tube)"', ' filter="none"').replace(
        'viewBox="0 0 64 64"', 'viewBox="1.5 1.5 61 61"'
    )

    with tempfile.TemporaryDirectory() as tmpdir:
        tmp = pathlib.Path(tmpdir)

        # Multi-resolution ICO: crisp small, glowing large.
        ico_parts = []
        for size in (16, 32, 48):
            part = tmp / f"ico-{size}.png"
            render(small if size < 32 else src, size, part)
            ico_parts.append(part)
        write_ico(ico_parts, STATIC / "favicon.ico")
        print(f"favicon.ico        {sum(p.stat().st_size for p in ico_parts)} B (16/32/48)")

        for name, size in (
            ("apple-touch-icon.png", 180),
            ("icon-192.png", 192),
            ("icon-512.png", 512),
        ):
            render(src, size, STATIC / name)
            print(f"{name:<18} {size}px")

        # Maskable: the same art inset on a full-bleed tile.
        big = tmp / "maskable-base.png"
        render(src, 512, big)
        canvas = Image.new("RGBA", (512, 512), (0, 0, 0, 0))
        with Image.open(big) as im:
            side = int(512 * MASKABLE_INSET)
            resized = im.convert("RGBA").resize((side, side), Image.Resampling.LANCZOS)
            canvas.paste(resized, ((512 - side) // 2, (512 - side) // 2))
        # Fill the corners the inset left behind with the tile colour, so the
        # icon never shows transparent shoulders under a circular mask.
        base = Image.new("RGBA", (512, 512), (10, 9, 16, 255))
        base.alpha_composite(canvas)
        base.save(STATIC / "icon-maskable-512.png")
        print("icon-maskable-512  512px")


if __name__ == "__main__":
    main()
