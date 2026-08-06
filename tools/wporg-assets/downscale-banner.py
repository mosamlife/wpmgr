#!/usr/bin/env python3
"""Produce banner-772x250.png as an exact half-scale Lanczos downscale of
banner-1544x500.png.

wp.org requires the standard 772x250 banner; the 1544x500 retina file is an
add-on and is never used alone. Downscaling the retina render (rather than
re-rendering at half size) guarantees the two files are the identical
composition, which is what the directory expects.

Usage: python3 tools/wporg-assets/downscale-banner.py [assetsDir]
"""

import sys
from pathlib import Path

from PIL import Image

assets = Path(sys.argv[1] if len(sys.argv) > 1 else "release/wporg-assets").resolve()
src = assets / "banner-1544x500.png"
dst = assets / "banner-772x250.png"

im = Image.open(src)
if im.size != (1544, 500):
    raise SystemExit(f"expected {src} to be 1544x500, got {im.size}")

rgb = im.convert("RGB")

# Normalise the retina file to opaque RGB too. The renderer emits RGBA with a
# uniformly opaque alpha channel; flattening keeps the two banners byte-identical
# in colour model and shaves a few KB.
rgb.save(src, "PNG", optimize=True)
rgb.resize((772, 250), Image.LANCZOS).save(dst, "PNG", optimize=True)
print(f"wrote {src} ({src.stat().st_size} bytes, flattened to RGB)")
print(f"wrote {dst} ({dst.stat().st_size} bytes)")
