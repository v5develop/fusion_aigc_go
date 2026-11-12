"""numpy renderer vs. the real colorfix-math.ts (rendered by node) — max abs diff <= 1."""
import json
import os
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest

from colorfix_solver.colorfix_render import render_rgb

ROOT = Path(__file__).resolve().parent.parent
MATH_TS = os.environ.get("COLORFIX_MATH_TS", "/Users/apple/Project/Code/fusion-aigc-web/src/utils/colorfix-math.ts")


@pytest.fixture(scope="module")
def golden(tmp_path_factory):
    if shutil.which("node") is None:
        pytest.skip("node not available")
    if not Path(MATH_TS).is_file():
        pytest.skip(f"colorfix-math.ts not found: {MATH_TS}")
    out = tmp_path_factory.mktemp("golden")
    env = dict(os.environ, COLORFIX_MATH_TS=MATH_TS)
    r = subprocess.run(["node", str(ROOT / "parity" / "gen_golden.ts"), str(out)], capture_output=True, text=True, env=env, timeout=120)
    assert r.returncode == 0, r.stderr
    return out, json.loads((out / "manifest.json").read_text())


def test_numpy_matches_web_renderer(golden):
    d, manifest = golden
    worst = 0
    n = 0
    for img in manifest:
        w, h = img["w"], img["h"]
        src = np.frombuffer((d / img["input"]).read_bytes(), np.uint8).reshape(h, w, 4)
        for c in img["cases"]:
            want = np.frombuffer((d / c["expected"]).read_bytes(), np.uint8).reshape(h, w, 4)
            got = render_rgb(src, c["params"])
            diff = np.abs(got.astype(int) - want.astype(int))
            worst = max(worst, int(diff.max()))
            assert diff.max() <= 1, (img["name"], c["params"], int(diff.max()))
            assert np.array_equal(got[..., 3], want[..., 3])  # alpha untouched
            n += 1
    print(f"parity: {n} renders, max abs diff = {worst}")
    assert n >= 300


def test_alias_names_and_identity():
    rng = np.random.default_rng(1)
    img = rng.integers(0, 256, (8, 8, 3), dtype=np.uint8)
    a = render_rgb(img, {"hue": 5, "saturation": -20, "value": 7, "brightness": -30, "contrast": 12})
    b = render_rgb(img, {"hue": 5, "sat": -20, "light": 7, "bright": -30, "contrast": 12})
    assert np.array_equal(a, b)
    assert np.array_equal(render_rgb(img, {}), img)
