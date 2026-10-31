# -*- coding: utf-8 -*-
"""ai-gateway 最小客户端（仅标准库）：HMAC v1 签名、提交调用、长轮询取结果。

签名规则见 ai-gateway contract/hmac.md（参考实现 src/ai_gateway/contract/hmac.py，算法逐行照抄）。
serve_reverse.py 用 importlib 按绝对路径加载本文件（`python3 -I` 下脚本目录不在 sys.path）。

错误分类（都继承 AigError）：
  AigConfigError    地址 / key id / secret 缺失；不发请求
  AigRejected       不可重试：HTTP 4xx（408/429 除外），或调用终态 FAILED 且失败类别为
                    INPUT_REJECTED / CONTENT_POLICY / BUDGET_EXCEEDED 等输入或策略类
  AigRetryable      可重试：HTTP 408/429/5xx、网络错误、超时、终态 FAILED 且失败类别为容量/上游类
  AigNetworkError   AigRetryable 的子类：连不上 / 超时（无 HTTP 状态码）

日志约定：本模块不写日志；异常消息里只有状态码、错误码、check 与网关返回的 message，
不含 secret、请求体、图片或提示词。
"""
import datetime
import hashlib
import hmac as _hmac
import json
import re
import secrets
import socket
import time
import urllib.error
import urllib.request
from urllib.parse import parse_qsl, quote, urlsplit

VERSION = "v1"
PROTOCOL_HEADERS = (
    "X-Aig-Version",
    "X-Aig-Caller",
    "X-Aig-Key-Id",
    "X-Aig-Purpose",
    "X-Aig-Timestamp",
    "X-Aig-Nonce",
    "X-Aig-Signature",
)
TERMINAL = frozenset({"SUCCEEDED", "FAILED", "UNKNOWN", "CANCELLED"})
NONCE_RE = re.compile(r"^[A-Za-z0-9_-]{16,128}$")
IDEMPOTENCY_KEY_RE = re.compile(r"^[A-Za-z0-9._:/#@-]{1,255}$")
MAX_WAIT_S = 60

# 失败类别 → 是否值得重提（新幂等键）。未列出的按不可重试处理。
RETRYABLE_FAILURE_CLASSES = frozenset({
    "NO_CAPACITY", "NO_ROUTE", "ATTEMPTS_EXHAUSTED", "VENDOR_FAILED",
    "DEADLINE_EXCEEDED", "REHOST_FAILED", "INTERNAL_ERROR", "STREAM_INTERRUPTED",
})


# ---------------------------------------------------------------- 错误


class AigError(Exception):
    """code：HTTP 状态码（int）或调用终态描述（如 'FAILED/INPUT_REJECTED'）。"""

    retryable = False

    def __init__(self, message, *, code=None, error_code="", check="", failure_class="",
                 call_id=""):
        super().__init__(message)
        self.code = code
        self.error_code = error_code or ""
        self.check = check or ""
        self.failure_class = failure_class or ""
        self.call_id = call_id or ""

    @property
    def is_input_rejected(self):
        """网关判定输入不合法：HTTP 400/422 校验失败，或调用 FAILED 且类别 INPUT_REJECTED。"""
        if self.failure_class == "INPUT_REJECTED":
            return True
        return isinstance(self.code, int) and self.code in (400, 422)

    def mentions(self, *words):
        text = " ".join([str(self), self.error_code, self.check]).lower()
        return any(w.lower() in text for w in words)


class AigConfigError(AigError):
    pass


class AigRejected(AigError):
    pass


class AigRetryable(AigError):
    retryable = True


class AigNetworkError(AigRetryable):
    @property
    def reason(self):
        return str(self)


# ---------------------------------------------------------------- 签名（与 contract/hmac.py 一致）


def normalize_path(path):
    if not path or not path.startswith("/") or "?" in path or "#" in path:
        raise ValueError("path must be absolute and exclude query/fragment (check=hmac.path_format)")
    normalized = re.sub(r"/{2,}", "/", path)
    if len(normalized) > 1 and normalized.endswith("/"):
        normalized = normalized[:-1]
    return normalized


def canonical_query(query):
    """RFC 3986 编码每个 key 与 value（保留 unreserved），按 (key, value) 排序，用 & 连接。"""
    if not query:
        return ""
    if isinstance(query, str):
        pairs = parse_qsl(query, keep_blank_values=True, strict_parsing=False)
    elif isinstance(query, dict):
        pairs = list(query.items())
    else:
        pairs = list(query)
    encoded = sorted((quote(str(k), safe="-._~"), quote(str(v), safe="-._~")) for k, v in pairs)
    return "&".join("%s=%s" % (k, v) for k, v in encoded)


def body_sha256(raw_body):
    return hashlib.sha256(raw_body).hexdigest()


def canonical_string(*, caller_id, purpose, method, path, query, timestamp, nonce, raw_body):
    if not re.match(r"^[A-Za-z]{3,10}$", method or ""):
        raise ValueError("invalid HTTP method (check=hmac.method_format)")
    return "\n".join([
        VERSION,
        caller_id,
        purpose,
        method.upper(),
        normalize_path(path),
        canonical_query(query),
        str(timestamp),
        nonce,
        body_sha256(raw_body),
    ])


def sign(*, secret, caller_id, key_id, purpose, method, path, query=None, raw_body=b"",
         timestamp=None, nonce=None):
    """返回七个协议头。"""
    if not secret:
        raise AigConfigError("AIG_SECRET 未配置", code="CONFIG")
    ts = int(time.time()) if timestamp is None else int(timestamp)
    nonce = nonce or secrets.token_urlsafe(24)
    if not NONCE_RE.match(nonce):
        raise ValueError("invalid nonce (check=hmac.nonce_format)")
    text = canonical_string(caller_id=caller_id, purpose=purpose, method=method, path=path,
                            query=query, timestamp=ts, nonce=nonce, raw_body=raw_body)
    signature = _hmac.new(secret.encode("utf-8"), text.encode("utf-8"), hashlib.sha256).hexdigest()
    return {
        "X-Aig-Version": VERSION,
        "X-Aig-Caller": caller_id,
        "X-Aig-Key-Id": key_id,
        "X-Aig-Purpose": purpose,
        "X-Aig-Timestamp": str(ts),
        "X-Aig-Nonce": nonce,
        "X-Aig-Signature": signature,
    }


# ---------------------------------------------------------------- 客户端


def utc_deadline(seconds):
    t = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=max(1, seconds))
    return t.strftime("%Y-%m-%dT%H:%M:%SZ")


def new_idempotency_key(caller, route):
    route = re.sub(r"[^A-Za-z0-9._:/#@-]", "-", str(route or "call"))[:80]
    key = "%s:%s:%s" % (caller, route, secrets.token_hex(12))
    return key[:255]


def _error_from_http(status, raw, retry_after=None):
    error_code = check = msg = call_id = ""
    try:
        err = (json.loads(raw.decode("utf-8") or "{}") or {}).get("error") or {}
        error_code = str(err.get("code") or "")
        check = str(err.get("check") or "")
        msg = str(err.get("message") or "")[:300]
        call_id = str(err.get("call_id") or "")
    except (ValueError, AttributeError, UnicodeDecodeError):
        pass
    text = "网关返回 HTTP %d" % status
    if error_code:
        text += " %s" % error_code
    if check:
        text += "（%s）" % check
    if msg:
        text += "：%s" % msg
    cls = AigRetryable if (status in (408, 429) or status >= 500) else AigRejected
    return cls(text, code=status, error_code=error_code, check=check, call_id=call_id)


class AigClient:
    def __init__(self, base_url, key_id, secret, caller, *, wait_s=25, timeout_pad_s=10):
        self.base_url = (base_url or "").rstrip("/")
        self.key_id = key_id or ""
        self.secret = secret or ""
        self.caller = caller or ""
        self.wait_s = max(0, min(MAX_WAIT_S, int(wait_s or 0)))
        self.timeout_pad_s = timeout_pad_s
        parts = urlsplit(self.base_url) if self.base_url else None
        self._prefix = (parts.path.rstrip("/") if parts else "")

    @property
    def configured(self):
        return bool(self.base_url and self.secret and self.key_id and self.caller)

    def _check_config(self):
        missing = [n for n, v in (("AIG_BASE_URL", self.base_url), ("AIG_SECRET", self.secret),
                                  ("AIG_KEY_ID", self.key_id), ("AIG_CALLER", self.caller)) if not v]
        if missing:
            raise AigConfigError("AI 网关未配置：" + "、".join(missing), code="CONFIG")

    def request(self, method, path, *, purpose, body=None, query=None, timeout=30, headers=None):
        """发一次签名请求，返回 (status, json)。非 2xx 抛 AigRejected / AigRetryable。"""
        self._check_config()
        raw = b"" if body is None else json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        full_path = self._prefix + path
        qs = canonical_query(query) if query else ""
        url = self.base_url + path + ("?" + qs if qs else "")
        hdrs = {"Accept": "application/json"}
        if body is not None:
            hdrs["Content-Type"] = "application/json"
        hdrs.update(headers or {})
        hdrs.update(sign(secret=self.secret, caller_id=self.caller, key_id=self.key_id, purpose=purpose,
                         method=method, path=full_path, query=query, raw_body=raw))
        req = urllib.request.Request(url, data=raw if body is not None else None, headers=hdrs, method=method)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                data = resp.read()
                return resp.status, (json.loads(data.decode("utf-8")) if data else {})
        except urllib.error.HTTPError as e:
            try:
                raw_err = e.read()
            except Exception:  # noqa: BLE001
                raw_err = b""
            raise _error_from_http(e.code, raw_err) from None
        except urllib.error.URLError as e:
            raise AigNetworkError("网关连接失败（%s）" % getattr(e, "reason", e)) from None
        except (socket.timeout, TimeoutError) as e:
            raise AigNetworkError("网关请求超时（%s）" % e) from None
        except ValueError as e:
            raise AigRetryable("网关响应不是 JSON（%s）" % e) from None

    # ------------------------------------------------------------ calls

    def submit(self, body, idempotency_key, *, timeout=30):
        if not IDEMPOTENCY_KEY_RE.match(idempotency_key or ""):
            raise ValueError("invalid idempotency key")
        last = None
        # 同一幂等键重提是安全的：网关按 (caller, key) 返回同一条记录
        for attempt in range(2):
            try:
                _, rec = self.request("POST", "/v1/calls", purpose="calls", body=body, timeout=timeout,
                                      headers={"Idempotency-Key": idempotency_key})
                return rec
            except AigRetryable as e:
                last = e
                if attempt == 0:
                    time.sleep(0.5)
        raise last

    def get(self, call_id, *, wait=None, timeout=None):
        query = {"wait": str(int(wait))} if wait else None
        t = timeout if timeout is not None else (int(wait or 0) + self.timeout_pad_s)
        _, rec = self.request("GET", "/v1/calls/" + call_id, purpose="calls", query=query, timeout=t)
        return rec

    def wait_terminal(self, record, *, budget_s):
        """长轮询直到终态或预算用尽（预算用尽抛 AigRetryable）。GET 的网络错误在预算内重试。"""
        rec = record or {}
        deadline = time.monotonic() + max(1.0, budget_s)
        call_id = str(rec.get("id") or "")
        while str(rec.get("status") or "") not in TERMINAL:
            if not call_id:
                raise AigRetryable("网关响应缺少调用 id")
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AigRetryable("等待网关结果超时（%s）" % call_id, code="TIMEOUT", call_id=call_id)
            wait = max(1, min(self.wait_s or 1, int(remaining)))
            try:
                rec = self.get(call_id, wait=wait)
            except AigNetworkError:
                time.sleep(min(1.0, max(0.0, deadline - time.monotonic())))
                continue
            except AigRetryable as e:
                if isinstance(e.code, int) and e.code >= 500:
                    time.sleep(min(1.0, max(0.0, deadline - time.monotonic())))
                    continue
                raise
        return rec

    def call(self, body, *, idempotency_key, budget_s):
        """提交并等到终态；SUCCEEDED 返回 record，其余终态抛 AigError。"""
        started = time.monotonic()
        rec = self.submit(body, idempotency_key, timeout=min(60, self.timeout_pad_s + 20))
        rec = self.wait_terminal(rec, budget_s=budget_s - (time.monotonic() - started))
        status = str(rec.get("status") or "")
        if status == "SUCCEEDED":
            return rec
        failure = rec.get("failure") or {}
        fclass = str(failure.get("class") or status)
        fcode = str(failure.get("code") or "")
        fmsg = str(failure.get("message") or "")[:300]
        text = "网关调用 %s/%s" % (status, fclass) + ("（%s）" % fcode if fcode else "") + ("：%s" % fmsg if fmsg else "")
        cls = AigRetryable if (status == "FAILED" and fclass in RETRYABLE_FAILURE_CLASSES) else AigRejected
        raise cls(text, code="%s/%s" % (status, fclass), error_code=fclass, check=fcode,
                  failure_class=fclass, call_id=str(rec.get("id") or ""))

    # ------------------------------------------------------------ catalog

    def readiness(self, model_group, operation="chat.completion", *, timeout=3):
        """返回 (ready: bool, reason: str)。"""
        _, data = self.request("POST", "/v1/catalog/readiness", purpose="catalog", timeout=timeout,
                               body={"items": [{"model_group": model_group, "operation": operation}]})
        for item in (data or {}).get("items") or []:
            if item.get("model_group") == model_group:
                return bool(item.get("ready")), str(item.get("reason") or "")
        return False, "model_group.missing_in_response"
