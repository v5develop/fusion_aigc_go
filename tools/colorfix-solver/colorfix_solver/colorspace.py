"""Small Lab / CIEDE2000 helpers (numpy + cv2; no scikit-image)."""
from __future__ import annotations

import cv2
import numpy as np


def rgb_to_lab(rgb) -> np.ndarray:
    """sRGB (uint8 0..255 or float 0..255) with last axis 3 -> Lab (D65, L 0..100)."""
    a = np.asarray(rgb)
    shp = a.shape
    x = (a.reshape(-1, 1, 3).astype(np.float32) / 255.0).clip(0, 1)
    lab = cv2.cvtColor(x, cv2.COLOR_RGB2LAB)
    return lab.reshape(shp).astype(np.float64)


def lab_to_rgb(lab) -> np.ndarray:
    a = np.asarray(lab, np.float32)
    shp = a.shape
    rgb = cv2.cvtColor(a.reshape(-1, 1, 3), cv2.COLOR_LAB2RGB)
    return (rgb.reshape(shp).clip(0, 1) * 255.0).astype(np.float64)


def ciede2000(lab1, lab2) -> np.ndarray:
    """CIEDE2000 (Sharma et al.), broadcasting over leading axes."""
    l1, a1, b1 = np.moveaxis(np.asarray(lab1, np.float64), -1, 0)
    l2, a2, b2 = np.moveaxis(np.asarray(lab2, np.float64), -1, 0)
    c1 = np.hypot(a1, b1)
    c2 = np.hypot(a2, b2)
    cm = (c1 + c2) / 2
    g = 0.5 * (1 - np.sqrt(cm**7 / (cm**7 + 25.0**7)))
    a1p = (1 + g) * a1
    a2p = (1 + g) * a2
    c1p = np.hypot(a1p, b1)
    c2p = np.hypot(a2p, b2)
    h1p = np.degrees(np.arctan2(b1, a1p)) % 360
    h2p = np.degrees(np.arctan2(b2, a2p)) % 360
    dl = l2 - l1
    dc = c2p - c1p
    dh = h2p - h1p
    dh = np.where(dh > 180, dh - 360, np.where(dh < -180, dh + 360, dh))
    dh = np.where(c1p * c2p == 0, 0.0, dh)
    dH = 2 * np.sqrt(c1p * c2p) * np.sin(np.radians(dh) / 2)
    lp = (l1 + l2) / 2
    cp = (c1p + c2p) / 2
    hsum = h1p + h2p
    hp = np.where(np.abs(h1p - h2p) <= 180, hsum / 2, np.where(hsum < 360, (hsum + 360) / 2, (hsum - 360) / 2))
    hp = np.where(c1p * c2p == 0, hsum, hp)
    t = (1 - 0.17 * np.cos(np.radians(hp - 30)) + 0.24 * np.cos(np.radians(2 * hp))
         + 0.32 * np.cos(np.radians(3 * hp + 6)) - 0.20 * np.cos(np.radians(4 * hp - 63)))
    sl = 1 + 0.015 * (lp - 50) ** 2 / np.sqrt(20 + (lp - 50) ** 2)
    sc = 1 + 0.045 * cp
    sh = 1 + 0.015 * cp * t
    dtheta = 30 * np.exp(-(((hp - 275) / 25) ** 2))
    rc = 2 * np.sqrt(cp**7 / (cp**7 + 25.0**7))
    rt = -np.sin(np.radians(2 * dtheta)) * rc
    return np.sqrt((dl / sl) ** 2 + (dc / sc) ** 2 + (dH / sh) ** 2 + rt * (dc / sc) * (dH / sh))
