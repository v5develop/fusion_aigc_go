"""Reference-driven solver for the five colour-fix sliders under OUR render formula.

Ideas reused from the qifenshu package (colorfix_core.py): reference "truth" colour taken
from the lit part of the reference garment (removes hard shadows), Lab statistics inside
the mask using the diffuse L band (no highlights / shadows), scene ratio that only partly
pulls the target lightness to the reference, bounded Nelder-Mead.  What differs: every
candidate is rendered with colorfix_render (the web five-slider formula), not ps_adjust HSL,
so solved parameters look the same in the browser preview and the exact export.

Resolution independent: everything runs on a copy whose long side is <= max_side.
"""
from __future__ import annotations

import time
from dataclasses import dataclass, field

import cv2
import numpy as np
from scipy.optimize import minimize

from . import colorfix_render as R
from .colorspace import ciede2000, rgb_to_lab

STRENGTH = 0.5          # package default: 0 = match reference L fully, 1 = keep generated L
RATIO_LO, RATIO_HI = 0.70, 1.30
MIN_AREA = 400          # px at solve scale
REG = 0.35              # package value: penalty on (param / range)^2
REG_W = np.array([1.0, 1.0, 1.0, 1.0, 3.0])   # contrast is the blunt tool: 3x stiffer
DEADBAND = 0.3          # dE2000 below this is invisible; inside it the smallest params win
TEX_W, TEX_FREE = 3.0, 0.15   # keep the fabric's L spread within +-15% for free, then penalise
MAX_SAMPLES = 6000


class SolveError(ValueError):
    """Input the solver cannot work with (empty mask, ...). Maps to HTTP 422."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


class SolveTimeout(Exception):
    pass


# ---------------------------------------------------------------- image helpers

def _alpha01(m: np.ndarray) -> np.ndarray:
    a = np.asarray(m)
    if a.ndim == 3:
        a = a[..., 0]
    if a.dtype == np.uint8:
        return a.astype(np.float32) / 255.0
    return np.clip(a.astype(np.float32), 0, 1)


def downscale(rgb: np.ndarray, alpha: np.ndarray | None, max_side: int):
    h, w = rgb.shape[:2]
    sc = min(1.0, max_side / max(h, w))
    if sc >= 1.0:
        return rgb, alpha
    size = (max(8, round(w * sc)), max(8, round(h * sc)))
    rgb2 = cv2.resize(rgb, size, interpolation=cv2.INTER_AREA)
    a2 = None if alpha is None else cv2.resize(alpha, size, interpolation=cv2.INTER_AREA)
    return rgb2, a2


def core_mask(alpha: np.ndarray, thr: float = 0.5) -> np.ndarray:
    """Binary mask, eroded a little so edge pixels (halo / background bleed) do not skew the stats."""
    m = (alpha >= thr).astype(np.uint8)
    area = int(m.sum())
    r = max(1, round(min(m.shape) * 0.004))
    e = cv2.erode(m, np.ones((2 * r + 1, 2 * r + 1), np.uint8))
    return (e if int(e.sum()) >= 0.5 * area and int(e.sum()) >= MIN_AREA else m).astype(bool)


def bg_diff_mask(rgb: np.ndarray) -> np.ndarray:
    """Flat-lay reference without a mask: pixels that differ from the border colour, biggest blob."""
    h, w = rgb.shape[:2]
    lab = rgb_to_lab(rgb)
    b = max(2, round(min(h, w) * 0.03))
    border = np.zeros((h, w), bool)
    border[:b, :] = border[-b:, :] = border[:, :b] = border[:, -b:] = True
    bg = np.median(lab[border], axis=0)
    d = np.linalg.norm(lab - bg, axis=-1)
    thr = max(6.0, 2.0 * float(np.percentile(d[border], 95)))
    m = (d > thr).astype(np.uint8)
    m = cv2.morphologyEx(m, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    m = cv2.morphologyEx(m, cv2.MORPH_CLOSE, np.ones((5, 5), np.uint8))
    n, lbl, stats, _ = cv2.connectedComponentsWithStats(m, connectivity=8)
    if n <= 1:
        return np.zeros((h, w), bool)
    big = 1 + int(np.argmax(stats[1:, cv2.CC_STAT_AREA]))
    m = (lbl == big).astype(np.uint8)
    # fill holes: flood the background from a border pixel
    inv = (1 - m).astype(np.uint8)
    ff = np.zeros((h + 2, w + 2), np.uint8)
    for seed in ((0, 0), (w - 1, 0), (0, h - 1), (w - 1, h - 1)):
        if inv[seed[1], seed[0]]:
            cv2.floodFill(inv, ff, seed, 2)
    return (inv != 2)


# ---------------------------------------------------------------- statistics

def diffuse_select(lab: np.ndarray, lo: float = 15, hi: float = 85) -> np.ndarray:
    """Boolean over rows of an (N,3) Lab array: the diffuse L band (drops highlights / shadows)."""
    L = lab[:, 0]
    a, b = np.percentile(L, [lo, hi])
    return (L >= a) & (L <= b)


def diffuse_median(lab: np.ndarray) -> np.ndarray:
    if len(lab) < 50:
        return np.array([50.0, 0.0, 0.0])
    return np.median(lab[diffuse_select(lab)], axis=0)


def reference_truth(ref_lab_px: np.ndarray, spread: float = 30.0, pct: float = 75.0) -> np.ndarray:
    """Reference colour from the (N,3) Lab pixels of the reference garment.

    Even lighting -> diffuse median.  Strong shadow (L P90-P10 > spread) -> median of the lit top
    quarter of L, so a shadow does not drag the colour darker (package behaviour).
    """
    if len(ref_lab_px) < 50:
        return diffuse_median(ref_lab_px)
    L = ref_lab_px[:, 0]
    if float(np.percentile(L, 90) - np.percentile(L, 10)) <= spread:
        return diffuse_median(ref_lab_px)
    sel = L >= float(np.percentile(L, pct))
    if int(sel.sum()) < 50:
        return diffuse_median(ref_lab_px)
    return np.median(ref_lab_px[sel], axis=0)


def scene_target(truth: np.ndarray, gen_med: np.ndarray, strength: float = STRENGTH):
    ratio = float(np.clip(gen_med[0] / max(truth[0], 1e-3), RATIO_LO, RATIO_HI))
    tgt = np.array([truth[0] * (1.0 + strength * (ratio - 1.0)), truth[1], truth[2]])
    return tgt, ratio


# ---------------------------------------------------------------- search

_LO = np.array([R.RANGES[k][0] for k in R.KEYS])
_HI = np.array([R.RANGES[k][1] for k in R.KEYS])
_SPAN = _HI - _LO


def _pdict(v) -> dict[str, float]:
    return {k: float(x) for k, x in zip(R.KEYS, v)}


def _search(samples: np.ndarray, target: np.ndarray, deadline: float | None, *, reg: float = REG):
    """samples: (N,3) uint8 diffuse garment pixels. Returns (int params vector, evals, cost)."""
    px = samples.reshape(-1, 1, 3)
    evals = [0]
    s0 = float(np.subtract(*np.percentile(rgb_to_lab(samples)[:, 0], [85, 15])))

    def objective(v, quantize: bool, data) -> float:
        out = R.render_rgb(data, _pdict(v), quantize=quantize)
        lab = rgb_to_lab(out.reshape(-1, 3))
        de = float(ciede2000(np.median(lab, axis=0), target))
        s1 = float(np.subtract(*np.percentile(lab[:, 0], [85, 15])))
        tex = max(0.0, abs(np.log((s1 + 1.0) / (s0 + 1.0))) - TEX_FREE)
        return (max(0.0, de - DEADBAND) + TEX_W * tex
                + reg * float(np.sum(REG_W * (np.asarray(v) / _SPAN) ** 2)))

    def cost_of(v, quantize: bool) -> float:
        if deadline is not None and time.monotonic() > deadline:
            raise SolveTimeout()
        evals[0] += 1
        return objective(v, quantize, px)

    # coarse grid (hue x brightness x saturation) on a thinned sample
    thin = px[:: max(1, len(px) // 1500)]

    def grid_cost(v):
        return objective(v, False, thin)

    cands = []
    for h in np.linspace(-30, 30, 13):
        for b in np.linspace(-72, 72, 13):
            for s in (-45, -20, 0, 25, 55):
                if deadline is not None and time.monotonic() > deadline:
                    raise SolveTimeout()
                v = np.array([h, s, 0.0, b, 0.0])
                cands.append((grid_cost(v), v))
    cands.sort(key=lambda t: t[0])
    starts, seen = [np.zeros(5)], []
    for c, v in cands:
        if all(np.linalg.norm((v - u) / _SPAN) > 0.12 for u in seen):
            seen.append(v)
            starts.append(v)
        if len(seen) >= 3:
            break

    steps = np.array([3.0, 6.0, 4.0, 6.0, 4.0])
    best_v, best_c = None, np.inf
    for x0 in starts:
        simplex = [x0] + [x0 + np.eye(5)[i] * steps[i] * (1 if x0[i] + steps[i] <= _HI[i] else -1) for i in range(5)]
        res = minimize(lambda v: cost_of(np.clip(v, _LO, _HI), False), x0, method="Nelder-Mead",
                       bounds=list(zip(_LO, _HI)),
                       options=dict(maxiter=260, xatol=0.05, fatol=0.01, initial_simplex=np.array(simplex)))
        v = np.clip(res.x, _LO, _HI)
        if res.fun < best_c:
            best_v, best_c = v, float(res.fun)

    # integer polish against the exact (quantized) renderer: the UI sliders are integers
    v = np.clip(np.rint(best_v), _LO, _HI)
    c = cost_of(v, True)
    for _ in range(3):
        improved = False
        for i in range(5):
            for d in (-1, 1):
                w = v.copy()
                w[i] = np.clip(w[i] + d, _LO[i], _HI[i])
                if w[i] == v[i]:
                    continue
                cw = cost_of(w, True)
                if cw < c - 1e-9:
                    v, c, improved = w, cw, True
        if not improved:
            break
    return v, evals[0], c


# ---------------------------------------------------------------- public API

def solve(gen_rgb: np.ndarray, ref_rgb: np.ndarray, mask_alpha: np.ndarray,
          ref_mask_alpha: np.ndarray | None = None, *, strength: float = STRENGTH,
          max_side: int = 1024, timeout: float | None = None) -> dict:
    """Solve the five sliders that move the garment under `mask_alpha` in the generated image
    to the colour of the garment in the reference.

    gen_rgb / ref_rgb: uint8 HxWx3 RGB.  mask_alpha / ref_mask_alpha: uint8 0..255 or float 0..1,
    same size as their image (ref mask optional: background-difference fallback).
    Returns {"params": {hue,saturation,value,brightness,contrast} (ints),
             "score": 0..100 (higher better = 100 - 10*dE_target), "diagnostics": {...}}.
    """
    t0 = time.monotonic()
    deadline = None if timeout is None else t0 + timeout
    gen_rgb = np.asarray(gen_rgb)
    ref_rgb = np.asarray(ref_rgb)
    if gen_rgb.shape[:2] != np.shape(mask_alpha)[:2]:
        raise SolveError("mask_size", "mask size differs from the generated image")
    g, ga = downscale(gen_rgb, _alpha01(mask_alpha), max_side)
    m = core_mask(ga)
    if int(m.sum()) < MIN_AREA:
        raise SolveError("mask_empty", f"mask covers {int(m.sum())} px at solve scale (< {MIN_AREA})")

    if ref_mask_alpha is not None and ref_mask_alpha.shape[:2] != ref_rgb.shape[:2]:
        raise SolveError("mask_size", "reference mask size differs from the reference image")
    r, ra = downscale(ref_rgb, None if ref_mask_alpha is None else _alpha01(ref_mask_alpha), max_side)
    if ra is not None:
        rm, ref_src = core_mask(ra), "given"
    else:
        rm, ref_src = bg_diff_mask(r), "bg-diff"
        e = cv2.erode(rm.astype(np.uint8), np.ones((3, 3), np.uint8)).astype(bool)
        rm = e if int(e.sum()) >= MIN_AREA else rm
    if int(rm.sum()) < MIN_AREA:
        raise SolveError("ref_mask_empty", f"reference garment covers {int(rm.sum())} px (< {MIN_AREA})")

    ref_lab = rgb_to_lab(r[rm])
    truth = reference_truth(ref_lab)
    gen_px = g[m]
    gen_lab = rgb_to_lab(gen_px)
    gen_med = diffuse_median(gen_lab)
    target, ratio = scene_target(truth, gen_med, strength)

    # fixed diffuse subset of the generated garment (selected on the ORIGINAL L), thinned
    sel = gen_px[diffuse_select(gen_lab)]
    if len(sel) > MAX_SAMPLES:
        sel = sel[np.linspace(0, len(sel) - 1, MAX_SAMPLES).astype(int)]
    v, evals, _ = _search(sel, target, deadline)
    params = {k: int(x) for k, x in zip(R.KEYS, v)}

    out_med = diffuse_median(rgb_to_lab(R.render_rgb(gen_px, params)))
    de_target = float(ciede2000(out_med, target))
    de_flat = float(ciede2000(out_med, truth))
    hits = [k for k, x, lo, hi in zip(R.KEYS, v, _LO, _HI) if x <= lo or x >= hi]
    return {
        "params": params,
        "score": round(max(0.0, 100.0 - 10.0 * de_target), 2),
        "diagnostics": {
            "dE_target": round(de_target, 3),
            "dE_reference": round(de_flat, 3),
            "dE_before": round(float(ciede2000(gen_med, target)), 3),
            "reference_lab": [round(float(x), 2) for x in truth],
            "target_lab": [round(float(x), 2) for x in target],
            "garment_lab_before": [round(float(x), 2) for x in gen_med],
            "garment_lab_after": [round(float(x), 2) for x in out_med],
            "scene_ratio": round(ratio, 4),
            "strength": strength,
            "mask_px": int(m.sum()),
            "reference_mask": ref_src,
            "at_bounds": hits,
            "evals": evals,
            "seconds": round(time.monotonic() - t0, 3),
        },
    }


def exclusive_masks(alphas: list[np.ndarray], thr: float = 0.5) -> list[np.ndarray]:
    """Earlier pieces win overlapping pixels. Inputs float 0..1 (same size) -> bool masks."""
    claimed = np.zeros(alphas[0].shape[:2], bool)
    out = []
    for a in alphas:
        m = a >= thr
        out.append(m & ~claimed)
        claimed |= m
    return out


def solve_pieces(gen_rgb: np.ndarray, pieces: list[dict], *, strength: float = STRENGTH, max_side: int = 1024,
                 timeout: float | None = None) -> list[dict]:
    """pieces: [{key, ref_rgb, mask (gen-sized alpha), ref_mask (optional)}].

    Each piece is solved independently on its own (overlap-exclusive) mask; params of one piece
    never depend on another piece's reference or parameters.  Returns [{key, params, score, diagnostics}].
    """
    t0 = time.monotonic()
    alphas = [_alpha01(p["mask"]) for p in pieces]
    for a in alphas:
        if a.shape[:2] != gen_rgb.shape[:2]:
            raise SolveError("mask_size", "mask size differs from the generated image")
    ex = exclusive_masks(alphas)
    res = []
    for p, e in zip(pieces, ex):
        left = None if timeout is None else max(0.1, timeout - (time.monotonic() - t0))
        a = np.where(e, alphas[len(res)], 0).astype(np.float32)
        try:
            r = solve(gen_rgb, p["ref_rgb"], a, p.get("ref_mask"), strength=strength, max_side=max_side, timeout=left)
        except SolveError as ex_:
            raise SolveError(ex_.code, f"piece '{p['key']}': {ex_}") from ex_
        res.append({"key": p["key"], **r})
    return res
