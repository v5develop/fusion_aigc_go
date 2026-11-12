"""Masks + loaders for the qifenshu package samples (hand-drawn polygons, colour-filtered)."""
import os
from pathlib import Path

import cv2
import numpy as np

PKG = os.environ.get("COLORFIX_PKG_DIR", "")


def pkg_dir():
    return Path(PKG) if PKG and (Path(PKG) / "samples").is_dir() else None


def rd(path):
    return cv2.cvtColor(cv2.imread(str(path)), cv2.COLOR_BGR2RGB)


def _poly(shape, pts):
    m = np.zeros(shape, np.uint8)
    cv2.fillPoly(m, [np.array(pts, np.int32)], 255)
    return m > 0


def _clean(m):
    m = cv2.morphologyEx(m.astype(np.uint8) * 255, cv2.MORPH_OPEN, np.ones((5, 5), np.uint8))
    return cv2.morphologyEx(m, cv2.MORPH_CLOSE, np.ones((9, 9), np.uint8))


def multi_sample(d):
    """samples/multi-2garments: left = before, right = package result (pants lighter, jacket kept)."""
    im = rd(d / "samples" / "multi-2garments-2026-11-02.png")
    before, after = im[:, :717], im[:, 727:]
    hsv = cv2.cvtColor(before, cv2.COLOR_RGB2HSV)
    s, v = hsv[..., 1].astype(int), hsv[..., 2].astype(int)
    shp = before.shape[:2]
    jh = _poly(shp, [(300, 270), (430, 270), (468, 300), (484, 420), (478, 525), (300, 525), (246, 525), (244, 400), (262, 295)])
    ph = _poly(shp, [(282, 525), (440, 525), (458, 700), (476, 862), (322, 862), (305, 700)])
    return before, after, _clean(jh & (s > 45) & (v < 170)), _clean(ph & (v < 125) & (s < 80))


def demo_pair(d):
    def red(img):
        hsv = cv2.cvtColor(img, cv2.COLOR_RGB2HSV)
        h, s, v = [hsv[..., i].astype(int) for i in range(3)]
        m = (((h < 8) | (h > 170)) & (s > 120) & (v > 60)).astype(np.uint8) * 255
        return cv2.morphologyEx(m, cv2.MORPH_OPEN, np.ones((3, 3), np.uint8))
    gen, ref = rd(d / "demo" / "gen.jpg"), rd(d / "demo" / "ref.jpg")
    return gen, red(gen), ref, red(ref)
