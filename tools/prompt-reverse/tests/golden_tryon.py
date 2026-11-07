#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""商拍（试穿）提示词黄金回归脚手架：有 Key 时录制并锁定，无 Key 时离线结构校验。

    python3 -I tests/golden_tryon.py record [--case NAME]      # 需要 sidecar 已配 Key；写 golden/expected/<case>.json
    python3 -I tests/golden_tryon.py check [--offline] [--case NAME]
    python3 -I tests/golden_tryon.py selftest                  # 不联网：用 sidecar 的「无 Key 模板」跑 validator 自检

环境变量：
    REVERSE_BASE         sidecar 地址，默认 http://127.0.0.1:8090
    GOLDEN_IGNORE_REGEX  逐字 diff 前，从两边提示词里删掉匹配的片段（时间戳类波动）
    GOLDEN_TIMEOUT       单次请求超时秒数，默认 200（读图最长 180s）

check 语义：
  - sidecar 可达且 /api/status 显示 llm_configured=true 且未指定 --offline：重跑并与 expected 逐字 diff，
    同时对新结果和 expected 都做结构校验。
  - 否则（无 Key / 连不上 / --offline）：不发任何 analyze 请求，只对 expected 做离线结构校验。
  - 没有 expected 文件的用例：只校验用例输入本身，并提示先 record；不算失败（退出码 0）。
  - 任何 FAIL -> 退出码 1。
"""
import argparse
import datetime
import hashlib
import importlib.util
import json
import os
import re
import sys
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
CASES = os.path.join(HERE, "golden", "cases")
EXPECTED = os.path.join(HERE, "golden", "expected")
SERVER = os.path.join(os.path.dirname(HERE), "serve_reverse.py")
BASE = os.environ.get("REVERSE_BASE", "http://127.0.0.1:8090").rstrip("/")
TIMEOUT = float(os.environ.get("GOLDEN_TIMEOUT", "200"))

ITEM_LABELS = {"top": "上衣", "bottom": "下装", "dress": "连衣裙", "outer": "外套", "shoes": "鞋子",
               "accessory": "配饰", "bra": "文胸", "panties": "内裤"}
# 复制自 serve_reverse.py 的 BANNED_WORDS / NEGATIONS（_sanitize 同款）；能 import 时以源码为准，见 _load_banned()
_BANNED_COPY = ("透视", "湿身", "紧身勾勒", "性感", "诱惑", "裸露", "私密", "半透明", "真空")
_NEG_COPY = ("不", "无", "非", "避免", "禁止", "不要")
# 面料同义词：按「用户标注面料」里出现的词扩展
FABRIC_SYNONYMS = {
    "皮革": ("皮革", "皮质", "真皮", "牛皮", "羊皮", "皮面", "leather"),
    "真丝": ("真丝", "桑蚕丝", "丝绸"),
}
# 这些段落承载 LLM / 用户文字，才做泄漏词检查；其余是固定模板，里面本来就有「不能裸露」之类的禁止句
LEAK_SCOPE = ("面料与颜色", "设计细节保留", "补充说明", "比例锚点", "模特与姿态", "光线与背景", "背景锁定",
              "款式依据", "皮革单品", "皮革质感", "皮革光线")
# case2：模特相关段（应因模特而异）；其余视为服装段（应逐字相同）
HUMAN_SECTIONS = ("模特与姿态", "光线与背景", "背景锁定")


def _load_banned():
    try:
        spec = importlib.util.spec_from_file_location("serve_reverse_src", SERVER)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return tuple(mod.BANNED_WORDS), tuple(mod.NEGATIONS), "serve_reverse.py"
    except Exception:  # noqa: BLE001
        return _BANNED_COPY, _NEG_COPY, "内置副本"


def _load_item_locks():
    """【单品锁定】逐件文字以源码 ITEM_LOCKS 为准（2026-11-07.2 起不再以「只替换X」开头）；加载失败返回 {}。"""
    try:
        spec = importlib.util.spec_from_file_location("serve_reverse_locks", SERVER)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return dict(mod.ITEM_LOCKS)
    except Exception:  # noqa: BLE001
        return {}


ITEM_LOCKS = _load_item_locks()
LABEL_KEYS = {v: k for k, v in ITEM_LABELS.items()}


# ------------------------------------------------------------------ 结构校验

def sections(prompt):
    """按【名称】切段：返回 [(名称, 正文)]；【x】之前的文字忽略；同名段保留多份。"""
    parts = re.split(r"(【[^】\n]{1,12}】)", prompt)
    out = []
    for i in range(1, len(parts) - 1, 2):
        out.append((parts[i][1:-1], parts[i + 1]))
    return out


def sec(secs, name):
    return "\n".join(b for n, b in secs if n == name)


def clauses(text):
    return [c for c in re.split(r"[；;。\n]", text) if c.strip()]


def fabric_words(value):
    words = {value}
    for _, syn in FABRIC_SYNONYMS.items():
        if any(s in value.lower() for s in syn):
            words.update(syn)
    return sorted((w for w in words if w), key=len, reverse=True)


def validate_tryon_prompt(prompt, case):
    """返回 [(规则, ok, 说明)]；ok 为 True/False/"warn"。case 用 golden/cases 里的完整对象。"""
    res = []

    def add(rule, ok, msg=""):
        res.append((rule, ok, msg))

    exp = case.get("expect") or {}
    req = case.get("request") or {}
    labels = exp.get("item_labels") or [ITEM_LABELS[k] for k in (req.get("items") or [])]
    notes = str(req.get("notes") or "").strip()
    secs = sections(prompt)
    names = [n for n, _ in secs]

    # 1 必需段落
    need = ["任务", "参考图", "本次替换", "单品锁定", "面料与颜色", "禁止项"]
    if notes:
        need.append("补充说明")
    miss = [n for n in need if n not in names]
    add("S1 必需段落", not miss, "缺少 " + "、".join(f"【{n}】" for n in miss) if miss else f"{len(secs)} 段")

    # 2 各单品段落按 items 顺序
    repl = sec(secs, "本次替换")
    lock = sec(secs, "单品锁定")
    pos_repl = [repl.find(l) for l in labels]
    # 单件走 CATS 品类锁（仍是「只替换X」）；多件按 ITEM_LOCKS 逐件文字定位，两种写法任一命中即可
    pos_lock = []
    for l in labels:
        p = lock.find("只替换" + l)
        item_lock = ITEM_LOCKS.get(LABEL_KEYS.get(l, ""), "")
        if p < 0 and item_lock:
            p = lock.find(item_lock)
        pos_lock.append(p)
    ok_repl = bool(labels) and all(p >= 0 for p in pos_repl) and pos_repl == sorted(pos_repl)
    ok_lock = bool(labels) and all(p >= 0 for p in pos_lock) and pos_lock == sorted(pos_lock)
    add("S2 单品顺序（本次替换）", ok_repl, f"期望 {'、'.join(labels)}；位置 {pos_repl}")
    add("S2 单品顺序（单品锁定）", ok_lock, f"位置 {pos_lock}")
    ref = sec(secs, "参考图")
    if req.get("set_mode") == "two" or not any("整套" in str(i.get("role")) for i in req.get("images") or []):
        seq = re.findall(r"整件服装（([^）]+)）", ref)
        want = []
        for im in req.get("images") or []:
            r = str(im.get("role") or "")
            if r in labels:
                want.append(r)
        add("S2 参考图角色顺序", seq == want and ref.find("模特图") > ref.rfind("整件服装"),
            f"提示词 {seq}；输入 {want}；模特图应在服装之后")
    else:
        add("S2 参考图角色（合影）", "整套穿搭合影" in ref and ref.find("模特图") > ref.find("整套穿搭合影"),
            "合影图在前、模特图在后")

    # 3 补充说明逐字带入
    if notes:
        add("S3 用户补充逐字出现", notes in sec(secs, "补充说明"), f"notes={notes!r}")

    # 4 E4 面料按件
    fab = exp.get("fabric_by_item") or {}
    if fab:
        body = sec(secs, "面料与颜色")
        tagged = "用户标注面料（按件）" in body
        add("E4a 面料段为按件格式", tagged, "应含「用户标注面料（按件）」")
        last = -1
        order_ok = True
        for lbl, val in fab.items():
            val = val[0] if isinstance(val, list) else val
            p = body.find(f"{lbl} {val}")
            order_ok = order_ok and p > last
            last = max(last, p)
        add("E4b 面料段逐件「件名 面料」", order_ok, "；".join(f"{k} {v[0] if isinstance(v, list) else v}" for k, v in fab.items()))
        all_labels = set(labels) | {"上装", "下装", "上衣", "鞋", "外套", "连衣裙", "配饰"}
        for lbl, val in fab.items():
            val = val[0] if isinstance(val, list) else val
            words = fabric_words(val)
            wrong, unattr, right = [], [], 0
            for n, b in secs:
                for c in clauses(b):
                    for w in words:
                        for m in re.finditer(re.escape(w), c.lower()):
                            before = c[:m.start()]
                            nearest, np = None, -1
                            for l in all_labels:
                                p = before.rfind(l)
                                if p > np:
                                    nearest, np = l, p
                            tgt = lbl if lbl != "鞋子" else "鞋"
                            if nearest is None:
                                unattr.append(f"【{n}】{c.strip()[:36]}")
                            elif tgt in nearest or nearest in lbl:
                                right += 1
                            else:
                                wrong.append(f"【{n}】{c.strip()[:36]}（紧邻前文件名「{nearest}」）")
            add(f"E4c「{val}」只归属{lbl}", not wrong and right > 0,
                (f"归属正确 {right} 处" if not wrong else "串件：" + " | ".join(wrong[:3])))
            if unattr:
                add(f"E4d「{val}」无归属句（风险提示）", "warn", " | ".join(sorted(set(unattr))[:3]))
        # 皮革专段只能点名皮革件
        leather_lbls = [l for l, v in fab.items() if any(w in str(v) for w in FABRIC_SYNONYMS["皮革"])]
        if leather_lbls:
            others = [l for l in labels if l not in leather_lbls]
            bad = []
            for n, b in secs:
                if n in ("皮革单品", "皮革质感", "皮革光线"):
                    bad += [f"【{n}】含{l}" for l in others if l in b]
            add("E4e 皮革专段不串到非皮革件", not bad, "；".join(bad) or "ok")

    # 5 泄漏词（与 _sanitize 同款否定判断）
    banned, negs, src = _load_banned()
    hits = []
    for n, b in secs:
        if n not in LEAK_SCOPE:
            continue
        for c in re.split(r"(?<=[。；;，,])", b):
            for w in banned:
                s = 0
                while True:
                    i = c.find(w, s)
                    if i < 0:
                        break
                    if not any(x in c[max(0, i - 3):i] for x in negs):
                        hits.append(f"【{n}】{w}")
                    s = i + 1
    add("L1 泄漏词（面料/设计/补充等变量段）", not hits, f"词表来源 {src}；" + ("命中 " + "、".join(sorted(set(hits))) if hits else "无"))

    # 6 长度 / base64
    add("L2 长度 > 800", len(prompt) > 800, f"{len(prompt)} 字")
    b64 = re.search(r"data:[\w/+.-]+;base64|[A-Za-z0-9+/]{120,}={0,2}", prompt)
    add("L3 不含 base64 / data URL", not b64, b64.group(0)[:40] if b64 else "无")
    add("L4 无明显截断（以句末标点结尾）", prompt.rstrip()[-1:] in "。！？.）)」", f"结尾 {prompt.rstrip()[-12:]!r}")
    return res


def validate_pair(case, results):
    """case2：两模特提示词服装段相同、模特段不同。results: [{prompt,meta}]"""
    res = []
    if len(results) < 2:
        return res
    a, b = [dict((n, bd) for n, bd in sections(r["prompt"])) for r in results[:2]]
    degraded = any((r.get("meta") or {}).get("degraded") for r in results[:2])
    garment = [n for n in a if n not in HUMAN_SECTIONS]
    diff = [n for n in garment if a.get(n) != b.get(n)] + [n for n in b if n not in a and n not in HUMAN_SECTIONS]
    res.append(("P1 两模特：服装段逐字相同", not diff, "差异段 " + "、".join(diff) if diff else f"{len(garment)} 段相同"))
    human_diff = [n for n in HUMAN_SECTIONS if a.get(n) != b.get(n)]
    if human_diff:
        res.append(("P2 两模特：人物段不同", True, "不同段 " + "、".join(human_diff)))
    else:
        res.append(("P2 两模特：人物段不同", "warn" if degraded else False,
                    "人物段完全相同" + ("（degraded=true，走的是无读图模板，属预期）" if degraded else "")))
    return res


def validate_case_input(case):
    res = []
    req = case.get("request") or {}
    imgs = [i for i in (req.get("images") or []) if isinstance(i, dict)]
    bad = [i for i in imgs if not str(i.get("data", "")).startswith("data:image/")]
    res.append(("I1 输入图均为 data URL", not bad and bool(imgs), f"{len(imgs)} 张"))
    hs = [hashlib.sha1(i["data"].encode()).hexdigest() for i in imgs if "data" in i]
    res.append(("I2 输入图互不重复（否则被 sha1 去重）", len(hs) == len(set(hs)), ""))
    exp = case.get("expect") or {}
    res.append(("I3 items 与 item_labels 对应",
                [ITEM_LABELS.get(k) for k in req.get("items") or []] == exp.get("item_labels"), ""))
    return res


# ------------------------------------------------------------------ 网络

def http(method, path, body=None):
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    r = urllib.request.Request(BASE + path, data=data, method=method,
                               headers={"Content-Type": "application/json"} if data else {})
    with urllib.request.urlopen(r, timeout=TIMEOUT) as resp:
        return json.loads(resp.read().decode("utf-8"))


def sidecar_state():
    """返回 (reachable, llm_configured, version)。"""
    try:
        st = http("GET", "/api/status")
        return True, bool(st.get("llm_configured")), str(st.get("version") or "")
    except (urllib.error.URLError, OSError, ValueError):
        return False, False, ""


def call_case(case):
    """返回 [{model_id, prompt, meta, warning}]。"""
    req = case["request"]
    if case["endpoint"] == "analyze-batch":
        js = http("POST", "/api/reverse/analyze-batch", req)
        out = []
        for r in js.get("results") or []:
            if r.get("error"):
                raise RuntimeError(f"模特 {r.get('model_id')} 失败：{r['error']}")
            out.append({"model_id": r.get("model_id", ""), "prompt": r.get("prompt", ""),
                        "meta": r.get("meta") or {}, "warning": r.get("warning", "")})
        return out
    js = http("POST", "/api/reverse/analyze", req)
    return [{"model_id": "", "prompt": js.get("prompt", ""), "meta": js.get("meta") or {}, "warning": js.get("warning", "")}]


def derive(prompt):
    secs = sections(prompt)
    return {"length": len(prompt), "sha1": hashlib.sha1(prompt.encode()).hexdigest(), "sections": [n for n, _ in secs]}


def load_cases(only):
    out = []
    for f in sorted(os.listdir(CASES)):
        if f.endswith(".json") and (not only or f[:-5] == only):
            with open(os.path.join(CASES, f), encoding="utf-8") as fh:
                out.append(json.load(fh))
    return out


def exp_path(case):
    return os.path.join(EXPECTED, case["name"] + ".json")


def normalize(text):
    pat = os.environ.get("GOLDEN_IGNORE_REGEX")
    return re.sub(pat, "", text) if pat else text


# ------------------------------------------------------------------ 报告

class Report:
    def __init__(self):
        self.fail = 0
        self.warn = 0

    def show(self, case_name, rows):
        for rule, ok, msg in rows:
            tag = "PASS" if ok is True else ("WARN" if ok == "warn" else "FAIL")
            if ok is False:
                self.fail += 1
            elif ok == "warn":
                self.warn += 1
            print(f"  [{tag}] {rule}" + (f" - {msg}" if msg else ""))


def validate_all(case, results):
    rows = []
    for i, r in enumerate(results):
        sub = validate_tryon_prompt(r["prompt"], case)
        pre = f"[{r.get('model_id') or i}] " if len(results) > 1 else ""
        rows += [(pre + a, b, c) for a, b, c in sub]
    if case["endpoint"] == "analyze-batch":
        rows += validate_pair(case, results)
        exp = (case.get("expect") or {}).get("models")
        rows.append(("P0 结果数 = 模特数", len(results) == exp, f"{len(results)}/{exp}"))
    return rows


def cmd_record(args):
    ok, key, ver = sidecar_state()
    if not ok or not key:
        print(f"拒绝录制：{BASE} " + ("不可达" if not ok else "未配置 Key（llm_configured=false），录出来的是降级模板，不能当黄金"))
        return 2
    os.makedirs(EXPECTED, exist_ok=True)
    rc = 0
    for case in load_cases(args.case):
        print(f"== record {case['name']}")
        results = call_case(case)
        if any(r["meta"].get("degraded") for r in results):
            print("  [FAIL] 响应 degraded=true（读图失败降级），不写入")
            rc = 1
            continue
        doc = {"case": case["name"], "endpoint": case["endpoint"], "version": ver,
               "recorded_at": datetime.datetime.now().astimezone().isoformat(timespec="seconds"),
               "locked": False,
               "results": [dict(r, derived=derive(r["prompt"])) for r in results]}
        with open(exp_path(case), "w", encoding="utf-8") as fh:
            json.dump(doc, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
        rep = Report()
        rep.show(case["name"], validate_all(case, results))
        print(f"  已写 {os.path.relpath(exp_path(case), os.path.dirname(HERE))}（locked=false，产品确认后改 true）")
        rc = rc or (1 if rep.fail else 0)
    return rc


def cmd_check(args):
    reach, key, ver = sidecar_state()
    online = reach and key and not args.offline
    print(f"模式：{'在线重跑 + 逐字 diff' if online else '离线结构校验'}"
          f"（sidecar={BASE} 可达={reach} 已配Key={key}{' --offline' if args.offline else ''}）")
    rep = Report()
    cases = load_cases(args.case)
    if not cases:
        print("没有匹配的用例")
        return 1
    for case in cases:
        print(f"== {case['name']}")
        rep.show(case["name"], validate_case_input(case))
        p = exp_path(case)
        if not os.path.exists(p):
            print("  [SKIP] 没有 expected：先在有 Key 的环境运行 `golden_tryon.py record`")
            continue
        with open(p, encoding="utf-8") as fh:
            doc = json.load(fh)
        old = doc.get("results") or []
        rep.show(case["name"], [("E0 expected 的 locked 标记", True if doc.get("locked") else "warn",
                                 "已锁定" if doc.get("locked") else "尚未经产品确认锁定")])
        rep.show(case["name"], validate_all(case, old))
        if online:
            new = call_case(case)
            rows = [("D0 结果数一致", len(new) == len(old), f"{len(new)}/{len(old)}")]
            for i, (o, n) in enumerate(zip(old, new)):
                a, b = normalize(o["prompt"]), normalize(n["prompt"])
                if a == b:
                    rows.append((f"D{i + 1} 逐字一致", True, f"{len(a)} 字"))
                else:
                    k = next((j for j in range(min(len(a), len(b))) if a[j] != b[j]), min(len(a), len(b)))
                    rows.append((f"D{i + 1} 逐字一致", False,
                                 f"首个差异位置 {k}：expected …{a[max(0, k - 20):k + 30]!r} / actual …{b[max(0, k - 20):k + 30]!r}"))
            rep.show(case["name"], rows)
            rep.show(case["name"], [("新结果·" + a, b, c) for a, b, c in validate_all(case, new)])
    print(f"\n汇总：FAIL={rep.fail} WARN={rep.warn}")
    return 1 if rep.fail else 0


def cmd_selftest(args):
    """不联网、不读真实 Key：把 sidecar 的 Key 清空后进程内跑一遍（走无读图的文字模板），用来验证 validator 本身。"""
    spec = importlib.util.spec_from_file_location("serve_reverse_selftest", SERVER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mod.AIG_SECRET = ""   # 清空网关配置 → llm_configured() 为 False，不联网
    mod.AIG_BASE_URL = ""
    print("selftest：sidecar 内存中网关配置已清空，结果是降级模板（degraded=true），仅验证校验器，不是黄金")
    rep = Report()
    for case in load_cases(args.case):
        print(f"== {case['name']}")
        req = json.loads(json.dumps(case["request"]))
        if case["endpoint"] == "analyze-batch":
            rs = mod.analyze_batch(req)["results"]
        else:
            rs = [mod.analyze(req)]
        results = [{"model_id": r.get("model_id", ""), "prompt": r["prompt"], "meta": r.get("meta") or {}} for r in rs]
        rep.show(case["name"], validate_all(case, results))
    print(f"\n汇总：FAIL={rep.fail} WARN={rep.warn}")
    return 1 if rep.fail else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("record", "check", "selftest"):
        p = sub.add_parser(name)
        p.add_argument("--case", help="只跑某个用例（文件名去掉 .json）")
        if name == "check":
            p.add_argument("--offline", action="store_true", help="强制离线，不发 analyze 请求")
    args = ap.parse_args()
    sys.exit({"record": cmd_record, "check": cmd_check, "selftest": cmd_selftest}[args.cmd](args))


if __name__ == "__main__":
    main()
