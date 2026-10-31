#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""aig_client 单测：签名（hmac.md 测试向量）、canonical query、长轮询、错误分类。不访问外部服务。

    python3 -I tests/test_aig_client.py -v      # 在 tools/prompt-reverse/ 下
"""
import importlib.util
import os
import socket
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def _load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


aig = _load("aig_client_under_test", os.path.join(os.path.dirname(HERE), "aig_client.py"))
fake_gateway = _load("fake_gateway", os.path.join(HERE, "fake_gateway.py"))


class SignatureTest(unittest.TestCase):
    """contract/hmac.md「Test vector」逐字断言。"""

    VECTOR = dict(secret="test-secret-please-rotate", caller_id="fusion-java", key_id="fj-2026-09",
                  purpose="calls", method="POST", path="/v1/calls", query="wait=25&retry_failed=true",
                  timestamp=1790000000, nonce="nonce-0123456789abcdef", raw_body=b'{"operation":"image.generate"}')

    def test_reference_vector_canonical_string(self):
        v = dict(self.VECTOR)
        for k in ("secret", "key_id"):
            v.pop(k)
        self.assertEqual(aig.canonical_string(**v), "\n".join([
            "v1",
            "fusion-java",
            "calls",
            "POST",
            "/v1/calls",
            "retry_failed=true&wait=25",
            "1790000000",
            "nonce-0123456789abcdef",
            "6d62550888a7c7d42cb854d4321536b176ffe666fded010fdf87655718f61fbb",
        ]))

    def test_reference_vector_signature(self):
        h = aig.sign(**self.VECTOR)
        self.assertEqual(h["X-Aig-Signature"], "c85d585c8ce54c561674ca404ba99cff9570cfd7b32e50191edba2a0a6991d89")
        self.assertEqual(h, {
            "X-Aig-Version": "v1",
            "X-Aig-Caller": "fusion-java",
            "X-Aig-Key-Id": "fj-2026-09",
            "X-Aig-Purpose": "calls",
            "X-Aig-Timestamp": "1790000000",
            "X-Aig-Nonce": "nonce-0123456789abcdef",
            "X-Aig-Signature": "c85d585c8ce54c561674ca404ba99cff9570cfd7b32e50191edba2a0a6991d89",
        })

    def test_get_hashes_empty_body(self):
        self.assertEqual(aig.body_sha256(b""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

    def test_fresh_nonce_and_format(self):
        a = aig.sign(secret="s", caller_id="c", key_id="k", purpose="calls", method="GET", path="/v1/calls/x")
        b = aig.sign(secret="s", caller_id="c", key_id="k", purpose="calls", method="GET", path="/v1/calls/x")
        self.assertNotEqual(a["X-Aig-Nonce"], b["X-Aig-Nonce"])
        self.assertRegex(a["X-Aig-Nonce"], r"^[A-Za-z0-9_-]{16,128}$")
        self.assertRegex(a["X-Aig-Signature"], r"^[0-9a-f]{64}$")

    def test_missing_secret_is_config_error(self):
        with self.assertRaises(aig.AigConfigError):
            aig.sign(secret="", caller_id="c", key_id="k", purpose="calls", method="GET", path="/v1/calls")


class CanonicalTest(unittest.TestCase):
    def test_query_sorted_and_encoded(self):
        self.assertEqual(aig.canonical_query(""), "")
        self.assertEqual(aig.canonical_query(None), "")
        self.assertEqual(aig.canonical_query("wait=25&retry_failed=true"), "retry_failed=true&wait=25")
        self.assertEqual(aig.canonical_query({"wait": "25"}), "wait=25")
        # 空格 %20（不是 +）、保留 unreserved、其余大写 %XX
        self.assertEqual(aig.canonical_query([("a b", "x y"), ("k", "~-._/")]), "a%20b=x%20y&k=~-._%2F")
        self.assertEqual(aig.canonical_query([("k", "中")]), "k=%E4%B8%AD")
        # 同 key 按 value 排序；空值保留
        self.assertEqual(aig.canonical_query("b=2&a=&b=1"), "a=&b=1&b=2")

    def test_path_normalized(self):
        self.assertEqual(aig.normalize_path("//v1//calls/"), "/v1/calls")
        self.assertEqual(aig.normalize_path("/"), "/")
        self.assertEqual(aig.normalize_path("/v1/calls:batchGet"), "/v1/calls:batchGet")
        with self.assertRaises(ValueError):
            aig.normalize_path("/v1/calls?wait=1")


class ClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gw = fake_gateway.FakeGateway().start()

    @classmethod
    def tearDownClass(cls):
        cls.gw.stop()

    def setUp(self):
        self.gw.reset()
        self.client = aig.AigClient(self.gw.base_url, "prompt-reverse-v1", "test-secret", "prompt-reverse", wait_s=2)

    def body(self):
        return {"operation": "chat.completion", "model_group": "vlm.deepseek-flash",
                "input": {"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]}}

    def test_long_poll_until_terminal(self):
        self.gw.running_polls = 2
        self.gw.reply = lambda body: ('{"ok":1}', "think")
        rec = self.client.call(self.body(), idempotency_key="prompt-reverse:t:1", budget_s=20)
        self.assertEqual(rec["status"], "SUCCEEDED")
        self.assertEqual(rec["result"]["message"]["content"], '{"ok":1}')
        self.assertEqual(rec["result"]["message"]["reasoning_content"], "think")
        self.assertEqual(len(self.gw.gets), 3)  # 2 次 RUNNING + 1 次终态
        for g in self.gw.gets:
            self.assertEqual(g["query"], "wait=2")
            self.assertEqual(g["purpose"], "calls")
        sub = self.gw.submits[0]
        self.assertEqual(sub["headers"].get("Idempotency-Key"), "prompt-reverse:t:1")
        self.assertEqual(self.gw.auth_failures, [])

    def test_422_is_rejected_input(self):
        self.gw.submit_http_error = (422, "VALIDATION_FAILED")
        with self.assertRaises(aig.AigRejected) as cm:
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:2", budget_s=5)
        self.assertEqual(cm.exception.code, 422)
        self.assertTrue(cm.exception.is_input_rejected)
        self.assertFalse(cm.exception.retryable)
        self.assertEqual(len(self.gw.submits), 1)  # 不可重试：不重提

    def test_429_and_5xx_are_retryable(self):
        for status, code in ((429, "RATE_LIMITED"), (503, "UNAVAILABLE"), (500, "INTERNAL")):
            with self.subTest(status=status):
                self.gw.reset()
                self.gw.submit_http_error = (status, code)
                with self.assertRaises(aig.AigRetryable) as cm:
                    self.client.call(self.body(), idempotency_key="prompt-reverse:t:3", budget_s=5)
                self.assertEqual(cm.exception.code, status)
                self.assertEqual(cm.exception.error_code, code)
                self.assertFalse(isinstance(cm.exception, aig.AigNetworkError))
                # 提交阶段同一幂等键内部重提一次
                self.assertEqual(len(self.gw.submits), 2)
                keys = {s["headers"].get("Idempotency-Key") for s in self.gw.submits}
                self.assertEqual(keys, {"prompt-reverse:t:3"})

    def test_terminal_failures_classified(self):
        self.gw.fail_class = "INPUT_REJECTED"
        with self.assertRaises(aig.AigRejected) as cm:
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:4", budget_s=5)
        self.assertTrue(cm.exception.is_input_rejected)
        self.assertEqual(cm.exception.code, "FAILED/INPUT_REJECTED")
        self.gw.reset()
        self.gw.fail_class = "NO_CAPACITY"
        with self.assertRaises(aig.AigRetryable):
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:5", budget_s=5)
        self.gw.reset()
        self.gw.fail_class = "CONTENT_POLICY"
        with self.assertRaises(aig.AigRejected):
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:6", budget_s=5)

    def test_wrong_secret_is_401_rejected(self):
        bad = aig.AigClient(self.gw.base_url, "prompt-reverse-v1", "wrong", "prompt-reverse")
        with self.assertRaises(aig.AigRejected) as cm:
            bad.call(self.body(), idempotency_key="prompt-reverse:t:7", budget_s=5)
        self.assertEqual(cm.exception.code, 401)
        self.assertEqual(cm.exception.error_code, "AUTH_FAILED")
        self.assertEqual(self.gw.auth_failures, ["INVALID_SIGNATURE"])

    def test_budget_exhausted_is_retryable(self):
        self.gw.running_polls = 10_000
        started = time.monotonic()
        with self.assertRaises(aig.AigRetryable) as cm:
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:8", budget_s=1.5)
        self.assertEqual(cm.exception.code, "TIMEOUT")
        self.assertLess(time.monotonic() - started, 6)

    def test_network_error(self):
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        port = s.getsockname()[1]
        s.close()
        c = aig.AigClient("http://127.0.0.1:%d" % port, "k", "s", "prompt-reverse")
        with self.assertRaises(aig.AigNetworkError) as cm:
            c.call(self.body(), idempotency_key="prompt-reverse:t:9", budget_s=3)
        self.assertTrue(cm.exception.retryable)
        self.assertIn("连接失败", cm.exception.reason)

    def test_missing_config(self):
        for args in (("", "k", "s", "c"), ("http://x", "k", "", "c"), ("http://x", "", "s", "c")):
            with self.subTest(args=args):
                c = aig.AigClient(*args)
                self.assertFalse(c.configured)
                with self.assertRaises(aig.AigConfigError):
                    c.call(self.body(), idempotency_key="prompt-reverse:t:10", budget_s=1)

    def test_readiness_uses_catalog_scope(self):
        ready, reason = self.client.readiness("vlm.deepseek-flash")
        self.assertTrue(ready)
        self.assertEqual(reason, "")
        call = self.gw.readiness_calls[0]
        self.assertEqual(call["purpose"], "catalog")
        self.assertEqual(call["body"], {"items": [{"model_group": "vlm.deepseek-flash", "operation": "chat.completion"}]})

    def test_error_message_has_no_secret(self):
        self.gw.submit_http_error = (422, "VALIDATION_FAILED")
        try:
            self.client.call(self.body(), idempotency_key="prompt-reverse:t:11", budget_s=5)
        except aig.AigError as e:
            self.assertNotIn("test-secret", str(e))


if __name__ == "__main__":
    unittest.main()
