"""Synthetic garment scenes for solver tests."""
import numpy as np

from colorfix_solver.colorfix_render import render_rgb


def garment_scene(h, w, rects, base_colors, bg=(205, 205, 200), seed=0, noise=6.0):
    """Image with shaded rectangular 'garments' (+ their 0/255 masks) on a flat background."""
    rng = np.random.default_rng(seed)
    img = np.empty((h, w, 3), np.float64)
    img[:] = bg
    yy, xx = np.mgrid[0:h, 0:w]
    masks = []
    for (x0, y0, x1, y1), col in zip(rects, base_colors):
        shade = 0.85 + 0.15 * np.sin(xx / 17.0) * np.cos(yy / 23.0)
        patch = np.array(col, float)[None, None, :] * shade[..., None] + rng.normal(0, noise, (h, w, 3))
        m = np.zeros((h, w), np.uint8)
        m[y0:y1, x0:x1] = 255
        img[m > 0] = patch[m > 0]
        masks.append(m)
    return np.clip(img, 0, 255).astype(np.uint8), masks


def make_pair(known, base=(150, 70, 80), seed=1):
    """gen = garment as generated; ref = flat-lay of the garment rendered with `known` params."""
    gen, (gm,) = garment_scene(300, 220, [(60, 50, 170, 250)], [base], seed=seed)
    flat, (rm,) = garment_scene(260, 260, [(50, 40, 210, 220)], [base], bg=(228, 228, 228), seed=seed + 100)
    ref = render_rgb(flat, known)
    return gen, gm, ref, rm
