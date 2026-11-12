"""HTTP API (stdlib only).

POST /v1/solve   bearer-token protected
GET  /healthz    open (no data, for probes)
"""
from __future__ import annotations

import hmac
import json
import logging
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import cv2
import numpy as np

from . import VERSION
from .config import Config
from .imageio import ImageError, decode_b64, decode_mask, decode_rgb
from .safefetch import FetchError, fetch
from .solver import SolveError, SolveTimeout, solve_pieces

log = logging.getLogger("colorfix")
KEY_RE = re.compile(r"^[A-Za-z0-9_.\-]{1,64}$")


class ApiError(Exception):
    def __init__(self, status: int, code: str, message: str):
        super().__init__(message)
        self.status, self.code, self.message = status, code, message


class _Loader:
    """Resolve {url|b64} sources with limits; fetches share one request-wide budget."""

    def __init__(self, cfg: Config):
        self.cfg = cfg
        self.deadline = time.monotonic() + 3 * cfg.fetch_timeout

    def raw(self, url, b64, what: str) -> bytes:
        if (url is None) == (b64 is None):
            raise ApiError(400, "bad_request", f"{what}: give exactly one of url / b64")
        try:
            if b64 is not None:
                return decode_b64(b64, self.cfg.max_image_bytes)
            left = self.deadline - time.monotonic()
            if left <= 0:
                raise ApiError(504, "fetch_timeout", "fetching images took too long")
            return fetch(url, max_bytes=self.cfg.max_image_bytes, timeout=min(self.cfg.fetch_timeout, left),
                         allow_private=self.cfg.allow_private_fetch, allow_hosts=self.cfg.fetch_allow_hosts)
        except FetchError as e:
            status = {"fetch_timeout": 504, "fetch_failed": 502, "fetch_status": 502, "fetch_dns": 502,
                      "fetch_forbidden": 403, "fetch_too_large": 413}.get(e.code, 400)
            raise ApiError(status, e.code, f"{what}: {e}") from e
        except ImageError as e:
            raise ApiError(413 if e.code == "too_large" else 400, e.code, f"{what}: {e}") from e

    def rgb(self, url, b64, what):
        try:
            return decode_rgb(self.raw(url, b64, what), self.cfg.max_pixels)
        except ImageError as e:
            raise ApiError(413 if e.code in ("too_many_pixels", "too_large") else 400, e.code, f"{what}: {e}") from e

    def mask(self, url, b64, what, size_hw):
        try:
            m = decode_mask(self.raw(url, b64, what), self.cfg.max_pixels)
        except ImageError as e:
            raise ApiError(413 if e.code in ("too_many_pixels", "too_large") else 400, e.code, f"{what}: {e}") from e
        h, w = size_hw
        if m.shape != (h, w):
            if abs(m.shape[1] / m.shape[0] - w / h) > 0.01 * (w / h):
                raise ApiError(400, "mask_size", f"{what}: aspect ratio differs from its image")
            m = cv2.resize(m, (w, h), interpolation=cv2.INTER_AREA)
        return m


def _opt_str(d: dict, k: str, what: str):
    v = d.get(k)
    if v is not None and not isinstance(v, str):
        raise ApiError(400, "bad_request", f"{what}.{k} must be a string")
    return v


def handle_solve(cfg: Config, body: dict) -> dict:
    if not isinstance(body, dict):
        raise ApiError(400, "bad_request", "body must be a JSON object")
    refs, masks = body.get("reference"), body.get("masks")
    if not isinstance(refs, list) or not isinstance(masks, list) or not masks:
        raise ApiError(400, "bad_request", "reference[] and masks[] are required")
    if len(masks) > cfg.max_pieces or len(refs) > cfg.max_pieces:
        raise ApiError(400, "too_many_pieces", f"at most {cfg.max_pieces} pieces")
    strength = body.get("strength", 0.5)
    if isinstance(strength, bool) or not isinstance(strength, (int, float)) or not 0 <= strength <= 1:
        raise ApiError(400, "bad_request", "strength must be a number in [0, 1]")

    def keyed(items, what):
        out = {}
        for it in items:
            if not isinstance(it, dict) or not isinstance(it.get("key"), str) or not KEY_RE.match(it["key"]):
                raise ApiError(400, "bad_request", f"{what}[].key must match {KEY_RE.pattern}")
            if it["key"] in out:
                raise ApiError(400, "bad_request", f"duplicate key '{it['key']}' in {what}")
            out[it["key"]] = it
        return out

    ref_by, mask_by = keyed(refs, "reference"), keyed(masks, "masks")
    if set(ref_by) != set(mask_by):
        raise ApiError(400, "bad_request", "reference[] and masks[] must have the same keys")

    ld = _Loader(cfg)
    gen = ld.rgb(_opt_str(body, "image_url", "body"), _opt_str(body, "image_b64", "body"), "image")
    pieces = []
    for key, m in mask_by.items():  # mask order = overlap priority (earlier wins)
        r = ref_by[key]
        ref = ld.rgb(_opt_str(r, "url", f"reference[{key}]"), _opt_str(r, "b64", f"reference[{key}]"), f"reference[{key}]")
        ref_mask = None
        mu, mb = _opt_str(r, "mask_url", "reference"), _opt_str(r, "mask_b64", "reference")
        if mu is not None or mb is not None:
            ref_mask = ld.mask(mu, mb, f"reference[{key}].mask", ref.shape[:2])
        gm = ld.mask(_opt_str(m, "url", f"masks[{key}]"), _opt_str(m, "b64", f"masks[{key}]"), f"masks[{key}]", gen.shape[:2])
        pieces.append({"key": key, "ref_rgb": ref, "mask": gm, "ref_mask": ref_mask})

    try:
        res = solve_pieces(gen, pieces, strength=float(strength), max_side=cfg.max_side, timeout=cfg.solve_timeout)
    except SolveTimeout as e:
        raise ApiError(504, "solve_timeout", "solving took too long") from e
    except SolveError as e:
        raise ApiError(422, e.code, str(e)) from e
    out = []
    for r in res:
        item = {"key": r["key"], "params": r["params"], "score": r["score"]}
        if body.get("diagnostics") is True:
            item["diagnostics"] = r["diagnostics"]
        out.append(item)
    return {"pieces": out, "version": VERSION}


def make_handler(cfg: Config, sem: threading.BoundedSemaphore):
    class Handler(BaseHTTPRequestHandler):
        timeout = cfg.read_timeout
        server_version = "colorfix-solver"
        sys_version = ""

        def log_message(self, fmt, *args):  # no bodies, no auth headers
            log.info("%s %s", self.address_string(), fmt % args)

        def _send(self, status: int, obj: dict):
            data = json.dumps(obj, ensure_ascii=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(data)

        def _err(self, status, code, message):
            self._send(status, {"error": {"code": code, "message": message}})

        def _authorized(self) -> bool:
            if not cfg.token:
                return True  # dev mode, loopback only (Config.validate)
            h = self.headers.get("Authorization", "")
            return h.startswith("Bearer ") and hmac.compare_digest(h[7:].strip().encode(), cfg.token.encode())

        def do_GET(self):
            if self.path == "/healthz":
                return self._send(200, {"status": "ok", "version": VERSION})
            self._err(404, "not_found", "not found")

        def do_POST(self):
            if self.path != "/v1/solve":
                return self._err(404, "not_found", "not found")
            if not self._authorized():
                return self._err(401, "unauthorized", "missing or invalid token")
            try:
                if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
                    raise ApiError(411, "length_required", "Content-Length is required")
                cl = self.headers.get("Content-Length", "")
                if not cl.isdigit():
                    raise ApiError(411, "length_required", "Content-Length is required")
                if int(cl) > cfg.max_body:
                    raise ApiError(413, "body_too_large", f"body exceeds {cfg.max_body} bytes")
                if "json" not in self.headers.get("Content-Type", "").lower():
                    raise ApiError(415, "unsupported_media_type", "Content-Type must be application/json")
                raw = self.rfile.read(int(cl))
                if len(raw) != int(cl):
                    raise ApiError(400, "bad_request", "truncated body")
                try:
                    body = json.loads(raw)
                except (ValueError, RecursionError) as e:
                    raise ApiError(400, "bad_json", "body is not valid JSON") from e
                if not sem.acquire(blocking=False):
                    raise ApiError(429, "busy", "too many concurrent solves")
                try:
                    out = handle_solve(cfg, body)
                finally:
                    sem.release()
                self._send(200, out)
            except ApiError as e:
                self._err(e.status, e.code, e.message)
            except TimeoutError:
                self._err(408, "read_timeout", "request read timed out")
            except Exception:  # never leak internals
                log.exception("solve failed")
                self._err(500, "internal", "internal error")

    return Handler


class _Server(ThreadingHTTPServer):
    daemon_threads = True

    def server_bind(self):  # skip HTTPServer's getfqdn() reverse lookup (can stall for tens of seconds)
        import socketserver
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]


def create_server(cfg: Config) -> ThreadingHTTPServer:
    cfg.validate()
    srv = _Server((cfg.host, cfg.port), make_handler(cfg, threading.BoundedSemaphore(cfg.max_concurrent)))
    return srv


def main() -> None:
    logging.basicConfig(level=logging.INFO, stream=sys.stderr, format="%(asctime)s %(levelname)s %(message)s")
    cfg = Config.from_env()
    srv = create_server(cfg)
    log.info("%s listening on %s:%d (env=%s, auth=%s)", VERSION, cfg.host, srv.server_address[1], cfg.env,
             "token" if cfg.token else "OFF")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
