# colorfix-solver (phase 1)

Reference-driven auto colour-fix solver. Given a generated image, a mask per garment piece and a reference
(flat-lay) image per piece, it returns the five slider values (`hue / saturation / value / brightness / contrast`)
that move each piece to the reference colour. Standalone Python service; nothing in the Go app calls it yet.

## Design decision: optimise under OUR render formula

The solver does not use the qifenshu package's `ps_adjust` HSL model. Every candidate parameter set is rendered with
`colorfix_solver/colorfix_render.py`, a numpy port of `applyColorFixPixels` from
`fusion-aigc-web/src/utils/colorfix-math.ts` (the formula behind the browser CSS-filter preview and our exact export):
hue-rotate matrix -> saturate matrix -> brightness x contrast LUT, same clamps and the same two rounding rules
(`Math.round` for the saturate output, half-to-even for the `Uint8ClampedArray` LUT). Solved values therefore look the
same in the preview and in the export.

Parity is proven against the real TS file: `parity/gen_golden.ts` (run by node with type stripping) renders 8
synthetic images x 44 parameter sets (352 renders incl. range corners, random and fractional values, alpha) and
`tests/test_parity.py` compares: **max abs diff = 0** (limit 1). The web file is only read, never modified;
override its location with `COLORFIX_MATH_TS`.

Parameter names: the API uses `saturation / value / brightness`; the web code calls them `sat / light / bright`
(`colorfix_render.normalize_params` accepts both). Ranges are the web ones: hue +-30, saturation -60..70,
value +-40, brightness +-80, contrast +-50. Results are integers (the UI sliders are integers).
Note `value` and `brightness` multiply (`(1+value/200)*(1+brightness/100)`), so they are one degree of freedom;
the solver splits it by a small regulariser, do not read meaning into the split.

## Method (per piece, independent)

1. downscale to long side <= 1024 (params are resolution independent), erode the mask a couple of px;
2. reference "truth" colour: diffuse median Lab of the reference garment; if its L spread is large (hard shadow)
   the median of the lit top quarter instead (package idea). Without a reference mask: background-difference mask;
3. scene ratio `gen L / ref L` clipped to 0.70..1.30; `strength` (default 0.5, package value) decides how far the
   target lightness follows the reference (0 = full reference L, 1 = keep generated L); a/b = reference;
4. objective = CIEDE2000(diffuse median of the rendered garment, target) with a 0.3 dead band, a fabric-spread
   (texture) guard and a small param regulariser (contrast 3x stiffer);
5. coarse grid (hue x brightness x saturation) -> 3 Nelder-Mead starts (+ identity) -> integer polish against the
   exact quantised renderer.

Pieces are made overlap-exclusive in the order of `masks[]` (earlier wins), then solved separately: a piece's result
never depends on another piece's reference/params. Applying one piece's params inside its own mask leaves everything
else bit-identical (tested).

## Run

```bash
python -m venv .venv && .venv/bin/pip install -r requirements.txt
COLORFIX_TOKEN=$(openssl rand -hex 24) .venv/bin/python -m colorfix_solver      # 127.0.0.1:18090
COLORFIX_ENV=dev .venv/bin/python -m colorfix_solver                              # no token, loopback only
docker build -t colorfix-solver . && docker run --rm -p 127.0.0.1:18090:18090 -e COLORFIX_TOKEN=... colorfix-solver
```

Tests: `pip install -r requirements-dev.txt && pytest` (needs `node` and the web repo for the parity test, otherwise
that test skips). Package-sample acceptance: `COLORFIX_PKG_DIR=/path/to/unzipped/qifenshu-colorfix-2026-11-03 pytest`.

## Environment

| var | default | meaning |
|---|---|---|
| `HOST` / `PORT` | `127.0.0.1` / `18090` | bind address (Docker image sets `0.0.0.0`) |
| `COLORFIX_TOKEN` | - | shared bearer token, >= 16 chars. **Required** unless `COLORFIX_ENV=dev` (then loopback only) |
| `COLORFIX_ENV` | `prod` | `dev` allows no token |
| `COLORFIX_MAX_BODY` | 24 MiB | request body cap (413) |
| `COLORFIX_MAX_IMAGE_BYTES` | 12 MiB | per image, raw bytes (fetch and base64) |
| `COLORFIX_MAX_PIXELS` | 36,000,000 | per decoded image, checked from the header before decoding |
| `COLORFIX_MAX_PIECES` | 8 | |
| `COLORFIX_SOLVE_MAX_SIDE` | 1024 | solve resolution |
| `COLORFIX_SOLVE_TIMEOUT` | 20 s | solver deadline (504) |
| `COLORFIX_FETCH_TIMEOUT` | 10 s | per fetch; all fetches of a request share 3x this |
| `COLORFIX_READ_TIMEOUT` | 15 s | socket read timeout (slowloris) |
| `COLORFIX_MAX_CONCURRENT` | 4 | concurrent solves, more -> 429 |
| `COLORFIX_ALLOW_PRIVATE_FETCH` | off | allow fetching loopback / private addresses (dev only) |
| `COLORFIX_FETCH_ALLOW_HOSTS` | - | optional comma list; when set only these hosts may be fetched |

## API

`GET /healthz` -> `{"status":"ok","version":...}` (no auth, no data).

`POST /v1/solve` with `Authorization: Bearer <token>`, `Content-Type: application/json`:

```json
{
  "image_url": "https://...",            // or "image_b64" (exactly one)
  "reference": [{"key": "top", "url": "https://...", "mask_url": "https://..."}],   // url|b64, mask_url|mask_b64 optional
  "masks":     [{"key": "top", "url": "https://..."}],                               // url|b64; same keys as reference
  "strength": 0.5,                        // optional 0..1
  "diagnostics": false                    // optional: add per-piece diagnostics
}
```
```json
{"pieces": [{"key": "top", "params": {"hue": -9, "saturation": 2, "value": 0, "brightness": -7, "contrast": 3}, "score": 97.1}],
 "version": "colorfix-solver/0.1.0"}
```

`score` = `max(0, 100 - 10 * dE2000)` between the rendered garment and the target (higher is better); with
`diagnostics` you get dE before/after, Lab values, scene ratio, parameters at a bound, evals, seconds.
Masks: PNG/JPEG/WebP; RGBA with real transparency uses alpha, otherwise luminance; a mask of a different size but the same
aspect ratio is resized. Errors: `{"error":{"code","message"}}` - 400 validation, 401 auth, 403 forbidden fetch target,
411/413/415 transport limits, 422 unusable mask, 429 busy, 502 upstream fetch failure, 504 timeout.

Fetching: only caller-supplied `http(s)` URLs, no userinfo, host resolved once and the connection pinned to that
vetted IP (every record must be public, no DNS-rebinding window), no redirects followed, streamed byte cap and timeout.

## Excluded in phase 1

SAM3 / any segmentation (masks come from the caller), the package's `ps_adjust` HSL formula, `refine_mask` /
guided-filter alpha / composite image output (this service returns parameters only), the package's Flask server,
feedback / history / tune endpoints, `runs/` persistence, UI.

## Known limits

* Median colour match: contrast is not identifiable from colour statistics, so it stays near 0 unless a texture-spread
  correction is needed; saturation/hue are only constrained through the median chroma (+-10 on synthetic data).
* Dark or near-neutral garments needing a big lift can land on bounds (e.g. dark grey pants -> mid grey gave
  hue 30 / saturation -59 / brightness 75 / contrast -28, still within 1.4 dE of the package result); `at_bounds` in
  diagnostics flags it.
* The reference "lit quarter" rule assumes a flat-lay: a reference with strong lighting variation (a worn photo)
  biases the truth toward its bright part.
* Mask quality is the caller's job; one colour per piece (prints, lace, labels are not protected).
