import base64
import http.client
import io
import json
import threading
from dataclasses import replace
from http.server import BaseHTTPRequestHandler, HTTPServer

import numpy as np
import pytest
from PIL import Image

from colorfix_solver.config import Config
from colorfix_solver.server import create_server

from .synth import garment_scene, make_pair

TOKEN = "t" * 24
KNOWN = {"hue": 12, "saturation": -25, "value": 0, "brightness": -30, "contrast": 0}


def png(arr) -> bytes:
    b = io.BytesIO()
    Image.fromarray(arr).save(b, "PNG")
    return b.getvalue()


def b64(arr) -> str:
    return base64.b64encode(png(arr)).decode()


class Running:
    def __init__(self, **kw):
        self.cfg = replace(Config(), host="127.0.0.1", port=0, token=TOKEN, **{"solve_timeout": 30.0, **kw})
        self.srv = create_server(self.cfg)
        self.port = self.srv.server_address[1]
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()

    def req(self, method, path, body=None, headers=None, token=TOKEN, raw=None):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=60)
        h = dict(headers or {})
        if token:
            h.setdefault("Authorization", f"Bearer {token}")
        data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        if data is not None:
            h.setdefault("Content-Type", "application/json")
        c.request(method, path, body=data, headers=h)
        r = c.getresponse()
        txt = r.read()
        c.close()
        return r.status, (json.loads(txt) if txt else None)


@pytest.fixture()
def api():
    s = Running()
    yield s
    s.close()


def payload(known=KNOWN):
    gen, gm, ref, rm = make_pair(known)
    return {
        "image_b64": b64(gen),
        "reference": [{"key": "top", "b64": b64(ref), "mask_b64": b64(rm)}],
        "masks": [{"key": "top", "b64": b64(gm)}],
    }


def test_healthz_open(api):
    st, j = api.req("GET", "/healthz", token=None)
    assert st == 200 and j["status"] == "ok"


def test_auth(api):
    p = payload()
    assert api.req("POST", "/v1/solve", p, token=None)[0] == 401
    assert api.req("POST", "/v1/solve", p, token="x" * 24)[0] == 401
    assert api.req("POST", "/v1/solve", p, headers={"Authorization": "Basic abc"}, token=None)[0] == 401


def test_start_requires_token_unless_dev():
    with pytest.raises(SystemExit):
        create_server(replace(Config(), port=0, token=""))
    with pytest.raises(SystemExit):
        create_server(replace(Config(), port=0, token="short"))
    with pytest.raises(SystemExit):  # dev mode without a token must stay on loopback
        create_server(replace(Config(), port=0, token="", env="dev", host="0.0.0.0"))
    s = create_server(replace(Config(), port=0, token="", env="dev"))
    s.server_close()


def test_solve_b64_recovers_params(api):
    p = payload()
    p["strength"] = 0
    p["diagnostics"] = True
    st, j = api.req("POST", "/v1/solve", p)
    assert st == 200, j
    piece = j["pieces"][0]
    assert piece["key"] == "top" and j["version"].startswith("colorfix-solver/")
    assert set(piece["params"]) == {"hue", "saturation", "value", "brightness", "contrast"}
    assert abs(piece["params"]["hue"] - KNOWN["hue"]) <= 4
    assert piece["score"] > 80 and "diagnostics" in piece


def test_solve_two_pieces_without_ref_mask(api):
    gen, (tm, bm) = garment_scene(420, 300, [(60, 30, 240, 200), (60, 215, 240, 400)], [(150, 70, 70), (60, 70, 130)], seed=5)
    rt, _ = garment_scene(260, 260, [(40, 40, 220, 220)], [(150, 70, 70)], bg=(230, 230, 230), seed=6)
    rb, _ = garment_scene(260, 260, [(40, 40, 220, 220)], [(60, 70, 130)], bg=(230, 230, 230), seed=7)
    st, j = api.req("POST", "/v1/solve", {
        "image_b64": b64(gen),
        "reference": [{"key": "top", "b64": b64(rt)}, {"key": "bottom", "b64": b64(rb)}],
        "masks": [{"key": "top", "b64": b64(tm)}, {"key": "bottom", "b64": b64(bm)}],
    })
    assert st == 200, j
    assert [p["key"] for p in j["pieces"]] == ["top", "bottom"]


def test_mask_resized_to_image(api):
    p = payload()
    gen, gm, ref, rm = make_pair(KNOWN)
    small = np.kron(gm, np.ones((1, 1), np.uint8))[::2, ::2]  # half-size mask, same aspect
    p["masks"][0]["b64"] = b64(small)
    assert api.req("POST", "/v1/solve", p)[0] == 200


@pytest.mark.parametrize("mutate,status,code", [
    (lambda p: p.pop("reference"), 400, "bad_request"),
    (lambda p: p.update(masks=[{"key": "other", "b64": p["masks"][0]["b64"]}]), 400, "bad_request"),
    (lambda p: p["masks"][0].update(url="http://example.com/x.png"), 400, "bad_request"),   # both url and b64
    (lambda p: p.update(image_b64="!!!notbase64"), 400, "bad_b64"),
    (lambda p: p.update(image_b64=base64.b64encode(b"not an image").decode()), 400, "bad_image"),
    (lambda p: p.update(strength=2), 400, "bad_request"),
    (lambda p: p["reference"][0].update(key="../x"), 400, "bad_request"),
    (lambda p: p.update(masks=p["masks"] * 2, reference=p["reference"] * 2), 400, "bad_request"),  # duplicate keys
    (lambda p: p["masks"][0].update(b64=b64(np.zeros((300, 220), np.uint8))), 422, "mask_empty"),
])
def test_validation(api, mutate, status, code):
    p = payload()
    mutate(p)
    st, j = api.req("POST", "/v1/solve", p)
    assert (st, j["error"]["code"]) == (status, code), j


def test_transport_validation(api):
    assert api.req("POST", "/v1/solve", raw=b"{nope")[0] == 400
    assert api.req("POST", "/v1/solve", headers={"Content-Type": "text/plain"}, raw=b"{}")[0] == 415
    assert api.req("GET", "/nope")[0] == 404
    assert api.req("POST", "/v1/other", {})[0] == 404
    c = http.client.HTTPConnection("127.0.0.1", api.port, timeout=10)
    c.putrequest("POST", "/v1/solve")
    c.putheader("Authorization", f"Bearer {TOKEN}")
    c.putheader("Content-Type", "application/json")
    c.putheader("Transfer-Encoding", "chunked")
    c.endheaders()
    assert c.getresponse().status == 411


def test_body_and_pixel_limits():
    s = Running(max_body=2000)
    try:
        st, j = s.req("POST", "/v1/solve", payload())
        assert (st, j["error"]["code"]) == (413, "body_too_large")
    finally:
        s.close()
    s = Running(max_pixels=10_000)  # payload images are far bigger
    try:
        st, j = s.req("POST", "/v1/solve", payload())
        assert (st, j["error"]["code"]) == (413, "too_many_pixels")
    finally:
        s.close()
    s = Running(max_image_bytes=1000)
    try:
        st, j = s.req("POST", "/v1/solve", payload())
        assert st == 413
    finally:
        s.close()


def test_decompression_bomb_header_rejected():
    s = Running(max_pixels=1_000_000)
    try:
        big = png(np.full((4000, 4000, 3), 255, np.uint8))  # tiny file, 16 Mpx
        p = payload()
        p["image_b64"] = base64.b64encode(big).decode()
        st, j = s.req("POST", "/v1/solve", p)
        assert (st, j["error"]["code"]) == (413, "too_many_pixels")
    finally:
        s.close()


def test_solve_timeout():
    s = Running(solve_timeout=0.0)
    try:
        st, j = s.req("POST", "/v1/solve", payload())
        assert (st, j["error"]["code"]) == (504, "solve_timeout")
    finally:
        s.close()


# ------------------------------------------------------------------ outbound fetch

class _Img(BaseHTTPRequestHandler):
    files: dict = {}

    def do_GET(self):
        if self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", "http://127.0.0.1:1/x")
            self.end_headers()
            return
        data = self.files.get(self.path)
        if data is None:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a):
        pass


class _QuietServer(HTTPServer):
    def server_bind(self):  # HTTPServer.server_bind does a getfqdn() lookup that can stall
        import socketserver
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]


@pytest.fixture()
def img_server():
    gen, gm, ref, rm = make_pair(KNOWN)
    _Img.files = {"/gen.png": png(gen), "/ref.png": png(ref), "/refmask.png": png(rm), "/mask.png": png(gm)}
    srv = _QuietServer(("127.0.0.1", 0), _Img)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_address[1]}"
    srv.shutdown()
    srv.server_close()


def url_payload(base):
    return {
        "image_url": f"{base}/gen.png",
        "reference": [{"key": "top", "url": f"{base}/ref.png", "mask_url": f"{base}/refmask.png"}],
        "masks": [{"key": "top", "url": f"{base}/mask.png"}],
    }


def test_fetch_private_blocked_by_default(api, img_server):
    st, j = api.req("POST", "/v1/solve", url_payload(img_server))
    assert (st, j["error"]["code"]) == (403, "fetch_forbidden")


def test_fetch_allowed_when_explicit(img_server):
    s = Running(allow_private_fetch=True)
    try:
        st, j = s.req("POST", "/v1/solve", url_payload(img_server))
        assert st == 200, j
        p = url_payload(img_server)
        p["image_url"] = img_server + "/redirect"  # redirects are never followed
        st, j = s.req("POST", "/v1/solve", p)
        assert st == 502 and j["error"]["code"] == "fetch_status"
        p["image_url"] = img_server + "/missing.png"
        assert s.req("POST", "/v1/solve", p)[0] == 502
    finally:
        s.close()


def test_fetch_allow_list(img_server):
    s = Running(allow_private_fetch=True, fetch_allow_hosts=("cdn.example.com",))
    try:
        st, j = s.req("POST", "/v1/solve", url_payload(img_server))
        assert (st, j["error"]["code"]) == (403, "fetch_forbidden")
    finally:
        s.close()


@pytest.mark.parametrize("url,code", [
    ("file:///etc/passwd", "fetch_scheme"),
    ("ftp://example.com/a.png", "fetch_scheme"),
    ("gopher://127.0.0.1/", "fetch_scheme"),
    ("http://user:pw@example.com/a.png", "fetch_url"),
    ("http://169.254.169.254/latest/meta-data", "fetch_forbidden"),
    ("http://[::1]/a.png", "fetch_forbidden"),
    ("http://10.0.0.5/a.png", "fetch_forbidden"),
    ("http://[::ffff:127.0.0.1]/a.png", "fetch_forbidden"),
    ("http://0.0.0.0/a.png", "fetch_forbidden"),
    ("http://localhost/a.png", "fetch_forbidden"),
])
def test_fetch_ssrf_rejected(api, url, code):
    p = payload()
    p.pop("image_b64")
    p["image_url"] = url
    st, j = api.req("POST", "/v1/solve", p)
    assert j["error"]["code"] == code, (url, j)
    assert st in (400, 403)
