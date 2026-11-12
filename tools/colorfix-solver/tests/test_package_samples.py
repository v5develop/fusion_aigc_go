"""Acceptance on the qifenshu package's own samples (skipped unless COLORFIX_PKG_DIR points at the unzipped package).

The package ships no masks (it uses SAM3), so garment masks are hand-drawn polygons + colour filters (tests/pkgsamples.py).
"""
import numpy as np
import pytest

from colorfix_solver import colorfix_render as R
from colorfix_solver import solver as S
from colorfix_solver.colorspace import ciede2000, rgb_to_lab

from .pkgsamples import demo_pair, multi_sample, pkg_dir

pytestmark = pytest.mark.skipif(pkg_dir() is None, reason="COLORFIX_PKG_DIR not set")


def med(img, m):
    return S.diffuse_median(rgb_to_lab(img[m > 0]))


@pytest.mark.parametrize("give_ref_mask", [True, False])
def test_demo_red_dress(give_ref_mask):
    gen, gm, ref, rm = demo_pair(pkg_dir())
    res = S.solve(gen, ref, gm, rm if give_ref_mask else None)
    d = res["diagnostics"]
    print("demo", give_ref_mask, res["params"], res["score"], d["dE_before"], "->", d["dE_target"])
    assert d["dE_target"] < 1.0 and d["dE_target"] < d["dE_before"] / 5
    after = R.render_rgb(gen, res["params"])
    # moves toward the reference colour
    assert ciede2000(med(after, gm), d["reference_lab"]) < ciede2000(med(gen, gm), d["reference_lab"])


def test_multi_two_garments_match_package_and_stay_isolated():
    before, after, jm, pm = multi_sample(pkg_dir())
    pieces = lambda: [{"key": "top", "ref_rgb": after, "mask": jm, "ref_mask": jm},
                      {"key": "bottom", "ref_rgb": after, "mask": pm, "ref_mask": pm}]
    res = {r["key"]: r for r in S.solve_pieces(before, pieces(), strength=0.0)}
    for key, m in (("top", jm), ("bottom", pm)):
        out = R.render_rgb(before, res[key]["params"])
        truth = S.reference_truth(rgb_to_lab(after[m > 0]))
        de = float(ciede2000(med(out, m), truth))
        print("multi", key, res[key]["params"], "dE to package result:", round(de, 2))
        assert de < 2.0  # the package's own corrected colour is reproduced under OUR formula
    # the bottom piece solved alone is bit-identical: pieces do not influence each other
    alone = S.solve_pieces(before, pieces()[1:], strength=0.0)[0]
    assert alone["params"] == res["bottom"]["params"]
    # adjusting the top leaves the bottom solid region (and everything outside the top mask) untouched
    out = before.copy()
    out[jm > 0] = R.render_rgb(before, res["top"]["params"])[jm > 0]
    assert np.array_equal(out[pm > 0], before[pm > 0])
    assert np.array_equal(out[(jm == 0)], before[(jm == 0)])
    assert not np.array_equal(out[jm > 0], before[jm > 0])
