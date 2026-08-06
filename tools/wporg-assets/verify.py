#!/usr/bin/env python3
"""Verify the staged wordpress.org listing assets against the official spec.

Spec source: https://developer.wordpress.org/plugins/wordpress-org/plugin-assets/
  icon-128x128.(png|jpg|gif)    128x128, max 1MB
  icon-256x256.(png|jpg|gif)    256x256, max 1MB   (retina, add-on only)
  icon.svg                      optional, PNG fallback still required
  banner-772x250.(jpg|png)      772x250, max 4MB   (REQUIRED if you ship a banner)
  banner-1544x500.(jpg|png)     1544x500, max 4MB  (retina, add-on only)
  screenshot-N.(png|jpg)        max 10MB each, lowercase filenames only

Usage: python3 tools/wporg-assets/verify.py [assetsDir]
"""

import re
import sys
from pathlib import Path

from PIL import Image

assets = Path(sys.argv[1] if len(sys.argv) > 1 else "release/wporg-assets").resolve()

REQUIRED = {
    "icon-128x128.png": (128, 128, 1 << 20),
    "icon-256x256.png": (256, 256, 1 << 20),
    "banner-772x250.png": (772, 250, 4 << 20),
    "banner-1544x500.png": (1544, 500, 4 << 20),
}

failures = []
print(f"assets dir: {assets}\n")

for name, (w, h, maxbytes) in REQUIRED.items():
    p = assets / name
    if not p.exists():
        failures.append(f"MISSING {name}")
        continue
    im = Image.open(p)
    size = p.stat().st_size
    opaque = True
    if im.mode in ("RGBA", "LA"):
        lo, _ = im.getchannel("A").getextrema()
        opaque = lo == 255
    ok = im.size == (w, h) and size <= maxbytes and opaque
    print(f"{'OK  ' if ok else 'FAIL'} {name:22s} {im.size[0]}x{im.size[1]} {im.mode} {size:>9,} bytes opaque={opaque}")
    if im.size != (w, h):
        failures.append(f"{name}: expected {w}x{h}, got {im.size[0]}x{im.size[1]}")
    if size > maxbytes:
        failures.append(f"{name}: {size} bytes exceeds {maxbytes}")
    if not opaque:
        failures.append(f"{name}: has transparent pixels; wp.org composites on white or a dark card")

shots = sorted(assets.glob("screenshot-*"))
if not shots:
    print("\nNOTE screenshot-1.png ... screenshot-N.png are NOT present. A human must capture them.")
for p in shots:
    if not re.fullmatch(r"screenshot-\d+(-[a-z_]+)?\.(png|jpg|gif)", p.name):
        failures.append(f"{p.name}: not a valid wp.org screenshot filename (lowercase, .png or .jpg)")
        continue
    im = Image.open(p)
    size = p.stat().st_size
    ok = size <= (10 << 20)
    print(f"{'OK  ' if ok else 'FAIL'} {p.name:22s} {im.size[0]}x{im.size[1]} {im.mode} {size:>9,} bytes")
    if not ok:
        failures.append(f"{p.name}: {size} bytes exceeds 10MB")

IMAGE_SUFFIXES = (".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".avif")
for p in sorted(assets.iterdir()):
    if p.suffix.lower() not in IMAGE_SUFFIXES:
        continue  # README.md and friends are ignored by wp.org and by this check
    if p.name != p.name.lower():
        failures.append(f"{p.name}: uppercase in filename; wp.org silently ignores it")
    if p.suffix.lower() in (".jpeg", ".webp", ".avif"):
        failures.append(f"{p.name}: unsupported extension; only .png, .jpg, .gif")

if failures:
    print("\nFAILURES:")
    for f in failures:
        print(" -", f)
    sys.exit(1)
print("\nAll staged assets conform to the wordpress.org spec.")
