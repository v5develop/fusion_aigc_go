import numpy as np
import pytest

from colorfix_solver import colorfix_render as R
from colorfix_solver import solver as S
from colorfix_solver.colorspace import ciede2000, rgb_to_lab

from .synth import garment_scene, make_pair

KNOWN = [
    {"hue": 12, "saturation": -25, "value": 0, "brightness": -30, "contrast": 0},
    {"hue": -18, "saturation": 30, "value": 10, "brightness": 20, "contrast": 0},
    {"hue": 0, "saturation": -40, "value": -10, "brightness": 35, "contrast": 0},
    {"hue": 25, "saturation": 0, "value": 0, "brightness": 0, "contrast": 0},
]


def gain(p):
    return (1 + p["value"] / 200) * (1 + p["brightness"] / 100)


def garment_median(img, mask):
    lab = rgb_to_lab(img[mask > 0])
    return S.diffuse_median(lab)


@pytest.mark.parametrize("known", KNOWN)
@pytest.mark.parametrize("give_ref_mask", [True, False])
def test_recovers_known_params(known, give_ref_mask):
    gen, gm, ref, rm = make_pair(known)
    res = S.solve(gen, ref, gm, rm if give_ref_mask else None, strength=0.0)
    p = res["params"]
    print(known, "->", p, res["diagnostics"]["dE_target"])
    # rendered result matches the reference colour
    after = R.render_rgb(gen, p)
    assert float(ciede2000(garment_median(after, gm), garment_median(ref, rm))) < 1.0
    # identifiable parameters are close (value/brightness only through their product)
    assert abs(p["hue"] - known["hue"]) <= 4
    assert abs(p["saturation"] - known["saturation"]) <= 10
    assert abs(gain(p) - gain(known)) <= 0.08
    assert res["score"] > 80
    assert res["diagnostics"]["reference_mask"] == ("given" if give_ref_mask else "bg-diff")


def test_identity_when_reference_equals_generated():
    gen, gm, ref, rm = make_pair(R.ZERO)
    res = S.solve(gen, ref, gm, rm, strength=0.0)
    assert max(abs(v) for v in res["params"].values()) <= 4


def test_params_are_resolution_independent():
    known = KNOWN[0]
    gen, gm, ref, rm = make_pair(known)
    a = S.solve(gen, ref, gm, rm, strength=0.0)["params"]
    big = lambda x: np.kron(x, np.ones((4, 4) + (1,) * (x.ndim - 2), x.dtype))
    b = S.solve(big(gen), big(ref), big(gm), big(rm), strength=0.0, max_side=256)["params"]
    assert abs(a["hue"] - b["hue"]) <= 3 and abs(gain(a) - gain(b)) <= 0.05


def test_shadowed_reference_uses_lit_part():
    flat, (m,) = garment_scene(240, 240, [(50, 40, 190, 200)], [(180, 150, 110)], bg=(240, 240, 240), seed=3)
    flat[40:200, 120:190] = (flat[40:200, 120:190] * 0.4).astype(np.uint8)  # hard shadow
    lab = rgb_to_lab(flat[m > 0])
    assert S.reference_truth(lab)[0] > S.diffuse_median(lab)[0] + 5


def test_output_bounds_and_ints():
    gen, gm, ref, rm = make_pair({"hue": 30, "saturation": 70, "value": 0, "brightness": 80, "contrast": 0})
    p = S.solve(gen, ref, gm, rm, strength=0.0)["params"]
    for k, (lo, hi) in R.RANGES.items():
        assert isinstance(p[k], int) and lo <= p[k] <= hi


def test_empty_mask_rejected():
    gen, gm, ref, rm = make_pair(KNOWN[0])
    with pytest.raises(S.SolveError) as e:
        S.solve(gen, ref, np.zeros_like(gm), rm)
    assert e.value.code == "mask_empty"


def test_timeout():
    gen, gm, ref, rm = make_pair(KNOWN[0])
    with pytest.raises(S.SolveTimeout):
        S.solve(gen, ref, gm, rm, timeout=0.0)


# ------------------------------------------------------------- multi piece

def two_piece_scene():
    gen, (top_m, bot_m) = garment_scene(420, 300, [(60, 30, 240, 200), (60, 215, 240, 400)],
                                        [(150, 70, 70), (60, 70, 130)], seed=5)
    ref_top, (tm,) = garment_scene(260, 260, [(40, 40, 220, 220)], [(150, 70, 70)], bg=(230, 230, 230), seed=6)
    ref_bot, (bm,) = garment_scene(260, 260, [(40, 40, 220, 220)], [(60, 70, 130)], bg=(230, 230, 230), seed=7)
    return gen, top_m, bot_m, ref_top, tm, ref_bot, bm


def test_pieces_independent_and_isolated():
    gen, top_m, bot_m, ref_top, tm, ref_bot, bm = two_piece_scene()
    ref_top2 = R.render_rgb(ref_top, {"hue": 15, "saturation": -30, "brightness": -25})
    ref_bot2 = R.render_rgb(ref_bot, {"hue": -10, "saturation": 25, "brightness": 20})
    pieces = lambda rt: [
        {"key": "top", "ref_rgb": rt, "mask": top_m, "ref_mask": tm},
        {"key": "bottom", "ref_rgb": ref_bot2, "mask": bot_m, "ref_mask": bm},
    ]
    a = {r["key"]: r for r in S.solve_pieces(gen, pieces(ref_top))}
    b = {r["key"]: r for r in S.solve_pieces(gen, pieces(ref_top2))}
    # changing the top reference changes top params but not bottom params (bit-identical)
    assert a["top"]["params"] != b["top"]["params"]
    assert a["bottom"]["params"] == b["bottom"]["params"]
    # applying the top params on the top mask only leaves the bottom solid region untouched (diff 0)
    for res in (a, b):
        out = gen.copy()
        mk = top_m > 0
        out[mk] = R.render_rgb(gen, res["top"]["params"])[mk]
        assert np.array_equal(out[bot_m > 0], gen[bot_m > 0])
        assert np.array_equal(out[(top_m == 0) & (bot_m == 0)], gen[(top_m == 0) & (bot_m == 0)])


def test_pieces_overlap_is_exclusive():
    gen, top_m, bot_m, ref_top, tm, ref_bot, bm = two_piece_scene()
    overlap = top_m.copy()
    overlap[190:260, 60:240] = 255  # top mask bleeds into the bottom piece
    ex = S.exclusive_masks([overlap.astype(np.float32) / 255, bot_m.astype(np.float32) / 255])
    assert not (ex[0] & ex[1]).any() and ex[0].sum() > top_m.astype(bool).sum() - 1
