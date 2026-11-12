#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""路由冒烟测试，不访问任何外部服务。

- RoutesTest：不配 AI 网关（AIG_*）启动 sidecar，验证路由表与边界（行为与原「未配 Key」一致）。
- GatewayRoutesTest：测试进程内起伪 gateway（tests/fake_gateway.py），sidecar 指向它，跑通
  analyze / pose/prompt，断言请求体（model_group、image part 用 uri、Idempotency-Key、七个签名头）。

    python3 -I tests/test_routes.py -v      # 在 tools/prompt-reverse/ 下
"""
import http.client
import importlib.util
import json
import os
import socket
import subprocess
import sys
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.join(os.path.dirname(HERE), "serve_reverse.py")
MAX_BODY = 32 * 1024 * 1024
SECRET = "route-test-secret-0123456789"
TINY_PNG = ("data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8"
            "z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


fake_gateway = _load("fake_gateway", os.path.join(HERE, "fake_gateway.py"))


def clean_env():
    """去掉宿主环境里的网关 / 旧 DeepSeek 配置，避免误连真实服务。"""
    return {k: v for k, v in os.environ.items()
            if not (k.startswith("DEEPSEEK_") or k.startswith("AIG_")
                    or k in ("REVERSE_MODEL_GROUP", "REVERSE_ALLOWED_DEPLOYMENTS"))}


def start_sidecar(env, stderr=subprocess.DEVNULL):
    port = free_port()
    env = dict(env, REVERSE_BIND=f"127.0.0.1:{port}")
    proc = subprocess.Popen([sys.executable, "-I", SERVER], env=env,
                            stdout=subprocess.DEVNULL, stderr=stderr)
    for _ in range(100):
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
            return proc, port
        except OSError:
            time.sleep(0.1)
    proc.kill()
    raise RuntimeError("sidecar 未能启动")


def http_call(port, method, path, body=None, timeout=10):
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=timeout)
    try:
        data = None if body is None else json.dumps(body).encode()
        conn.request(method, path, body=data,
                     headers={"Content-Type": "application/json"} if data else {})
        resp = conn.getresponse()
        raw = resp.read()
        try:
            return resp.status, json.loads(raw.decode() or "null")
        except ValueError:
            return resp.status, raw
    finally:
        conn.close()

POST_ROUTES = [
    "/api/pose/source",
    "/api/pose/prompt",
    "/api/reverse/analyze",
    "/api/reverse/analyze-batch",
    "/api/reverse/crop-plan",
    "/api/reverse/crop-check",
]


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


class RoutesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.proc, cls.port = start_sidecar(clean_env())

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait(timeout=5)

    def call(self, method, path, body=None):
        return http_call(self.port, method, path, body)

    def test_status(self):
        st, js = self.call("GET", "/api/status")
        self.assertEqual(st, 200)
        self.assertEqual(js["service"], "prompt-reverse")
        self.assertEqual(js["version"], "2026-11-10.13.merged")
        self.assertFalse(js["llm_configured"])
        self.assertEqual(js["model_group"], "vlm.deepseek-flash")
        self.assertEqual(js["model"], "vlm.deepseek-flash")
        self.assertEqual(js["gateway"], "")
        self.assertIsNone(js["gateway_ready"])
        self.assertTrue(js["vision"])
        self.assertNotIn("key", json.dumps(js).lower().replace("llm_configured", ""))
        self.assertNotIn("secret", json.dumps(js).lower())

    def test_post_routes_exist_and_reject_empty(self):
        """空 JSON：路由必须存在，且不能 404/500（无栈追踪）。

        不配网关时各路由的实际行为（与原「未配 Key」一致）：
        analyze-batch -> 400；pose/source、pose/prompt、crop-plan、crop-check 先检查配置 -> 502「未配置」；
        analyze 无图时走无图模板兜底 -> 200（不调用 LLM）。
        """
        for path in POST_ROUTES:
            with self.subTest(path=path):
                st, js = self.call("POST", path, {})
                self.assertNotIn(st, (404, 500), path)
                if path == "/api/reverse/analyze":
                    self.assertEqual(st, 200)
                    self.assertTrue(js.get("prompt"))
                elif 400 <= st < 500:
                    self.assertIn("error", js)
                else:
                    self.assertEqual(st, 502, path)
                    self.assertIn("未配置", js["error"])

    def test_missing_fields_are_4xx(self):
        """字段缺失/无效且不涉及网关配置的路径：必须是 4xx。"""
        cases = [
            ("/api/reverse/analyze-batch", {"images": []}),
            ("/api/pose/prompt", {"source": {"has_person": False}}),
        ]
        for path, body in cases:
            with self.subTest(path=path):
                st, js = self.call("POST", path, body)
                self.assertTrue(400 <= st < 500, (path, st, js))

    def test_invalid_json_is_400(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        conn.request("POST", "/api/reverse/analyze", body=b"{not json")
        self.assertEqual(conn.getresponse().status, 400)
        conn.close()

    def test_analyze_internal_error_is_502_json(self):
        """analyze 内部抛异常（body 是合法 JSON 但不是对象 -> payload.get 报 AttributeError）：
        必须回 502 JSON，而不是断连（断连会让上游 Go 只看到 EOF）。"""
        for body in ([], "x", 1):
            with self.subTest(body=body):
                st, js = self.call("POST", "/api/reverse/analyze", body)
                self.assertEqual(st, 502)
                self.assertIn("error", js)

    def test_removed_paths_are_404(self):
        for method, path in [
            ("GET", "/.env"), ("GET", "/serve_reverse.py"), ("GET", "/pose"),
            ("GET", "/"), ("GET", "/tutorial"), ("GET", "/static/app.js"),
            ("GET", "/api/billing"), ("POST", "/api/billing"),
            ("GET", "/api/billing/balance"), ("GET", "/api/reverse/status"),
            ("GET", "/api/reverse/whatever"), ("POST", "/api/reverse/whatever"),
            ("GET", "/api/pose/source"),
        ]:
            with self.subTest(method=method, path=path):
                st, _ = self.call(method, path, {} if method == "POST" else None)
                self.assertEqual(st, 404)

    def test_body_over_24mb_is_413(self):
        # 只发请求头声明超限长度，服务端应在读 body 前直接拒绝
        for path in ("/api/reverse/analyze", "/api/pose/prompt"):
            with self.subTest(path=path):
                conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
                conn.putrequest("POST", path)
                conn.putheader("Content-Type", "application/json")
                conn.putheader("Content-Length", str(MAX_BODY + 1))
                conn.endheaders()
                self.assertEqual(conn.getresponse().status, 413)
                conn.close()


class GatewayRoutesTest(unittest.TestCase):
    """sidecar → 伪 gateway：验证请求形状与降级链。"""

    @classmethod
    def setUpClass(cls):
        cls.gw = fake_gateway.FakeGateway(secret=SECRET).start()
        cls.stderr_path = os.path.join(HERE, ".sidecar-stderr.log")
        cls.stderr = open(cls.stderr_path, "wb")
        env = clean_env()
        env.update(AIG_BASE_URL=cls.gw.base_url, AIG_SECRET=SECRET, AIG_WAIT_S="2")
        cls.proc, cls.port = start_sidecar(env, stderr=cls.stderr)

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait(timeout=5)
        cls.stderr.close()
        try:
            with open(cls.stderr_path, "rb") as f:
                log = f.read().decode("utf-8", "replace")
            # 日志不得出现 secret、base64 图片、完整提示词
            assert SECRET not in log, "stderr 出现 secret"
            assert "base64," not in log, "stderr 出现 base64 图片"
            assert "iVBORw0KGgo" not in log, "stderr 出现图片数据"
        finally:
            os.remove(cls.stderr_path)
            cls.gw.stop()

    def setUp(self):
        self.gw.reset()

    def call(self, method, path, body=None):
        return http_call(self.port, method, path, body, timeout=60)

    def assert_signed_submit(self, sub, route):
        h = {k.lower(): v for k, v in sub["headers"].items()}
        for name in fake_gateway.SEVEN:
            self.assertTrue(h.get(name.lower()), name)
        self.assertEqual(h["x-aig-caller"], "prompt-reverse")
        self.assertEqual(h["x-aig-key-id"], "prompt-reverse-v1")
        self.assertEqual(h["x-aig-purpose"], "calls")
        self.assertRegex(h.get("idempotency-key", ""), r"^[A-Za-z0-9._:/#@-]{1,255}$")
        body = sub["body"]
        self.assertEqual(body["operation"], "chat.completion")
        self.assertEqual(body["model_group"], "vlm.deepseek-flash")
        self.assertEqual(body["labels"], {"caller": "prompt-reverse", "route": route})
        self.assertRegex(body["constraints"]["deadline_at"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        # 未设 REVERSE_ALLOWED_DEPLOYMENTS：不钉部署，组内正常路由（官方优先、百炼备用）
        self.assertEqual(set(body["constraints"]), {"deadline_at"})
        self.assertEqual(set(body), {"operation", "model_group", "input", "constraints", "labels"})
        inp = body["input"]
        self.assertLessEqual(set(inp), {"messages", "response_format", "thinking", "temperature", "max_tokens"})
        for m in inp["messages"]:
            self.assertIsInstance(m["content"], list)
            for part in m["content"]:
                if part["type"] == "image_url":
                    self.assertTrue(part["uri"].startswith("data:image/"))
                    self.assertNotIn("image_url", part)
                    self.assertIn(part.get("detail"), (None, "low", "high", "auto"))
                    self.assertLessEqual(set(part), {"type", "uri", "detail"})
                else:
                    self.assertEqual(set(part), {"type", "text"})
        return inp

    def test_status_configured(self):
        st, js = self.call("GET", "/api/status")
        self.assertEqual(st, 200)
        self.assertTrue(js["llm_configured"])
        self.assertEqual(js["model_group"], "vlm.deepseek-flash")
        self.assertEqual(js["gateway"], self.gw.base_url)
        self.assertEqual(js["allowed_deployments"], [])
        self.assertTrue(js["gateway_ready"])
        self.assertNotIn(SECRET, json.dumps(js))
        # readiness 缓存 60s：再查不再打网关
        self.call("GET", "/api/status")
        self.assertLessEqual(len(self.gw.readiness_calls), 1)

    def test_analyze_vision_via_gateway(self):
        self.gw.running_polls = 1
        self.gw.reply = lambda body: (json.dumps({
            "garment_type": "上装", "category_match": True,
            "garment_colors": ["白色"], "material": "棉", "structure": {"top": "圆领短袖"},
        }, ensure_ascii=False), None)
        st, js = self.call("POST", "/api/reverse/analyze", {
            "category": "top",
            "images": [{"role": "上装", "label": "正面", "data": TINY_PNG}],
        })
        self.assertEqual(st, 200, js)
        self.assertTrue(js["prompt"])
        self.assertTrue(js["meta"]["vision"], js)
        self.assertFalse(js["meta"]["degraded"], js)
        self.assertEqual(js["meta"]["model"], "vlm.deepseek-flash")
        self.assertEqual(len(self.gw.submits), 1)
        inp = self.assert_signed_submit(self.gw.submits[0], "analyze.vision")
        self.assertEqual(inp["response_format"], {"type": "json_object"})
        self.assertEqual(inp["thinking"], "off")
        self.assertEqual(inp["max_tokens"], 4000)
        self.assertEqual(inp["temperature"], 0.1)
        self.assertEqual([m["role"] for m in inp["messages"]], ["system", "user"])
        images = [p for p in inp["messages"][1]["content"] if p["type"] == "image_url"]
        self.assertEqual(images, [{"type": "image_url", "uri": TINY_PNG, "detail": "high"}])
        # 走了长轮询
        self.assertGreaterEqual(len(self.gw.gets), 2)
        self.assertTrue(all(g["query"] == "wait=2" for g in self.gw.gets))
        self.assertEqual(self.gw.auth_failures, [])

    def _many_details_reply(self, body):
        return (json.dumps({
            "garment_type": "上装", "garment_match": True, "garment_colors": ["白色"], "material": "棉",
            "structure": ["圆领", "短袖"],
            "design_details": [f"左前片第{i}处刺绣，直径约{i}cm" for i in range(1, 14)],
        }, ensure_ascii=False), None)

    def _design_count(self, prompt):
        import re
        block = prompt.split("【设计细节保留】", 1)[-1] if "【设计细节保留】" in prompt else prompt
        return len(re.findall(r"左前片第\d+处刺绣", block))

    def test_analyze_fine_read(self):
        """精细读图（F7 / T2）：fine=true 强制 deep（不关思考、6000 token、全部 high），读图提示词带精细要求，设计细节上限 +3。"""
        self.gw.reply = self._many_details_reply
        images = [{"role": "上装", "data": TINY_PNG}, {"role": "人物模特", "data": TINY_PNG.replace("AAAAB", "AAAAC")}]
        st, js = self.call("POST", "/api/reverse/analyze", {"category": "top", "images": images, "fine": True})
        self.assertEqual(st, 200, js)
        self.assertTrue(js["meta"].get("fine"), js["meta"])
        inp = self.assert_signed_submit(self.gw.submits[0], "analyze.vision")
        self.assertNotIn("thinking", inp)
        self.assertEqual(inp["max_tokens"], 6000)
        text = "".join(p["text"] for p in inp["messages"][1]["content"] if p["type"] == "text")
        self.assertIn("精细读图：", text)
        for p in inp["messages"][1]["content"]:
            if p["type"] == "image_url":
                self.assertEqual(p["detail"], "high")
        fine_n = self._design_count(js["prompt"])

        self.gw.reset()
        self.gw.reply = self._many_details_reply
        st, js2 = self.call("POST", "/api/reverse/analyze", {"category": "top", "images": images, "deep": True})
        self.assertEqual(st, 200, js2)
        self.assertNotIn("fine", js2["meta"])
        text2 = "".join(p["text"] for p in self.gw.submits[0]["body"]["input"]["messages"][1]["content"] if p["type"] == "text")
        self.assertNotIn("精细读图", text2)
        std_n = self._design_count(js2["prompt"])
        self.assertEqual(std_n, 8, js2["prompt"])
        self.assertEqual(fine_n, 11, js["prompt"])

    def test_analyze_batch_passes_fine(self):
        self.gw.reply = self._many_details_reply
        st, js = self.call("POST", "/api/reverse/analyze-batch", {
            "category": "top", "fine": True,
            "images": [{"role": "上装", "data": TINY_PNG}],
            "models": [{"id": "m-1", "data": TINY_PNG.replace("AAAAB", "AAAAC")}],
        })
        self.assertEqual(st, 200, js)
        self.assertTrue(js["results"][0]["meta"].get("fine"), js)
        text = "".join(p["text"] for p in self.gw.submits[0]["body"]["input"]["messages"][1]["content"] if p["type"] == "text")
        self.assertIn("精细读图：", text)

    def test_analyze_degrades_when_gateway_fails(self):
        """视觉失败 → 文字降级 → 模板兜底，链路照常。"""
        self.gw.submit_http_error = (503, "UNAVAILABLE")
        st, js = self.call("POST", "/api/reverse/analyze", {
            "category": "top",
            "images": [{"role": "上装", "data": TINY_PNG}],
        })
        self.assertEqual(st, 200, js)
        self.assertTrue(js["prompt"])
        self.assertTrue(js["meta"]["degraded"])
        self.assertFalse(js["meta"]["vision"])
        self.assertIn("读图返回 503", js["warning"])
        self.assertIn("已用模板生成", js["warning"])
        routes = [s["body"]["labels"]["route"] for s in self.gw.submits]
        self.assertIn("analyze.vision", routes)

    def test_analyze_text_fallback_reaches_gateway(self):
        """KNOWN DEVIATION（与网关改造无关，原文件即如此）：serve_reverse.py 缺少 `def _colors_text(palettes):`
        这一行（函数体孤立在 crop_plan 的 return 之后），user_prompt 抛 NameError，文字降级从不调用模型、
        直接落到模板。修好后本用例会 unexpected success，届时去掉 expectedFailure。"""
        self.gw.reply = lambda body: ("{}", None)
        self.gw.fail_class = None
        st, js = self.call("POST", "/api/reverse/analyze", {"category": "top"})
        self.assertEqual(st, 200, js)
        routes = [s["body"]["labels"]["route"] for s in self.gw.submits]
        self.assertIn("analyze.text", routes)

    def pose_payload(self):
        return {
            "source": {"has_person": True, "gender_age": "成年女性", "face": "鹅蛋脸",
                       "outfit": ["白色衬衫"], "background": "白墙"},
            "image": TINY_PNG,
            "mode": "short",
        }

    def pose_reply(self, body):
        return json.dumps({"has_person": True, "orientation": "正面站立", "arms": "双臂自然下垂",
                           "legs": "双脚并拢", "confidence": "高"}, ensure_ascii=False), None

    def test_pose_prompt_via_gateway(self):
        self.gw.reply = self.pose_reply
        st, js = self.call("POST", "/api/pose/prompt", self.pose_payload())
        self.assertEqual(st, 200, js)
        self.assertTrue(js["prompt"])
        self.assertEqual(js["pose"]["confidence"], "高")
        self.assertEqual(js["meta"]["model"], "vlm.deepseek-flash")
        self.assertEqual(js["meta"]["usage"], {"input_tokens": 10, "output_tokens": 5})
        self.assertEqual(len(self.gw.submits), 1)
        inp = self.assert_signed_submit(self.gw.submits[0], "pose.prompt")
        self.assertEqual(inp["response_format"], {"type": "json_object"})
        self.assertNotIn("thinking", inp)  # 原实现姿态不传 thinking
        images = [p for p in inp["messages"][1]["content"] if p["type"] == "image_url"]
        self.assertEqual(images, [{"type": "image_url", "uri": TINY_PNG, "detail": "high"}])

    def test_pose_json_rejected_retries_without_json(self):
        self.gw.reject_json = True
        self.gw.reply = self.pose_reply
        st, js = self.call("POST", "/api/pose/prompt", self.pose_payload())
        self.assertEqual(st, 200, js)
        self.assertEqual(len(self.gw.submits), 2)
        self.assertIn("response_format", self.gw.submits[0]["body"]["input"])
        self.assertNotIn("response_format", self.gw.submits[1]["body"]["input"])
        k1, k2 = (s["headers"].get("Idempotency-Key") for s in self.gw.submits)
        self.assertNotEqual(k1, k2)

    def test_pose_content_policy_is_502(self):
        self.gw.fail_class = "CONTENT_POLICY"
        st, js = self.call("POST", "/api/pose/prompt", self.pose_payload())
        self.assertEqual(st, 502)
        self.assertIn("读图返回 FAILED/CONTENT_POLICY", js["error"])
        self.assertEqual(len(self.gw.submits), 1)  # 不可重试，不重提

    def test_reasoning_content_fallback(self):
        self.gw.reply = lambda body: ("", self.pose_reply(body)[0])
        st, js = self.call("POST", "/api/pose/prompt", self.pose_payload())
        self.assertEqual(st, 200, js)
        self.assertEqual(js["pose"]["confidence"], "高")


    # ---- 图片 data URL 归一 / 校验（sidecar 直接 422，不打网关）----

    def test_image_mime_jpg_normalized_to_jpeg(self):
        self.gw.reply = self.pose_reply
        payload = self.pose_payload()
        payload["image"] = TINY_PNG.replace("data:image/png", "data:image/JPG")
        st, js = self.call("POST", "/api/pose/prompt", payload)
        self.assertEqual(st, 200, js)
        inp = self.assert_signed_submit(self.gw.submits[0], "pose.prompt")
        images = [p for p in inp["messages"][1]["content"] if p["type"] == "image_url"]
        self.assertTrue(images[0]["uri"].startswith("data:image/jpeg;base64,"), images[0]["uri"][:40])

    def test_unsupported_image_format_is_422(self):
        for path, payload in (("/api/pose/prompt", self.pose_payload()), ("/api/pose/source", {"image": TINY_PNG})):
            payload = json.loads(json.dumps(payload).replace("data:image/png", "data:image/bmp"))
            with self.subTest(path=path):
                st, js = self.call("POST", path, payload)
                self.assertEqual(st, 422, js)
                self.assertIn("不支持的图片格式", js["error"])
        self.assertEqual(len(self.gw.submits), 0)

    def test_base64_with_whitespace_is_422(self):
        for ws in ("\n", " ", "\r\n", "\t"):
            payload = self.pose_payload()
            payload["image"] = TINY_PNG[:40] + ws + TINY_PNG[40:]
            with self.subTest(ws=repr(ws)):
                st, js = self.call("POST", "/api/pose/prompt", payload)
                self.assertEqual(st, 422, js)
                self.assertIn("空白", js["error"])
        # 其它路由（含 analyze 的 images[].data）同样拦截
        st, js = self.call("POST", "/api/reverse/analyze",
                           {"images": [{"role": "正面", "data": TINY_PNG[:40] + "\n" + TINY_PNG[40:]}]})
        self.assertEqual(st, 422, js)
        self.assertEqual(len(self.gw.submits), 0)


class PinnedDeploymentsTest(unittest.TestCase):
    """REVERSE_ALLOWED_DEPLOYMENTS（排障钉渠道）→ 请求体 constraints.allowed_deployments。"""

    @classmethod
    def setUpClass(cls):
        cls.gw = fake_gateway.FakeGateway(secret=SECRET).start()
        env = clean_env()
        env.update(AIG_BASE_URL=cls.gw.base_url, AIG_SECRET=SECRET, AIG_WAIT_S="2",
                   REVERSE_ALLOWED_DEPLOYMENTS=" bailian-chat.vl.deepseek-flash, ,deepseek-chat.vl.deepseek-flash ")
        cls.proc, cls.port = start_sidecar(env)

    @classmethod
    def tearDownClass(cls):
        cls.proc.terminate()
        cls.proc.wait(timeout=5)
        cls.gw.stop()

    def test_allowed_deployments_in_request_body(self):
        self.gw.reset()
        self.gw.reply = lambda body: GatewayRoutesTest.pose_reply(self, body)
        st, js = http_call(self.port, "POST", "/api/pose/prompt", GatewayRoutesTest.pose_payload(self), timeout=60)
        self.assertEqual(st, 200, js)
        self.assertEqual(len(self.gw.submits), 1)
        body = self.gw.submits[0]["body"]
        self.assertEqual(body["model_group"], "vlm.deepseek-flash")
        self.assertEqual(body["constraints"]["allowed_deployments"],
                         ["bailian-chat.vl.deepseek-flash", "deepseek-chat.vl.deepseek-flash"])
        self.assertEqual(set(body["constraints"]), {"deadline_at", "allowed_deployments"})
        st, js = http_call(self.port, "GET", "/api/status")
        self.assertEqual(st, 200)
        self.assertEqual(js["allowed_deployments"],
                         ["bailian-chat.vl.deepseek-flash", "deepseek-chat.vl.deepseek-flash"])


class RequestBudgetTest(unittest.TestCase):
    """REVERSE_REQUEST_BUDGET_S：pose 多轮重试不得超出请求总预算；AIG_WAIT_S 非数字不崩溃。"""

    def test_pose_retries_stay_within_budget(self):
        gw = fake_gateway.FakeGateway(secret=SECRET).start()
        env = clean_env()
        env.update(AIG_BASE_URL=gw.base_url, AIG_SECRET=SECRET, AIG_WAIT_S="1", REVERSE_REQUEST_BUDGET_S="10")
        proc, port = start_sidecar(env)
        try:
            gw.fail_class = "NO_CAPACITY"      # 可重试：不设预算时会 4 轮 + 退避 2.5+5+7.5s
            payload = GatewayRoutesTest.pose_payload(None)
            t0 = time.monotonic()
            st, js = http_call(port, "POST", "/api/pose/prompt", payload, timeout=60)
            elapsed = time.monotonic() - t0
            self.assertEqual(st, 504, js)
            self.assertIn("总时限 10 秒", js["error"])
            self.assertLess(elapsed, 12, elapsed)   # 预算 10s，不得拖到 4 轮重试（>15s）
            self.assertLess(len(gw.submits), 4)
        finally:
            proc.terminate()
            proc.wait(timeout=5)
            gw.stop()

    def test_non_numeric_env_falls_back(self):
        env = clean_env()
        env.update(AIG_WAIT_S="abc", REVERSE_REQUEST_BUDGET_S="x1")
        err_path = os.path.join(HERE, ".sidecar-env-stderr.log")
        with open(err_path, "wb") as f:
            proc, port = start_sidecar(env, stderr=f)
        try:
            st, js = http_call(port, "GET", "/api/status")
            self.assertEqual(st, 200, js)
        finally:
            proc.terminate()
            proc.wait(timeout=5)
        try:
            with open(err_path, "rb") as f:
                log = f.read().decode("utf-8", "replace")
        finally:
            os.remove(err_path)
        self.assertIn("AIG_WAIT_S", log)
        self.assertIn("REVERSE_REQUEST_BUDGET_S", log)
        self.assertNotIn("Traceback", log)


if __name__ == "__main__":
    unittest.main()
