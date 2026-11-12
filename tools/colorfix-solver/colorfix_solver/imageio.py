"""Decode caller images / masks with strict size limits."""
from __future__ import annotations

import base64
import binascii
import io

import numpy as np
from PIL import Image, ImageOps


# Pillow's own bomb guard would fire (and be racy to reconfigure per call) before our header check below.
Image.MAX_IMAGE_PIXELS = None


class ImageError(Exception):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


def decode_b64(s: str, max_bytes: int) -> bytes:
    if not isinstance(s, str):
        raise ImageError("bad_b64", "base64 field must be a string")
    if s.startswith("data:"):
        _, _, s = s.partition(",")
    if len(s) > max_bytes * 4 // 3 + 8:
        raise ImageError("too_large", "image is too large")
    try:
        raw = base64.b64decode(s, validate=True)
    except (binascii.Error, ValueError) as e:
        raise ImageError("bad_b64", "invalid base64") from e
    if len(raw) > max_bytes:
        raise ImageError("too_large", "image is too large")
    return raw


def _open(raw: bytes, max_pixels: int) -> Image.Image:
    try:
        im = Image.open(io.BytesIO(raw))
        if im.format not in ("PNG", "JPEG", "WEBP"):
            raise ImageError("bad_image", "only PNG / JPEG / WebP are accepted")
        w, h = im.size
        if w * h > max_pixels:  # header check, before any pixel is decoded
            raise ImageError("too_many_pixels", f"image has {w * h} px (max {max_pixels})")
        if w < 8 or h < 8:
            raise ImageError("bad_image", "image is too small")
        im.load()
        return im
    except ImageError:
        raise
    except (OSError, ValueError, Image.DecompressionBombError, SyntaxError) as e:
        raise ImageError("bad_image", "cannot decode image") from e


def decode_rgb(raw: bytes, max_pixels: int) -> np.ndarray:
    """-> uint8 HxWx3 RGB (transparent pixels composited onto white, EXIF orientation applied)."""
    im = ImageOps.exif_transpose(_open(raw, max_pixels))
    if im.mode in ("RGBA", "LA", "P"):
        im = im.convert("RGBA")
        bg = Image.new("RGBA", im.size, (255, 255, 255, 255))
        im = Image.alpha_composite(bg, im)
    return np.asarray(im.convert("RGB"))


def decode_mask(raw: bytes, max_pixels: int) -> np.ndarray:
    """-> uint8 HxW alpha. RGBA/LA with real transparency uses alpha, otherwise luminance."""
    im = _open(raw, max_pixels)
    if im.mode in ("RGBA", "LA") or (im.mode == "P" and "transparency" in im.info):
        a = np.asarray(im.convert("RGBA"))[..., 3]
        if a.min() < 255:
            return a
    return np.asarray(im.convert("L"))
