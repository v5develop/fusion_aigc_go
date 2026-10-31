# -*- coding: utf-8 -*-
"""测试用伪 ai-gateway（标准库 http.server），在测试进程内起线程。

- 校验七个 X-Aig-* 头齐全且签名正确（用测试 secret 重算），否则 401 AUTH_FAILED
- POST /v1/calls：记录请求，返回 201 RUNNING（逼客户端走长轮询）；可按需注入 HTTP 错误或终态失败
- GET  /v1/calls/{id}?wait=N：前 running_polls 次仍 RUNNING，之后返回终态
- POST /v1/catalog/readiness：返回 ready
不访问任何外部服务。
"""
import hashlib
import hmac
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qsl, quote, urlsplit

SEVEN = ("X-Aig-Version", "X-Aig-Caller", "X-Aig-Key-Id", "X-Aig-Purpose",
         "X-Aig-Timestamp", "X-Aig-Nonce", "X-Aig-Signature")


def _canonical_query(qs):
    pairs = parse_qsl(qs, keep_blank_values=True)
    enc = sorted((quote(k, safe="-._~"), quote(v, safe="-._~")) for k, v in pairs)
    return "&".join("%s=%s" % kv for kv in enc)


class FakeGateway:
    def __init__(self, secret="test-secret", caller="prompt-reverse", key_id="prompt-reverse-v1"):
        self.secret, self.caller, self.key_id = secret, caller, key_id
        self.lock = threading.Lock()
        self.submits = []      # [{"headers":{}, "body":{}, "path":...}]
        self.gets = []         # [{"path":..., "query":...}]
        self.readiness_calls = []
        self.auth_failures = []
        self.records = {}
        self.running_polls = 1
        self.submit_http_error = None    # (status, code) 注入提交阶段 HTTP 错误
        self.get_http_error = None
        self.reject_json = False         # 带 response_format 时终态 FAILED/INPUT_REJECTED
        self.fail_class = None           # 终态 FAILED 的失败类别
        self.reply = None                # callable(body) -> (content, reasoning_content)
        self._seq = 0
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self.port = self.server.server_address[1]
        self.base_url = "http://127.0.0.1:%d" % self.port
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def start(self):
        self.thread.start()
        return self

    def stop(self):
        self.server.shutdown()
        self.server.server_close()

    def reset(self):
        with self.lock:
            self.submits.clear()
            self.gets.clear()
            self.readiness_calls.clear()
            self.auth_failures.clear()
            self.records.clear()
            self.submit_http_error = self.get_http_error = None
            self.reject_json = False
            self.fail_class = None
            self.reply = None
            self.running_polls = 1

    # ------------------------------------------------------------------
    def verify(self, method, path, query, headers, raw):
        missing = [h for h in SEVEN if not headers.get(h)]
        if missing:
            return "MISSING_FIELD:" + ",".join(missing)
        if headers["X-Aig-Caller"] != self.caller or headers["X-Aig-Key-Id"] != self.key_id:
            return "UNKNOWN_KEY"
        text = "\n".join(["v1", headers["X-Aig-Caller"], headers["X-Aig-Purpose"], method.upper(), path,
                          _canonical_query(query), headers["X-Aig-Timestamp"], headers["X-Aig-Nonce"],
                          hashlib.sha256(raw).hexdigest()])
        want = hmac.new(self.secret.encode(), text.encode(), hashlib.sha256).hexdigest()
        if not hmac.compare_digest(want, headers["X-Aig-Signature"]):
            return "INVALID_SIGNATURE"
        return ""

    def _terminal(self, rec):
        body = rec["_body"]
        inp = body.get("input") or {}
        if self.reject_json and inp.get("response_format"):
            rec.update(status="FAILED", failure={"class": "INPUT_REJECTED", "code": "vendor.rejected",
                                                 "message": "response_format json_object not supported"})
        elif self.fail_class:
            rec.update(status="FAILED", failure={"class": self.fail_class, "code": "test.fail", "message": "x"})
        else:
            content, reasoning = (self.reply(body) if self.reply else ("{}", None))
            msg = {"role": "assistant", "content": content}
            if reasoning is not None:
                msg["reasoning_content"] = reasoning
            rec.update(status="SUCCEEDED", result={"operation": "chat.completion", "message": msg,
                                                   "finish_reason": "stop"},
                       usage={"input_tokens": 10, "output_tokens": 5})
        rec.pop("phase", None)

    def _public(self, rec):
        return {k: v for k, v in rec.items() if not k.startswith("_")}

    def _handler(self):
        gw = self

        class H(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):
                pass

            def reply_json(self, status, obj):
                data = json.dumps(obj).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def err(self, status, code, check="test", message="injected"):
                self.reply_json(status, {"error": {"code": code, "check": check, "message": message}})

            def handle_any(self, method):
                u = urlsplit(self.path)
                length = int(self.headers.get("Content-Length") or 0)
                raw = self.rfile.read(length) if length else b""
                headers = {h: self.headers.get(h) for h in SEVEN}
                bad = gw.verify(method, u.path, u.query, headers, raw)
                if bad:
                    with gw.lock:
                        gw.auth_failures.append(bad)
                    return self.err(401, "AUTH_FAILED", "hmac." + bad)
                if method == "POST" and u.path == "/v1/calls":
                    body = json.loads(raw.decode())
                    with gw.lock:
                        gw.submits.append({"headers": dict(self.headers.items()), "body": body,
                                           "purpose": headers["X-Aig-Purpose"]})
                        if gw.submit_http_error:
                            st, code = gw.submit_http_error
                            return self.err(st, code, message="injected submit error")
                        key = self.headers.get("Idempotency-Key") or ""
                        for rec in gw.records.values():
                            if rec["idempotency_key"] == key:
                                return self.reply_json(200, gw._public(rec))
                        gw._seq += 1
                        cid = "call_%012d" % gw._seq
                        rec = {"id": cid, "idempotency_key": key, "status": "RUNNING", "phase": "VENDOR",
                               "model_group": body.get("model_group"), "_body": body, "_polls": 0}
                        gw.records[cid] = rec
                        return self.reply_json(201, gw._public(rec))
                if method == "GET" and u.path.startswith("/v1/calls/"):
                    cid = u.path.rsplit("/", 1)[-1]
                    with gw.lock:
                        gw.gets.append({"path": u.path, "query": u.query, "purpose": headers["X-Aig-Purpose"]})
                        if gw.get_http_error:
                            st, code = gw.get_http_error
                            return self.err(st, code)
                        rec = gw.records.get(cid)
                        if not rec:
                            return self.err(404, "NOT_FOUND")
                        rec["_polls"] += 1
                        if rec["status"] == "RUNNING" and rec["_polls"] > gw.running_polls:
                            gw._terminal(rec)
                        return self.reply_json(200, gw._public(rec))
                if method == "POST" and u.path == "/v1/catalog/readiness":
                    body = json.loads(raw.decode())
                    with gw.lock:
                        gw.readiness_calls.append({"body": body, "purpose": headers["X-Aig-Purpose"]})
                    items = [{"model_group": it["model_group"], "ready": True, "enabled_deployments": 2,
                              "available_deployments": 2} for it in body.get("items") or []]
                    return self.reply_json(200, {"items": items, "config_version": 1,
                                                 "checked_at": "2026-10-31T00:00:00Z"})
                return self.err(404, "NOT_FOUND")

            def do_GET(self):
                self.handle_any("GET")

            def do_POST(self):
                self.handle_any("POST")

        return H
