"""numpy port of the web per-pixel colour-fix renderer.

Source of truth: fusion-aigc-web/src/utils/colorfix-math.ts  applyColorFixPixels().
Five sliders, applied in the fixed order that mirrors the CSS filter chain:

    hue-rotate -> saturate -> brightness -> contrast

Every step clamps to 0..255; the two RGB matrix stages stay in float (no rounding),
the saturate output is rounded with Math.round (half up) to index a 256-entry LUT
that folds brightness + contrast; LUT entries are stored through a Uint8ClampedArray,
i.e. round-half-to-even. Both rounding rules are reproduced here so the output matches
the browser preview / exact export (parity-tested against the TS file, <= 1 level).

Parameter names: the API uses  hue / saturation / value / brightness / contrast.
The web code calls the middle three  sat / light / bright; both spellings are accepted.
"""
from __future__ import annotations

import math
from typing import Mapping

import numpy as np

# API name -> web name
WEB_NAME = {"hue": "hue", "saturation": "sat", "value": "light", "brightness": "bright", "contrast": "contrast"}
KEYS = tuple(WEB_NAME)  # API order
RANGES = {
    "hue": (-30.0, 30.0),
    "saturation": (-60.0, 70.0),
    "value": (-40.0, 40.0),
    "brightness": (-80.0, 80.0),
    "contrast": (-50.0, 50.0),
}
ZERO = {k: 0 for k in KEYS}


def normalize_params(p: Mapping[str, float]) -> dict[str, float]:
    """Accept API names or web names; missing -> 0. Returns API-named floats."""
    out = {}
    for api, web in WEB_NAME.items():
        v = p.get(api, p.get(web, 0))
        out[api] = float(v)
    return out


def clamp_params(p: Mapping[str, float]) -> dict[str, float]:
    q = normalize_params(p)
    return {k: min(RANGES[k][1], max(RANGES[k][0], q[k])) for k in KEYS}


def to_web(p: Mapping[str, float]) -> dict[str, float]:
    q = normalize_params(p)
    return {WEB_NAME[k]: q[k] for k in KEYS}


def _js_round(x: np.ndarray) -> np.ndarray:
    """Math.round: ties toward +inf."""
    return np.floor(x + 0.5)


def build_lut(p: Mapping[str, float]) -> np.ndarray:
    """256-entry uint8 LUT for brightness -> contrast (value & brightness multiply)."""
    q = normalize_params(p)
    bright = max(0.05, (1 + q["value"] / 200) * (1 + q["brightness"] / 100))
    contrast = max(0.0, 100 + q["contrast"]) / 100
    v = np.arange(256, dtype=np.float64)
    t = np.clip(v * bright, 0, 255)
    t = np.clip((t - 127.5) * contrast + 127.5, 0, 255)
    return np.rint(t).astype(np.uint8)  # Uint8ClampedArray: ties to even


def hue_matrix(hue_deg: float) -> np.ndarray:
    rad = (hue_deg * math.pi) / 180
    c = math.cos(rad)
    s = math.sin(rad)
    return np.array([
        [0.213 + c * 0.787 - s * 0.213, 0.715 - c * 0.715 - s * 0.715, 0.072 - c * 0.072 + s * 0.928],
        [0.213 - c * 0.213 + s * 0.143, 0.715 + c * 0.285 + s * 0.14, 0.072 - c * 0.072 - s * 0.283],
        [0.213 - c * 0.213 - s * 0.787, 0.715 - c * 0.715 + s * 0.715, 0.072 + c * 0.928 + s * 0.072],
    ])


def sat_matrix(sat: float) -> np.ndarray:
    k = max(0.0, 100 + sat) / 100
    return np.array([
        [0.213 + 0.787 * k, 0.715 - 0.715 * k, 0.072 - 0.072 * k],
        [0.213 - 0.213 * k, 0.715 + 0.285 * k, 0.072 - 0.072 * k],
        [0.213 - 0.213 * k, 0.715 - 0.715 * k, 0.072 + 0.928 * k],
    ])


def render_rgb(rgb: np.ndarray, params: Mapping[str, float], *, quantize: bool = True) -> np.ndarray:
    """Render N-d uint8 RGB (last axis 3, or 4 with alpha kept) -> same shape uint8.

    quantize=False skips the integer roundings and returns float64 0..255 (smooth; used
    by the solver's objective). quantize=True is the exact browser-equivalent output.
    """
    q = normalize_params(params)
    a = np.asarray(rgb)
    has_alpha = a.shape[-1] == 4
    px = a[..., :3].astype(np.float64)
    r0, g0, b0 = px[..., 0], px[..., 1], px[..., 2]
    h = hue_matrix(q["hue"])
    t = sat_matrix(q["saturation"])
    # same left-to-right sums as the JS (float64 both sides)
    r1 = np.clip(h[0, 0] * r0 + h[0, 1] * g0 + h[0, 2] * b0, 0, 255)
    g1 = np.clip(h[1, 0] * r0 + h[1, 1] * g0 + h[1, 2] * b0, 0, 255)
    b1 = np.clip(h[2, 0] * r0 + h[2, 1] * g0 + h[2, 2] * b0, 0, 255)
    r2 = np.clip(t[0, 0] * r1 + t[0, 1] * g1 + t[0, 2] * b1, 0, 255)
    g2 = np.clip(t[1, 0] * r1 + t[1, 1] * g1 + t[1, 2] * b1, 0, 255)
    b2 = np.clip(t[2, 0] * r1 + t[2, 1] * g1 + t[2, 2] * b1, 0, 255)
    if quantize:
        lut = build_lut(q)
        out = np.stack([lut[_js_round(c).astype(np.intp)] for c in (r2, g2, b2)], axis=-1)
        if has_alpha:
            out = np.concatenate([out, a[..., 3:4].astype(np.uint8)], axis=-1)
        return out
    bright = max(0.05, (1 + q["value"] / 200) * (1 + q["brightness"] / 100))
    contrast = max(0.0, 100 + q["contrast"]) / 100
    out = np.stack([r2, g2, b2], axis=-1)
    out = np.clip((np.clip(out * bright, 0, 255) - 127.5) * contrast + 127.5, 0, 255)
    return out
