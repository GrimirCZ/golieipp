#!/usr/bin/env python3
"""Decode PWG Raster with the host libcups raster API.

The qualification command intentionally uses the CUPS reader itself instead
of a production parser or a hand-written PackBits decoder.  The macOS SDK on
the qualification host does not ship CUPS headers, so ctypes passes the
published 1796-byte cups_page_header2_t buffer directly to libcups.
"""

from __future__ import annotations

import ctypes
import hashlib
import json
import os
import struct
import sys
from typing import Any


HEADER_SIZE = 1796
READ_MODE = 0  # CUPS_RASTER_READ


def library_candidates() -> list[str]:
    configured = os.environ.get("GOLIEIPP_CUPS_LIBRARY")
    if configured:
        return [configured]
    if sys.platform == "darwin":
        return ["/usr/lib/libcups.2.dylib", "/usr/lib/libcups.dylib", "libcups.2.dylib"]
    return ["libcups.so.2", "libcups.so"]


def load_library() -> ctypes.CDLL:
    last_error: OSError | None = None
    for path in library_candidates():
        try:
            lib = ctypes.CDLL(path)
        except OSError as error:
            last_error = error
            continue
        for name in ("cupsRasterOpen", "cupsRasterReadHeader2", "cupsRasterReadPixels", "cupsRasterClose"):
            if not hasattr(lib, name):
                raise RuntimeError(f"{path} does not export {name}")
        lib.cupsRasterOpen.argtypes = [ctypes.c_int, ctypes.c_int]
        lib.cupsRasterOpen.restype = ctypes.c_void_p
        lib.cupsRasterReadHeader2.argtypes = [ctypes.c_void_p, ctypes.c_void_p]
        lib.cupsRasterReadHeader2.restype = ctypes.c_uint
        lib.cupsRasterReadPixels.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_uint]
        lib.cupsRasterReadPixels.restype = ctypes.c_uint
        lib.cupsRasterClose.argtypes = [ctypes.c_void_p]
        lib.cupsRasterClose.restype = None
        return lib
    raise RuntimeError(f"unable to load libcups ({last_error})")


def u32(raw: bytes, offset: int) -> int:
    return struct.unpack_from("=I", raw, offset)[0]


def text(raw: bytes, offset: int) -> str:
    value = raw[offset : offset + 64]
    return value.split(b"\0", 1)[0].decode("utf-8", "replace")


def page_header(raw: bytes) -> dict[str, Any]:
    return {
        "media_class": text(raw, 0),
        "media_name": text(raw, 1732),
        "media_type": text(raw, 128),
        "width": u32(raw, 372),
        "height": u32(raw, 376),
        "bits_per_color": u32(raw, 384),
        "bits_per_pixel": u32(raw, 388),
        "bytes_per_line": u32(raw, 392),
        "color_order": u32(raw, 396),
        "color_space": u32(raw, 400),
        "num_colors": u32(raw, 420),
        "resolution_x_dpi": u32(raw, 276),
        "resolution_y_dpi": u32(raw, 280),
        "page_size_x_points": u32(raw, 352),
        "page_size_y_points": u32(raw, 356),
        "duplex": u32(raw, 272),
        "tumble": u32(raw, 368),
        "total_page_count": u32(raw, 452),
        "cross_feed_transform": u32(raw, 456),
        "feed_transform": u32(raw, 460),
        "image_box_right": u32(raw, 472),
        "image_box_bottom": u32(raw, 476),
        "print_quality": u32(raw, 484),
    }


def decode(path: str) -> dict[str, Any]:
    lib = load_library()
    fd = os.open(path, os.O_RDONLY)
    raster = lib.cupsRasterOpen(fd, READ_MODE)
    if not raster:
        os.close(fd)
        raise RuntimeError(f"cupsRasterOpen failed for {path}")

    pages: list[dict[str, Any]] = []
    all_pixels = hashlib.sha256()
    try:
        while True:
            raw_header = (ctypes.c_ubyte * HEADER_SIZE)()
            if not lib.cupsRasterReadHeader2(raster, raw_header):
                break
            raw = bytes(raw_header)
            header = page_header(raw)
            width = header["width"]
            height = header["height"]
            bytes_per_line = header["bytes_per_line"]
            if width == 0 or height == 0 or bytes_per_line == 0:
                raise RuntimeError(f"CUPS returned invalid raster dimensions in {path}")
            if bytes_per_line > 1 << 30 or height > 1 << 30:
                raise RuntimeError(f"CUPS raster dimensions are unreasonable in {path}")

            page_pixels = hashlib.sha256()
            row = (ctypes.c_ubyte * bytes_per_line)()
            for line in range(height):
                got = lib.cupsRasterReadPixels(raster, row, bytes_per_line)
                if got != bytes_per_line:
                    raise RuntimeError(
                        f"cupsRasterReadPixels returned {got} for page {len(pages) + 1}, row {line + 1}; "
                        f"wanted {bytes_per_line}"
                    )
                pixels = bytes(row)
                page_pixels.update(pixels)
                all_pixels.update(pixels)
            header["pixels_sha256"] = page_pixels.hexdigest()
            header["pixel_bytes"] = height * bytes_per_line
            pages.append(header)
    finally:
        lib.cupsRasterClose(raster)
        os.close(fd)

    return {
        "pages": pages,
        "pixel_sha256": all_pixels.hexdigest(),
        "pixel_bytes": sum(page["pixel_bytes"] for page in pages),
    }


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--probe":
        load_library()
        return 0
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} --probe | PWG_FILE", file=sys.stderr)
        return 2
    try:
        json.dump(decode(sys.argv[1]), sys.stdout, separators=(",", ":"))
        sys.stdout.write("\n")
        return 0
    except Exception as error:  # qualification should report the oracle detail
        print(error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
