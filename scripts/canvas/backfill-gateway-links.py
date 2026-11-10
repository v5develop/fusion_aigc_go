#!/usr/bin/env python3
"""回填画布历史运行的网关调用关联（一次性 / 可重复执行，幂等）。

背景：画布直接调网关时，部署新版之前 Java 不知道这些调用号，创作任务链路缺网关阶段。
本脚本从画布管理端读取 gateway 后端的运行，把 backend_ref（网关调用号）回报给 Java 管理端接口
POST /api/v1/admin/creation/tasks/{taskId}/gateway-calls。Java 侧按调用号去重，重复执行不会重复写。

用法（默认只预览，不写入）：
  export CANVAS_ADMIN_TOKEN=...   # 画布管理端 Bearer（需 canvas:view）
  export JAVA_ADMIN_TOKEN=...     # 创作任务管理端 Bearer（需 task:retry）
  python3 scripts/canvas/backfill-gateway-links.py --host https://t81-aigc.pengqiwl.com.cn            # 预览
  python3 scripts/canvas/backfill-gateway-links.py --host https://t81-aigc.pengqiwl.com.cn --apply    # 写入

token 只从环境变量读取，不接受命令行参数（避免进入 shell 历史与进程列表）。
"""
import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

RUN_TASK = re.compile(r"^cv_(.+)_run_[0-9a-f]{4}$")
CALL_ID = re.compile(r"^call_[0-9A-Za-z]{10,40}$")
PAGE_SIZE = 100


def request_json(method, url, token, body=None):
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Authorization", "Bearer " + token)
    req.add_header("Accept", "application/json")
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"HTTP {e.code} {method} {url}") from e
    if payload.get("code") not in (0, None):
        raise RuntimeError(f"code={payload.get('code')} msg={payload.get('msg')} {method} {url}")
    return payload


def iter_gateway_runs(host, token):
    page = 1
    while True:
        q = urllib.parse.urlencode({"backend": "gateway", "page": page, "page_size": PAGE_SIZE})
        data = request_json("GET", f"{host}/api/v1/canvas/admin/runs?{q}", token)["data"]
        rows = data.get("list") or []
        for row in rows:
            yield row
        if len(rows) < PAGE_SIZE:
            return
        page += 1


def plan(run):
    m = RUN_TASK.match(run.get("task_id") or "")
    if not m:
        return None, "任务号不符合 cv_<任务号>_run_<hex4>"
    java_id = m.group(1)
    calls = [c.strip() for c in (run.get("backend_ref") or "").split(",") if c.strip()]
    calls = [c for c in calls if CALL_ID.match(c)]
    if not calls:
        return None, "没有合法的网关调用号"
    return (java_id, calls), None


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", required=True, help="网关地址，如 https://t81-aigc.pengqiwl.com.cn")
    ap.add_argument("--apply", action="store_true", help="真正写入（默认只预览）")
    args = ap.parse_args()
    canvas_token = os.environ.get("CANVAS_ADMIN_TOKEN", "")
    java_token = os.environ.get("JAVA_ADMIN_TOKEN", "")
    if not canvas_token or not java_token:
        sys.exit("缺少环境变量 CANVAS_ADMIN_TOKEN / JAVA_ADMIN_TOKEN")
    host = args.host.rstrip("/")

    total = planned = written = skipped = failed = 0
    for run in iter_gateway_runs(host, canvas_token):
        total += 1
        target, reason = plan(run)
        if target is None:
            skipped += 1
            continue
        java_id, calls = target
        planned += 1
        if not args.apply:
            print(f"[预览] run={run.get('run_id')} task={java_id} calls={','.join(calls)}")
            continue
        try:
            url = f"{host}/api/v1/admin/creation/tasks/{urllib.parse.quote(java_id, safe='')}/gateway-calls"
            request_json("POST", url, java_token, {"callIds": calls})
            written += 1
        except RuntimeError as e:
            failed += 1
            print(f"[失败] run={run.get('run_id')} task={java_id}: {e}", file=sys.stderr)
    print(json.dumps({"runs": total, "planned": planned, "written": written,
                      "skipped": skipped, "failed": failed, "apply": args.apply}, ensure_ascii=False))
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, urllib.error.URLError) as e:  # 鉴权失效、网络错误等：给出可读原因，不打印堆栈
        sys.exit(f"回填中止：{e}")
