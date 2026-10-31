#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""prompt-reverse · 内部 sidecar（只做反推，不生成图片）

    python3 -I serve_reverse.py                      # 127.0.0.1:8090
    REVERSE_BIND=0.0.0.0:8090 python3 -I serve_reverse.py

路由（其余一律 404）：
  GET  /api/status
  POST /api/pose/source        POST /api/pose/prompt
  POST /api/reverse/analyze    POST /api/reverse/analyze-batch
  POST /api/reverse/crop-plan  POST /api/reverse/crop-check

来源与合并说明（2026-10-30 交付包）
  基线：serve_reverse-2026-10-30.1.py（商拍：crop-plan / crop-check / analyze-batch / 按件面料）。
  从 serve_reverse-2026-10-30.3.py 并入的姿态差异（共 6 处，商拍部分一律保留 .1）：
    1. _pose_llm_json：读图重试次数 attempts 3 -> 4
    2. _pose_llm_json：重试间隔 sleep(1.5*(n+1)) -> sleep(2.5*(n+1))
    3. 新增常量 POSE_EXPR_STRONG（强表情词表，16 个词）与函数 _expr_strong(text)
    4. build_pose_prompt_short：expr 命中强表情时，在提示词最前插入
       「表情先改：图2的表情是{expr}；不要沿用图1原表情，表情肌要变到同等程度。」
    5. build_pose_prompt（长版）：新增 expr_head =「表情按图2，不要沿用图1；表情变化要明显。」
       （条件：有 ref_expr / ref_mood / detail_text 且 expr_text 命中强表情）
    6. build_pose_prompt（长版）：lines 列表首项加入 expr_head
  相对 .1 删除：静态页面服务与 send_local、/api/billing* 转发、
    嵌入开关、UPSTREAM 及 /api/reverse/* 上游转发（proxy）、/api/reverse/status、lan_ips。
  提示词模板、清洗函数、CATS、读图字段整理逻辑未改动。

模型调用经 ai-gateway（POST /v1/calls，模型组 REVERSE_MODEL_GROUP，默认 vlm.deepseek-flash），不再直连 DeepSeek；
排障指定单一渠道用 REVERSE_ALLOWED_DEPLOYMENTS（逗号分隔 → constraints.allowed_deployments）；
网关 secret（AIG_SECRET）只从环境变量或同目录 .env 读取，不下发、不写日志。客户端见同目录 aig_client.py。
"""
import json
import os
import re
import sys
import threading
import time
import hashlib
from concurrent.futures import ThreadPoolExecutor, as_completed
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

ROOT = os.path.dirname(os.path.abspath(__file__))
VERSION = "2026-10-30.merged"


def load_dotenv():
    path = os.path.join(ROOT, ".env")
    if not os.path.isfile(path):
        return
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                os.environ.setdefault(k.strip(), v.strip().strip('"').strip("'"))
    except OSError:
        pass


load_dotenv()


def _load_aig_client():
    # `python3 -I` 不把脚本目录放进 sys.path，按绝对路径加载同目录的 aig_client.py
    import importlib.util
    spec = importlib.util.spec_from_file_location("aig_client", os.path.join(ROOT, "aig_client.py"))
    mod = importlib.util.module_from_spec(spec)
    sys.modules.setdefault("aig_client", mod)
    spec.loader.exec_module(mod)
    return mod


aig = _load_aig_client()
AigError = aig.AigError

# 模型调用一律经 ai-gateway（DeepSeek 由网关侧渠道提供）；secret 只在环境变量，不下发、不写日志
AIG_BASE_URL = os.environ.get("AIG_BASE_URL", "").strip().rstrip("/")
AIG_KEY_ID = os.environ.get("AIG_KEY_ID", "prompt-reverse-v1").strip()
AIG_SECRET = os.environ.get("AIG_SECRET", "")
AIG_CALLER = os.environ.get("AIG_CALLER", "prompt-reverse").strip()


def _env_int(name, default, lo=None):
    """读整数环境变量；非数字（或小于下限）时回落默认值并打一行警告，不让进程崩溃。"""
    raw = os.environ.get(name, "")
    if raw is None or not str(raw).strip():
        return default
    try:
        v = int(str(raw).strip())
    except ValueError:
        sys.stderr.write("警告：环境变量 %s=%r 不是整数，回落默认值 %s\n" % (name, raw, default))
        return default
    if lo is not None and v < lo:
        sys.stderr.write("警告：环境变量 %s=%r 小于 %s，回落默认值 %s\n" % (name, raw, lo, default))
        return default
    return v


AIG_WAIT_S = _env_int("AIG_WAIT_S", 25)
# 单次 HTTP 请求的总时间预算（秒，从请求开始算）：所有 llm_chat、重试、退避都不得超出
REQUEST_BUDGET_S = _env_int("REVERSE_REQUEST_BUDGET_S", 190, lo=10)
MODEL_GROUP = os.environ.get("REVERSE_MODEL_GROUP", "vlm.deepseek-flash").strip() or "vlm.deepseek-flash"
# 排障：只走指定部署（逗号分隔，如 deepseek-chat.vl.deepseek-flash 或 bailian-chat.vl.deepseek-flash）。
# 非空时写入 constraints.allowed_deployments（ai-gateway contract/calls.py Constraints，最多 64 个）；空 = 组内正常路由。
ALLOWED_DEPLOYMENTS = [d.strip() for d in os.environ.get("REVERSE_ALLOWED_DEPLOYMENTS", "").split(",") if d.strip()]
# 监听地址：默认只听本机；容器内用 REVERSE_BIND=0.0.0.0:8090
BIND = os.environ.get("REVERSE_BIND", "127.0.0.1:8090")

MAX_BODY = 24 * 1024 * 1024  # 最多 9 张压缩图，留足余量
MAX_IMAGES = 9
VISION_MAX_TOKENS = 6000
VISION_TIMEOUT = 180

# ---------------------------------------------------------------- 提示词骨架

GENERAL_COMPLIANCE = (
    "本图为正常电商服装商品图，属于商业产品摄影，不是情色内容。"
    "画面不展示身体敏感部位，不使用透视、湿身、过度紧身或挑逗性姿态；模特姿态自然、表情平静。"
    "最终画面必须是完整穿着状态：只更换服装，不得脱掉模特原有服装导致裸露、未着装或用手遮挡；不要展示换衣过程。"
)

LINGERIE_COMPLIANCE = (
    "本图用于电商平台内衣类目商品详情页，属于正常商业产品摄影，不是情色内容。"
    "最终画面必须是穿着状态：产品完整穿在模特身上，不能出现未穿上衣/未穿下装、裸露胸部或臀部、用手或道具遮挡、脱衣过程；"
    "产品需完整覆盖相应部位，画面不展示身体敏感部位；"
    "不使用透视、湿身、过度紧身勾勒、挑逗性姿态或表情；"
    "遵守主流电商平台内衣类目审核规范。"
    "若构图无法同时满足展示与合规，优先保证合规，改用半身构图。"
    "网纱、蕾丝按参考图还原，但以不透光方式呈现，不出现透出皮肤或身体轮廓的效果。"
)

CATS = {
    "top": {
        "label": "上装",
        "subject": "上装",
        "lingerie": False,
        "task": "电商服装商品图：模特穿着参考图中的上装，用于商品详情页与主图。",
        "lock": "最终画面中，模特上身穿着的就是参考图中的上装，款式、长度与领口不得改动。",
        "basis": "领口、门襟与扣子（仅当图中可见）、胸袋（仅当图中可见）、袖型与袖口、下摆与长度、开衩（仅当图中可见）、褶量与走线，以及颜色与图案",
        "boundary": "上装按参考图完整穿着，长度与下摆位置不得改变；不要自行截短、加长或改成其他品类。最终画面必须是穿着上装的完整状态，不能出现未穿上衣、裸露或脱衣过程。",
        "forbidden": "与参考图不同的外套、马甲、围巾",
        "tail": "不要替换成参考图以外的款式。",
        "frame": "头部至腰部",
    },
    "bottom": {
        "label": "下装",
        "subject": "下装",
        "lingerie": False,
        "task": "电商服装商品图：模特穿着参考图中的下装，用于商品详情页与主图。",
        "lock": "最终画面中，模特下身穿着的是参考图中的下装，裤型或裙型、腰头与长度不得改动。",
        "basis": "腰头、门襟与拉链（仅当图中可见）、口袋（仅当图中可见）、裤型或裙型、长度、裤脚或裙摆、侧缝与走线，以及颜色与图案",
        "boundary": "下装按参考图完整穿着，腰头与裤脚（裙摆）位置不得改变；不要自行截短、加长或换成其他品类。最终画面必须是穿着下装的完整状态，不能出现未穿下装、裸露或脱衣过程。",
        "forbidden": "与参考图不同的短裤、长裤、裙子、连衣裙",
        "tail": "不要替换成参考图以外的款式。",
        "frame": "腰部至脚踝",
    },
    "dress": {
        "label": "连衣裙",
        "subject": "连衣裙",
        "lingerie": False,
        "task": "电商服装商品图：模特穿着参考图中的连衣裙，用于商品详情页与主图。",
        "lock": "最终画面中，模特穿着的就是参考图中的连衣裙，领口、腰线与裙长不得改动。",
        "basis": "领口、腰线与腰头、门襟与扣子（仅当图中可见）、袖型与袖口、裙摆与叠层、开衩（仅当图中可见）、褶量与走线，以及颜色与图案",
        "boundary": "连衣裙按参考图完整穿着，裙长与腰线位置不得改变；不要加长、截短或拆分上下装。最终画面必须是穿着连衣裙的完整状态，不能出现未着装、裸露或脱衣过程。",
        "forbidden": "与参考图不同的外套、腰带、衬裙",
        "tail": "不要替换成参考图以外的款式。",
        "frame": "全身",
    },
    "look": {
        "label": "整套 Look",
        "subject": "整套穿搭",
        "lingerie": False,
        "task": "电商服装商品图：模特同时穿着参考图中的整套穿搭（上装、下装、连衣裙、外套、鞋子、配饰等），用于商品详情页与主图。",
        "lock": (
            "最终画面必须同时出现参考图中的全部单品：不能只穿上装或只穿下装，不能漏掉已上传的外套、鞋子或配饰；"
            "各件的颜色、版型、长度、层叠顺序与搭配关系必须按参考图。"
        ),
        "basis": (
            "逐件还原实际出现的单品：上装的领口/门襟/袖型/下摆，下装的腰头/裤型/长度，"
            "连衣裙的腰线与裙摆，外套的长度与廓形，鞋子的鞋型/鞋跟/鞋面，配饰的材质与位置，"
            "以及整套的配色与材质关系"
        ),
        "boundary": (
            "所有已上传单品同时穿着，按参考图的层叠顺序搭配（外套穿在上装外，鞋子穿在脚上，配饰放在参考图的位置）；"
            "不能替换成其他款式，不能添加参考图以外的单品。最终画面必须是完整穿着状态，不能出现脱衣过程或裸露。"
        ),
        "forbidden": "参考图以外的上衣、裤子、裙子、外套、鞋子",
        "tail": "不要漏掉任何一件已上传的单品；不要添加参考图以外的配饰或用其他款式替代。",
        "frame": "全身",
    },
    "bra": {
        "label": "文胸",
        "subject": "文胸（胸罩）",
        "lingerie": True,
        "task": "电商内衣类目商品图：模特仅穿着参考图中的文胸，用于商品详情页与主图。",
        "lock": (
            "最终画面中，模特上身只能穿文胸（胸罩），不能出现袖子、衣身或下摆；"
            "文胸下沿止于胸下围，不得向下延长成上衣或连体衣。只允许文胸自带的肩带（宽度以参考图为准），肩带不能延展成吊带衫的衣身；肩带以外不出现大面积裸露。"
        ),
        "basis": "杯型与包覆范围、肩带（宽度、位置、是否可拆）、侧比宽度、钢圈或无钢圈、后背搭扣或绑带、蕾丝/网纱/印花（仅当图中可见），以及颜色与光泽；文胸只有杯面、肩带、侧比、下围与背扣，没有领口、袖口、衣身或下摆",
        "boundary": (
            "文胸只覆盖胸部（胸下围以上），无袖、无衣身、无下摆，不延伸到腰部。"
            "模特原有的上衣必须替换为参考图中的文胸，不能残留衬衫、毛衣、卫衣、吊带、背心、T恤、打底衫或任何长袖/短袖；"
            "最终画面必须是穿着文胸的完整状态，不能出现未穿上衣、裸露或脱衣过程。"
        ),
        "forbidden": "吊带衫、吊带裙、背心、打底衫、T恤、衬衫、长袖、连体衣、泳衣、睡衣、束腰、外套、披肩",
        "tail": "不要给模特添加任何外套、披肩或开衫；肩带以外不要出现袖子。",
        "frame": "头部至腰部",
    },
    "panties": {
        "label": "内裤",
        "subject": "内裤",
        "lingerie": True,
        "task": "电商内衣类目商品图：模特仅穿着参考图中的内裤，用于商品详情页与主图。",
        "lock": (
            "最终画面中，模特下身只能穿参考图中的内裤款式：三角裤、平角裤、丁字裤/系带款均以参考图为准，"
            "不能出现裤腿或裤脚；内裤上沿止于腰部，不得向上延长成连体衣。"
        ),
        "basis": "腰头高度与宽度、包覆范围（三角/平角/丁字/系带）、侧边宽度、裆部结构、蕾丝/包边（仅当图中可见）、走线与面料，以及颜色与图案；内裤只有腰头、裆部、侧边与裤口，没有领口、袖口或下摆",
        "boundary": (
            "内裤只覆盖腰胯与裆部：无裤腿、无裤脚，不延伸至大腿或小腿。"
            "模特原有的下装必须替换为参考图中的内裤，不能残留长裤、短裤、半身裙、打底裤或连体衣；"
            "最终画面必须是穿着内裤的完整状态，不能出现未穿下装、裸露或脱衣过程。"
            "画面只取腰部至膝盖，上沿不高于腰线，上身不入镜。"
        ),
        "forbidden": "短裤、长裤、打底裤、泳裤、束腰、连体衣、睡衣、半身裙",
        "tail": "不要把内裤延长成短裤或打底裤；不要给模特添加裙子或外裤。",
        "frame": "腰部至膝盖",
    },
    "set": {
        "label": "文胸内裤两件套",
        "subject": "文胸与内裤两件套",
        "lingerie": True,
        "task": "电商内衣类目商品图：模特同时穿着参考图中的文胸与内裤，用于两件套商品详情页与主图。",
        "lock": (
            "最终画面由文胸与内裤两件组成，不能连成一件；"
            "上身不能出现袖子、衣身或下摆，下身不能出现裤腿或裤脚。"
            "文胸与内裤分别对应各自的参考图，不得把文胸的杯型、肩带、钢圈套用到内裤，"
            "也不得把内裤的腰头、包覆套用到文胸；只替换这两件，不新增吊袜带、睡袍、外搭；"
            "未选中的包、鞋子、配饰保持原样。"
        ),
        "basis": "两件为同一系列，颜色一致，蕾丝/包边/五金（仅当图中可见）一致；文胸的杯型、肩带、侧比与背扣，内裤的腰头、包覆与侧边；文胸没有领口/衣身/下摆，内裤没有裤腿/下摆",
        "boundary": (
            "上身为文胸（只覆盖胸部，无袖、无衣身、无下摆，不延伸至腰部），"
            "下身为内裤（只覆盖腰胯与裆部，无裤腿、无裤脚，不延伸至大腿）；"
            "两件之间露出腰腹属于正常商品展示。模特原有的上下装必须替换为参考图中的文胸与内裤，不能残留原有衣物；"
            "最终画面必须是两件都穿好的完整状态，不能出现未穿、裸露或脱衣过程。"
        ),
        "forbidden": "连体衣、睡裙、吊带裙、吊带衫、背心、T恤、衬衫、长袖、短裤、长裤、打底裤、泳衣、睡衣",
        "tail": "不要把两件连成连体衣或裙装；不要给模特添加外套、裙子或外裤。",
        "frame": "头部至大腿",
    },
    "outer": {
        "label": "外套",
        "subject": "外套",
        "lingerie": False,
        "task": "电商服装商品图：模特穿着参考图中的外套，用于商品详情页与主图。",
        "lock": "只替换外套；内搭、下装、鞋子、包、配饰保持原样；外套的款式、长度、领型不得改动。",
        "basis": "领口、门襟与扣子（仅当图中可见）、袖型与袖口、长度与廓形、下摆、口袋（仅当图中可见）、里料（仅当可见）、面料纹理",
        "boundary": "外套按参考图穿着；内搭、下装、鞋子、包、配饰保持原样；最终画面必须是穿着外套的完整状态，不能出现未穿上衣、裸露或脱衣过程。",
        "forbidden": "与参考图不同的内搭、下装、鞋子、包",
        "tail": "不要替换内搭/下装/鞋子/包；不要改变姿势。",
        "frame": "头部至大腿",
    },
    "shoes": {
        "label": "鞋子",
        "subject": "鞋子",
        "lingerie": False,
        "task": "电商鞋履商品图：模特穿着参考图中的鞋子，用于商品详情页与主图。",
        "lock": "只替换鞋子；上装、下装、包、配饰、姿势保持原样；鞋型、鞋跟、鞋面不得改动。",
        "basis": "鞋型、鞋头、鞋面、鞋跟、鞋底、绑带/五金（仅当图中可见）、筒高（仅当可见）、颜色与材质",
        "boundary": "鞋子按参考图穿在脚上；上装、下装、包、配饰保持原样；最终画面必须是完整穿着状态，不能出现未穿鞋/脱鞋过程。",
        "forbidden": "与参考图不同的鞋子、袜子（参考图有袜子时除外）",
        "tail": "不要替换衣服/包/配饰；不要改变站姿。",
        "frame": "全身（含鞋子）",
    },
    "accessory": {
        "label": "配饰",
        "subject": "配饰",
        "lingerie": False,
        "task": "电商配饰商品图：模特佩戴或携带参考图中的配饰，用于商品详情页与主图。",
        "lock": "只替换配饰；服装、鞋子、非目标包、姿势保持原样；配饰的品类、数量、佩戴位置不得改动。",
        "basis": "品类、材质、尺寸、五金、颜色、佩戴位置/方式",
        "boundary": "配饰按参考图佩戴/携带；其他服装、鞋子、姿势保持原样；最终画面必须是完整穿着状态。",
        "forbidden": "与参考图不同的配饰或额外配饰",
        "tail": "不要添加参考图以外的配饰；不要改变服装/姿势。",
        "frame": "半身或全身",
    },
    "compose": {
        "label": "组合替换",
        "subject": "组合单品",
        "lingerie": False,
        "task": "电商服装商品图：只把选中的单品穿到模特身上，用于商品详情页与主图。",
        "lock": "只替换选中的单品；未选中的服装、鞋子、配饰、包、姿势、背景保持原样。",
        "basis": "逐件还原选中的单品：领口、袖型、腰头、裤型/裙长、鞋型、配饰位置、面料与颜色",
        "boundary": "只替换选中的单品；未选中的单品与姿势保持原样；最终画面必须是完整穿着状态，不能出现未穿、裸露或脱衣过程。",
        "forbidden": "与参考图不同的目标单品款式",
        "tail": "不要新增或替换未选中的单品，不要改动它们的颜色、位置、拿法、姿势或背景。",
        "frame": "全身",
    },
}

ITEM_LABELS = {
    "top": "上衣", "bottom": "下装", "dress": "连衣裙", "outer": "外套",
    "shoes": "鞋子", "accessory": "配饰", "bra": "文胸", "panties": "内裤",
}
ITEM_LOCKS = {
    "top": "只替换上衣；未选中的其他单品、包、姿势保持原样；上衣长度/袖长按锁定",
    "bottom": "只替换下装；未选中的其他单品、包、姿势保持原样；下装长度/腰头按锁定",
    "dress": "只替换连衣裙；未选中的其他单品、包、姿势保持原样；连衣裙覆盖上下装，不能拆成两件",
    "outer": "只替换外套；未选中的其他单品、包、姿势保持原样；外套长度/廓形按锁定",
    "shoes": "只替换鞋子；未选中的其他单品、包、姿势保持原样；鞋型/鞋跟/鞋面按参考图",
    "accessory": "只替换配饰；未选中的其他单品、包、姿势保持原样；配饰数量/位置按参考图",
    "bra": "只替换文胸；胸部区域原有上装替换为文胸；未选中的其他单品、包保持原样；最终穿着状态、不裸露",
    "panties": "只替换内裤；腰胯区域原有下装替换为内裤；未选中的其他单品、包保持原样；最终穿着状态、不裸露",
}
ITEM_BASIS = {k: CATS[k]["basis"] for k in ITEM_LABELS if k in CATS}


def _item_keys(payload):
    keys = [k for k in (payload.get("items") or []) if k in ITEM_LABELS]
    if keys:
        return keys
    if (payload.get("category") or "") == "set":
        roles = {str(im.get("role") or "") for im in payload.get("images") or []}
        if roles & {"文胸", "内裤", "文胸内裤套装", "套装", "bra", "panty", "panties"}:
            return ["bra", "panties"]
    return keys


def _compose_frame(keys):
    if "shoes" in keys:
        return "全身（含鞋子）"
    if "dress" in keys:
        return "全身"
    if "bottom" in keys:
        return "腰部至脚踝"
    if "top" in keys or "outer" in keys:
        return "头部至腰部"
    if "bra" in keys:
        return "头部至腰部"
    if "panties" in keys:
        return "腰部至膝盖"
    return "半身或全身"


def _compose_cat(keys):
    labels = [ITEM_LABELS[k] for k in keys if k in ITEM_LABELS]
    lingerie = any(k in ("bra", "panties") for k in keys)
    basis = "；".join(f"{ITEM_LABELS[k]}的{ITEM_BASIS[k]}" for k in keys if k in ITEM_BASIS)
    task = f"电商服装商品图：只把参考图中的{'、'.join(labels)}穿到模特身上，用于商品详情页与主图。"
    lock = (f"最终画面只替换{'、'.join(labels)}；未选中的服装、鞋子、配饰、包、姿势、背景、光线、机位、构图全部保持与参考模特图一致。")
    boundary = (f"只替换{'、'.join(labels)}；未选中的单品与姿势保持原样；"
                "最终画面必须是完整穿着状态，不能出现未穿、裸露或脱衣过程。")
    return {
        "label": "组合替换", "subject": "组合单品（" + "、".join(labels) + "）",
        "lingerie": lingerie, "task": task, "lock": lock,
        "basis": basis or CATS["compose"]["basis"], "boundary": boundary,
        "forbidden": "与参考图不同的目标单品款式",
        "tail": "不要新增或替换未选中的单品，不要改动它们的颜色、位置、拿法、姿势或背景。", "frame": _compose_frame(keys),
        "compose": True, "item_keys": keys, "labels": labels,
    }

SYSTEM_PROMPT = """你是资深内衣与服装电商视觉策划、商品摄影指导和图像生成提示词工程师。
你的任务：把用户给出的品类、面料、颜色、补充说明，整理成可直接交给图像生成模型的中文提示词素材。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。
2. 内容用于正常电商商品拍摄，不是情色内容。不得出现性暗示、挑逗性姿势、透视、湿身、过度紧身、肤色或身体敏感部位的描述。
3. 不得编造参考图中不存在的款式结构（杯型、肩带、钢圈、搭扣、裤型、腰头等）。这些一律以参考图为准。
4. 不得改变服装品类：文胸不是上衣、吊带衫、背心或连体衣；内裤不是短裤、打底裤或泳裤；两件套不能写成连体衣或裙装。
5. 语言专业、简洁、可执行；每项 1–2 句。
6. 信息不足时写“以参考图为准”，不要猜测。自动提取的主色可能有背景误差，只在没有明确颜色说明时参考。
7. styling 必须是“模特穿着展示”的自然站姿，不要建议平铺、人台、挂拍或只拍产品。
8. details 只能使用补充说明里明确写到的部件；补充说明没写到的部件（杯型、钢圈、肩带、裤型、腰头等）一律不要列举，统一写“其余部件以参考图为准”。

输出 JSON 字段：
{
  "material": "面料质感、颜色、图案；没有明确信息就写“颜色与图案以参考图为准”",
  "details": "一句，提示生成模型按参考图核对还原哪些部件，不指定具体款式",
  "styling": "一句，自然的站姿与镜头角度，不得有挑逗性",
  "light": "一句，光线与背景",
  "negatives": ["0–3 条该品类最容易误生成的服装品类"]
}"""


VISION_SYSTEM_PROMPT = """你是内衣/服装电商的商品图分析员。你只分析服装本身，不描述身体、肤色或敏感部位。根据用户上传的服装参考图、局部细节图和人物模特图，提取能直接用于图像生成提示词的事实，输出 JSON。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。
2. 只写图中看得见的结构；看不清的肩带宽度、开衩、门襟、扣子、领型、后背育克、内衬等，一律写“以参考图为准”，不得凭品类常识补写或猜测。
3. garment_match 只判断大品类：上装 / 下装 / 连衣裙 / 整套 Look / 文胸 / 内裤 / 两件套 / 多选组合（最多3件）。具体款式不等于品类不符：针织毛衣、衬衫、T恤、卫衣都属于上装；牛仔裤、半身裙都属于下装。整套 Look 至少要两件单品；多选组合要检查用户选中的 1–3 件单品是否都在服装参考图里，齐全写 true，缺件写 false 并在 garment_type 写缺什么。只有大类确实不符时才写 false。
4. structure 写该品类的关键部件：文胸（杯型/肩带/侧比/钢圈/背扣/蕾丝）、内裤（腰头/包覆/侧边/裆部/包边）、上装（领口/门襟/袖型与袖口/下摆）、下装（腰头/裤型/裤脚）、连衣裙（领口/腰线/裙摆）。上装、下装、连衣裙、外套必须写清袖长（长袖/七分袖/短袖/无袖）和长度（及胯/及腰/及膝/及踝/及地）；看不出来就写“以参考图为准”，不要猜。领口必须如实单选（高领/半高领/圆领/挂脖/一字肩/方领/V领/翻领），不得混写；无袖不等于露肩。文胸的下围不要写成下摆，杯口不要写成领口，杯身不要写成衣身；内裤的裤口不要写成下摆。两件套分别读图：文胸部件只写文胸，内裤部件只写内裤，不得互相套用；看不见的侧比、背扣、五金不要编造装饰。只写看到的。两件套用“文胸：…”“内裤：…”前缀；整套 Look 和多选组合都要逐件加前缀（上装/上衣、下装、连衣裙、外套、鞋子、配饰、文胸、内裤），只写上传/选中的单品，不要编造没上传/没选的；多选组合还要按单品填写 item_lengths、item_sleeves、item_design_details。
5. model_outfit 只写「人物模特」图里人物身上实际穿着的服装（品类、颜色、袖长、领口）。即使服装参考图里有人穿着衣服，那个人也不是本次模特，不能把她的衣服写进 model_outfit。没有人物模特图就留空。不写“必须移除”“需要替换”这类指令，不描述身体、肤色或敏感部位。
6. garment_colors、material、pattern、structure 只能来自服装参考图（role 不是「人物模特」的图）；人物模特图里的衣服颜色、材质、结构不得写入这些字段。颜色用中文色名，最多 3 个主色。多选组合时 material 必须按单品前缀写，例如“上衣：棉质府绸；下装：涤纶；鞋子：皮革”，方便把皮革/材质归到具体单品。
7. negatives 只写该品类最容易误生成的服装，0–4 条。
8. 语言简洁、专业。
9. 内容用于正常电商商品分析，不是情色内容。
10. proportions 只写尺度无关的比例或身体落点：腰线在总长百分之几、下摆宽/肩宽、袖长/臂长、衣长/肩宽、裤长/腰宽、裤脚宽/腰宽。没有模特时不要假装知道绝对厘米，用 length_class 表达。
11. length_class 按品类：上装（及腰/及胯/遮臀/长款）、下装（短裤/五分/七分/九分/长裤；短裙/中裙/长裙）、连衣裙（膝上/及膝/小腿中部/及踝/及地）、外套（短款/中长/长款）。sleeve_class：无袖/短袖/五分/七分/长袖；看不清写“以参考图为准”。
12. design_details 列出所有有辨识度的设计元素：不对称、开衩、镂空、绑带、抽绳、褶皱、拼接、撞色、刺绣、印花、五金、异形下摆/领口/袖口、可拆卸部件等；写清位置、方向、数量、大小比例。不要把设计元素当成瑕疵、背景、其他服装或需要修正的地方；不要为了简洁省略。衣服自带的腰带/绑带/胸针/五金/刺绣/拼接要标为自带设计，换装时不得移除。至少写 1 条；确实没有特殊设计就写“无明显特殊设计”。
13. 多选组合必须按选中的每件单品归位：structure 用“上衣：…”“下装：…”“鞋子：…”前缀；item_lengths / item_sleeves 用中文单品名做键；item_design_details 也按单品分组。没有该单品的数据就留空，不要把一件的部件写到另一件上。
14. 同一件衣服可能有多张不同角度的参考图（正面/背面/侧面/平铺）。把它们当成同一件：structure 和 design_details 合并去重；正面优先确认主结构，背面补后领、育克、后背拼接、后腰、后片，侧面补侧缝、开衩、袖型、廓形；不要写成多件，也不要因为背面看不到就删掉正面已确认的部件。
15. background 只来自「人物模特」图：写背景类型、主要颜色/材质、可见道具/家具/墙体/地面，以及景深/虚化程度；不要写“纯色/白底”，不要从服装参考图取背景。没有人物模特图就留空。

输出 JSON 字段：
{
  "garment_match": true,
  "garment_type": "实际品类",
  "garment_colors": ["服装参考图主色 1–3 个"],
  "material": "服装参考图可见面料质感",
  "pattern": "服装参考图图案/肌理",
  "structure": ["服装参考图看得见的结构，最多 8 条"],
  "proportions": ["尺度无关的比例，例如：腰线在总长 42%、下摆宽/肩宽 2.1、袖长到手腕"],
  "length_class": "按品类给出长度档位；看不清写以参考图为准",
  "sleeve_class": "无袖/短袖/五分/七分/长袖；看不清写以参考图为准",
  "design_details": ["有辨识度的设计元素，例如：右前下摆比左侧高约 8cm，斜切；左肩金属 D 环 + 织带"],
  "item_lengths": {"上衣": "及胯/短裙/…；没有则留空"},
  "item_sleeves": {"上衣": "长袖/无袖/…；没有则留空"},
  "item_design_details": {"上衣": ["…"], "下装": ["…"]},
  "model_outfit": "人物模特图里实际穿着的服装；没有模特图就留空",
  "pose": "只写站姿与镜头角度（如正面站立、微侧 3/4、平视），不写取景范围、手部动作、道具、场景",
  "background": "人物模特图的背景类型、颜色、材质、道具、墙面/地面、景深；只来自模特图",
  "light": "只写光线（如柔和均匀、侧光），不写户外/室内或背景",
  "negatives": ["0–4 条"]
}"""


CROP_SYSTEM_PROMPT = """你是服装电商的视觉关键点标注员。用户给你一张或几张服装图，你要标出可用于裁切细节的关键点。
你只看服装本身，不描述身体、肤色或敏感部位。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。
2. 每个关键点必须是 [x, y] 两个归一化数字（0–1），x 向右、y 向下；看不清就写 null，不要猜。
3. 每条 plan 给 angle 和 keypoints；多张图分别输出，image_index 从 0 开始。
4. 同一件衣服的不同角度分别标注各自看得见的关键点；不要写成多件。
5. 不要标背景、人脸、空白；关键点落在服装部件上。
6. garment_box 是服装主体框，[x,y,w,h] 归一化；看不清可省略。

输出 JSON：
{
  "plans": [
    {
      "image_index": 0,
      "angle": "front",
      "garment_box": [0.10, 0.08, 0.80, 0.86],
      "keypoints": {
        "neck": [0.45, 0.22],
        "waist_center": [0.48, 0.48],
        "hem_left": [0.22, 0.86],
        "hem_right": [0.72, 0.86]
      }
    }
  ]
}"""

CROP_KEYPOINTS = {
    "top": ("neck", "left_shoulder", "right_shoulder", "left_cuff", "right_cuff",
            "hem_left", "hem_right", "waist_center", "fabric_center"),
    "bottom": ("waist_left", "waist_right", "hem_left", "hem_right",
               "placket_top", "placket_bottom", "pocket_left", "pocket_right",
               "fabric_center"),
    "dress": ("neck", "waist_center", "hem_left", "hem_right",
              "left_cuff", "right_cuff", "slit", "fabric_center"),
    "outer": ("neck", "waist_center", "hem_left", "hem_right",
              "left_cuff", "right_cuff", "fabric_center"),
    "shoes": ("toe", "heel", "ankle", "vamp_center", "sole_left", "sole_right",
              "fabric_center"),
    "accessory": ("top_left", "top_right", "bottom_left", "bottom_right",
                  "handle_top", "hardware_center", "fabric_center"),
    "bra": ("cup_left", "cup_right", "band_left", "band_right",
            "strap_top_left", "strap_top_right", "back_center", "fabric_center"),
    "panties": ("waist_left", "waist_right", "gusset_center",
                "leg_left", "leg_right", "back_center", "fabric_center"),
    "set": ("cup_left", "cup_right", "band_left", "band_right", "back_center",
            "waist_left", "waist_right", "gusset_center", "leg_left", "leg_right",
            "fabric_center"),
}


def _kp_point(value):
    if not isinstance(value, (list, tuple)) or len(value) < 2:
        return None
    try:
        x, y = float(value[0]), float(value[1])
    except (TypeError, ValueError):
        return None
    if not (0 <= x <= 1 and 0 <= y <= 1):
        return None
    return (x, y)


def _kp_box(point, w, h):
    if not point:
        return None
    x = max(0.0, min(1.0, point[0] - w / 2))
    y = max(0.0, min(1.0, point[1] - h / 2))
    x2 = min(1.0, x + w)
    y2 = min(1.0, y + h)
    return _norm_crop_box([x, y, x2 - x, y2 - y])


def _kp_span(p1, p2, pad_x=0.08, pad_y=0.08):
    if not p1 or not p2:
        return None
    x0 = max(0.0, min(p1[0], p2[0]) - pad_x)
    y0 = max(0.0, min(p1[1], p2[1]) - pad_y)
    x1 = min(1.0, max(p1[0], p2[0]) + pad_x)
    y1 = min(1.0, max(p1[1], p2[1]) + pad_y)
    return _norm_crop_box([x0, y0, x1 - x0, y1 - y0])


def _derive_crops(cat_key, kp, angle, garment_box=None):
    kp = {str(k).lower(): _kp_point(v) for k, v in (kp or {}).items()}
    back = angle == "back"
    crops = []

    def add(label, box):
        if box and all(label != c["label"] for c in crops):
            crops.append({"label": label, "box": box, "confidence": 0.86})

    if cat_key in ("top", "outer", "dress"):
        neck = kp.get("neck")
        waist = kp.get("waist_center")
        collar = neck
        if garment_box:
            # 人体 neck 常常落在脖子/脸上；领口/翻领应跟服装主体框的上沿走
            collar_y = garment_box[1] + (0.18 if cat_key == "outer" else 0.06)
            collar = (garment_box[0] + garment_box[2] / 2,
                      max(neck[1] if neck else 0.0, collar_y))
        add("后领" if back else "领口",
            _kp_box(collar, 0.30 if cat_key == "outer" else 0.22, 0.15))
        add("后腰" if back else "腰线", _kp_box(waist, 0.36, 0.14))
        if collar and waist:
            cx = (collar[0] + waist[0]) / 2
            y0 = min(collar[1], waist[1])
            y1 = max(collar[1], waist[1])
            add("后背" if back else "门襟", _kp_box((cx, (y0 + y1) / 2), 0.18, max(0.16, y1 - y0 + 0.10)))
        add("下摆", _kp_span(kp.get("hem_left"), kp.get("hem_right"), 0.06, 0.05))
        for side, key in (("左", "left_cuff"), ("右", "right_cuff")):
            if kp.get(key):
                add(side + "袖口", _kp_box(kp[key], 0.20, 0.16))
        if kp.get("left_shoulder") and kp.get("right_shoulder"):
            add("后育克" if back else "肩部", _kp_span(kp["left_shoulder"], kp["right_shoulder"], 0.05, 0.05))
        if cat_key == "dress" and kp.get("slit"):
            add("开衩", _kp_box(kp["slit"], 0.20, 0.30))
        center = kp.get("fabric_center") or neck or waist or kp.get("hem_left")
        add("面料纹理", _kp_box(center, 0.26, 0.26))
    elif cat_key == "bottom":
        add("腰头", _kp_span(kp.get("waist_left"), kp.get("waist_right"), 0.05, 0.05))
        add("门襟与拉链", _kp_span(kp.get("placket_top"), kp.get("placket_bottom"), 0.06, 0.05))
        for side, key in (("左", "pocket_left"), ("右", "pocket_right")):
            if kp.get(key):
                add(side + "前袋", _kp_box(kp[key], 0.20, 0.20))
        add("裤脚/裙摆", _kp_span(kp.get("hem_left"), kp.get("hem_right"), 0.06, 0.05))
        add("面料纹理", _kp_box(kp.get("fabric_center") or kp.get("gusset_center") or kp.get("waist_left"), 0.26, 0.26))
    elif cat_key == "shoes":
        add("鞋头", _kp_box(kp.get("toe"), 0.24, 0.20))
        add("鞋跟", _kp_box(kp.get("heel"), 0.22, 0.24))
        add("鞋面", _kp_box(kp.get("vamp_center"), 0.30, 0.22))
        add("鞋底", _kp_span(kp.get("sole_left"), kp.get("sole_right"), 0.05, 0.04))
        add("绑带/五金", _kp_box(kp.get("ankle"), 0.22, 0.22))
        add("材质", _kp_box(kp.get("fabric_center") or kp.get("vamp_center"), 0.24, 0.24))
    elif cat_key == "accessory":
        add("包口", _kp_span(kp.get("top_left"), kp.get("top_right"), 0.05, 0.04))
        add("肩带", _kp_box(kp.get("handle_top"), 0.26, 0.26))
        add("五金", _kp_box(kp.get("hardware_center"), 0.22, 0.22))
        add("底部", _kp_span(kp.get("bottom_left"), kp.get("bottom_right"), 0.05, 0.04))
        add("材质纹理", _kp_box(kp.get("fabric_center") or kp.get("hardware_center") or kp.get("handle_top"), 0.28, 0.28))
    elif cat_key == "bra":
        if kp.get("cup_left"):
            add("左杯型", _kp_box(kp["cup_left"], 0.30, 0.26))
        if kp.get("cup_right"):
            add("右杯型", _kp_box(kp["cup_right"], 0.30, 0.26))
        if not (kp.get("cup_left") or kp.get("cup_right")):
            add("杯型", _kp_span(kp.get("cup_left"), kp.get("cup_right"), 0.06, 0.06))
        add("下围", _kp_span(kp.get("band_left"), kp.get("band_right"), 0.04, 0.03))
        for side, key in (("左", "strap_top_left"), ("右", "strap_top_right")):
            if kp.get(key):
                add(side + "肩带", _kp_box(kp[key], 0.18, 0.20))
        if not (kp.get("strap_top_left") or kp.get("strap_top_right")):
            add("肩带", _kp_box(None, 0.22, 0.22))
        fabric = kp.get("fabric_center")
        if not fabric and kp.get("cup_left") and kp.get("cup_right"):
            fabric = ((kp["cup_left"][0] + kp["cup_right"][0]) / 2,
                      (kp["cup_left"][1] + kp["cup_right"][1]) / 2)
        add("面料/蕾丝", _kp_box(fabric, 0.28, 0.24))
        add("背扣", _kp_box(kp.get("back_center"), 0.22, 0.18))
    elif cat_key == "panties":
        add("腰头", _kp_span(kp.get("waist_left"), kp.get("waist_right"), 0.05, 0.04))
        add("包覆", _kp_box(kp.get("gusset_center"), 0.28, 0.22))
        if kp.get("leg_left"):
            add("左侧边", _kp_box(kp["leg_left"], 0.18, 0.20))
        if kp.get("leg_right"):
            add("右侧边", _kp_box(kp["leg_right"], 0.18, 0.20))
        if not (kp.get("leg_left") or kp.get("leg_right")):
            add("侧边", _kp_box(None, 0.20, 0.22))
        add("后片", _kp_box(kp.get("back_center"), 0.30, 0.28))
        add("面料/蕾丝", _kp_box(kp.get("fabric_center") or kp.get("gusset_center"), 0.28, 0.24))
    elif cat_key == "set":
        for side, key in (("左", "cup_left"), ("右", "cup_right")):
            if kp.get(key):
                add("文胸·" + side + "杯型", _kp_box(kp[key], 0.28, 0.24))
        add("文胸·下围", _kp_span(kp.get("band_left"), kp.get("band_right"), 0.04, 0.03))
        add("文胸·背扣", _kp_box(kp.get("back_center"), 0.22, 0.18))
        add("内裤·腰头", _kp_span(kp.get("waist_left"), kp.get("waist_right"), 0.05, 0.04))
        add("内裤·包覆", _kp_box(kp.get("gusset_center"), 0.28, 0.22))
        add("内裤·侧边", _kp_box(kp.get("leg_left") or kp.get("leg_right"), 0.20, 0.22))
        add("面料/蕾丝", _kp_box(kp.get("fabric_center") or kp.get("gusset_center"), 0.28, 0.24))
    if angle == "side" and not crops:
        add("侧身结构", _kp_box(kp.get("waist_center") or kp.get("neck"), 0.40, 0.60))
    return crops[:6]


CROP_CANDIDATE_SYSTEM_PROMPT = """你是服装电商的视觉标注员。用户给你每张服装图 + 对应的候选细节表（C1、C2…），候选表里每个格子是从原图裁出的一个区域。你要为每个细节挑选最合适的候选，不要自己写坐标。
你只看服装本身，不描述身体、肤色或敏感部位。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。
2. 每个细节用 candidate 指定候选 ID（必须来自该图的候选表），例如 "C3"；不要写像素坐标。
3. 每张图给 2–4 个最有信息量的细节；不要选整件服装、纯背景、人脸、空白、模特身体。目标部件要尽量在候选画面中央、完整可见，不要选只露出一角或大部分是背景的候选。
4. 只选看得见的细节；看不清就不要选，不要凭常识补。同一细节只选一次，不要重复。
5. angle 取值：front / back / side / detail / flat / unknown；多张图分别判断。
6. crops 的 label 用中文部位名；多选组合时用“上衣·领口”“下装·腰头”这种前缀。
7. confidence 是 0–1 的置信度；低于 0.4 不要输出。

输出 JSON：
{
  "plans": [
    {
      "image_index": 0,
      "angle": "front",
      "crops": [
        {"label": "领口", "candidate": "C3", "confidence": 0.88},
        {"label": "袖口", "candidate": "C10", "confidence": 0.82}
      ]
    }
  ]
}"""

CROP_PARTS = {
    "top": ("领口", "门襟与扣子", "胸袋", "袖口", "下摆", "肩部/育克", "面料纹理"),
    "bottom": ("腰头", "门襟与拉链", "前袋", "裤脚/裙摆", "侧缝", "后袋", "面料纹理"),
    "dress": ("领口", "腰线", "门襟与扣子", "裙摆", "开衩", "袖口", "后背", "面料纹理"),
    "outer": ("领口", "门襟", "袖口", "下摆", "肩部", "里料", "五金", "面料纹理"),
    "shoes": ("鞋头", "鞋面", "鞋跟", "鞋底", "绑带/五金", "筒高"),
    "accessory": ("包口", "肩带", "五金", "材质", "底部", "细节"),
    "bra": ("杯型", "肩带", "下围", "侧比", "背扣", "蕾丝/网纱"),
    "panties": ("腰头", "包覆", "侧边", "裤口", "蕾丝/包边", "后片"),
    "set": ("文胸·杯型", "文胸·肩带", "文胸·背扣", "内裤·腰头", "内裤·包覆", "内裤·蕾丝"),
    "look": ("上装·领口", "下装·腰头", "连衣裙·裙摆", "外套·门襟", "鞋子·鞋面", "配饰·五金", "面料纹理"),
    "compose": ("整体", "领口/腰头", "鞋面/五金", "面料", "细节", "搭配"),
}

# 模型偶发返回空关键点时的确定性兜底；只保证每件都有可裁的细节，不替代正常定位。
FALLBACK_CROPS = {
    "top": [
        ("领口", [0.30, 0.14, 0.40, 0.18]),
        ("门襟", [0.40, 0.25, 0.20, 0.36]),
        ("下摆", [0.25, 0.68, 0.50, 0.14]),
        ("面料纹理", [0.35, 0.38, 0.30, 0.24]),
    ],
    "bottom": [
        ("腰头", [0.25, 0.20, 0.50, 0.14]),
        ("门襟与拉链", [0.40, 0.30, 0.20, 0.26]),
        ("裤脚/裙摆", [0.25, 0.70, 0.50, 0.14]),
        ("面料纹理", [0.35, 0.42, 0.30, 0.22]),
    ],
    "dress": [
        ("领口", [0.30, 0.10, 0.40, 0.18]),
        ("腰线", [0.28, 0.40, 0.44, 0.14]),
        ("裙摆", [0.20, 0.74, 0.60, 0.14]),
        ("面料纹理", [0.35, 0.45, 0.30, 0.24]),
    ],
    "outer": [
        ("领口", [0.28, 0.12, 0.44, 0.20]),
        ("门襟", [0.40, 0.26, 0.20, 0.38]),
        ("下摆", [0.22, 0.70, 0.56, 0.14]),
        ("面料纹理", [0.35, 0.40, 0.30, 0.24]),
    ],
    "shoes": [
        ("鞋头", [0.05, 0.50, 0.28, 0.20]),
        ("鞋面", [0.30, 0.45, 0.40, 0.22]),
        ("鞋跟", [0.68, 0.60, 0.25, 0.25]),
        ("材质", [0.35, 0.55, 0.25, 0.20]),
    ],
    "accessory": [
        ("包口", [0.25, 0.20, 0.50, 0.14]),
        ("肩带", [0.25, 0.10, 0.50, 0.20]),
        ("底部", [0.30, 0.70, 0.40, 0.14]),
        ("材质纹理", [0.35, 0.40, 0.30, 0.22]),
    ],
    "bra": [
        ("左杯型", [0.18, 0.30, 0.30, 0.26]),
        ("右杯型", [0.52, 0.30, 0.30, 0.26]),
        ("下围", [0.20, 0.60, 0.60, 0.12]),
        ("面料/蕾丝", [0.40, 0.38, 0.20, 0.20]),
    ],
    "panties": [
        ("腰头", [0.20, 0.20, 0.60, 0.12]),
        ("包覆", [0.36, 0.45, 0.28, 0.25]),
        ("侧边", [0.15, 0.35, 0.20, 0.25]),
        ("面料/蕾丝", [0.40, 0.40, 0.20, 0.20]),
    ],
    "set": [
        ("文胸·杯型", [0.20, 0.15, 0.30, 0.22]),
        ("文胸·下围", [0.22, 0.38, 0.56, 0.10]),
        ("内裤·腰头", [0.22, 0.58, 0.56, 0.10]),
        ("内裤·包覆", [0.38, 0.70, 0.24, 0.20]),
    ],
}
FALLBACK_CROPS["top"] += [("袖口", [0.08, 0.45, 0.22, 0.18])]


def _fallback_crops(cat_key, angle="front"):
    items = FALLBACK_CROPS.get(cat_key) or FALLBACK_CROPS["top"]
    out = []
    for label, box in items:
        b = _norm_crop_box(box)
        if b:
            out.append({"label": label, "box": b, "confidence": 0.4})
    return out


def _ensure_crop_plans(res, images, cat_key):
    """每个上传单品都必须有 plan；模型空返回时用确定性细节框补齐。"""
    plans = res.get("plans") or []
    if not plans:
        plans = [{"index": i, "angle": str(im.get("angle") or "front"),
                  "garment_box": [0.0, 0.0, 1.0, 1.0], "crops": []}
                 for i, im in enumerate(images)]
    for p in plans:
        if not p.get("crops"):
            angle = str(p.get("angle") or "front")
            if angle not in ("front", "back", "side", "detail", "flat"):
                angle = "front"
            p["crops"] = _fallback_crops(cat_key, angle)
    res["plans"] = plans
    return res


def _cells_to_box(cells):
    if not isinstance(cells, (list, tuple)):
        return None
    cols = "ABCDEFGHIJ"
    pts = []
    for cell in cells:
        s = str(cell or "").strip().upper().replace(" ", "")
        m = re.match(r"^([A-J])(10|[1-9])$", s)
        if not m:
            continue
        c = cols.index(m.group(1))
        r = int(m.group(2)) - 1
        pts.append((c, r))
    if not pts:
        return None
    c0 = min(p[0] for p in pts)
    c1 = max(p[0] for p in pts)
    r0 = min(p[1] for p in pts)
    r1 = max(p[1] for p in pts)
    x = max(0.0, c0 / 10 - 0.02)
    y = max(0.0, r0 / 10 - 0.02)
    x2 = min(1.0, (c1 + 1) / 10 + 0.02)
    y2 = min(1.0, (r1 + 1) / 10 + 0.02)
    return _norm_crop_box([x, y, x2 - x, y2 - y])


def _norm_crop_box(value):
    if not isinstance(value, (list, tuple)) or len(value) < 4:
        return None
    try:
        x, y, w, h = [float(v) for v in value[:4]]
    except (TypeError, ValueError):
        return None
    if w <= 0 or h <= 0:
        return None
    x = max(0.0, min(1.0, x))
    y = max(0.0, min(1.0, y))
    x2 = max(0.0, min(1.0, x + w))
    y2 = max(0.0, min(1.0, y + h))
    w, h = x2 - x, y2 - y
    if w < 0.04 or h < 0.04 or w * h > 0.75:
        return None
    return [round(x, 4), round(y, 4), round(w, 4), round(h, 4)]


def _crop_plan_keypoints(payload, images, cat_key, item_keys):
    keys = CROP_KEYPOINTS.get(cat_key, CROP_KEYPOINTS["top"])
    labels = [ITEM_LABELS[k] for k in item_keys]
    lines = [
        "品类：" + (CATS.get(cat_key, {}).get("label") or cat_key),
        "本次选中的单品：" + ("、".join(labels) if labels else "按图片判断"),
        "请标注这些关键点（能看到的都标，看不清写 null）：" + "、".join(keys),
        "坐标必须是 [x, y] 两个 0–1 数字；多张图分别输出 plan。",
        "请输出 JSON。",
    ]
    content = [{"type": "text", "text": "\n".join(lines)}]
    for idx, im in enumerate(images):
        content.append({"type": "text", "text": f"图{idx + 1}（image_index={idx}）"})
        content.append({"type": "image_url", "image_url": {"url": im["data"], "detail": "high"}})
    messages = [
        {"role": "system", "content": CROP_SYSTEM_PROMPT},
        {"role": "user", "content": content},
    ]
    text, _ = llm_chat(messages, json_mode=True, thinking=False, temperature=0.1, max_tokens=2200,
                       timeout=VISION_TIMEOUT, route="crop-plan.keypoints")
    v = parse_json(text)
    raw = v.get("plans") or v.get("images") or []
    if isinstance(raw, dict):
        raw = [raw]
    if not raw and v.get("keypoints"):
        raw = [{"image_index": 0, "angle": v.get("angle"), "keypoints": v.get("keypoints"),
                "garment_box": v.get("garment_box")}]
    plans = []
    for pos, plan in enumerate(raw[:len(images)]):
        if not isinstance(plan, dict):
            continue
        try:
            index = int(plan.get("image_index", plan.get("index", pos)))
        except (TypeError, ValueError):
            index = pos
        if index < 0 or index >= len(images):
            index = pos
        angle = str(plan.get("angle") or "unknown").strip().lower()
        if angle not in ("front", "back", "side", "detail", "flat", "unknown"):
            angle = "unknown"
        hint = str(images[index].get("angle") or "").strip().lower()
        if hint in ("front", "back", "side", "detail", "flat"):
            # 用户在界面上手动选的角度优先于模型判断
            angle = hint
        crops = _derive_crops(cat_key, plan.get("keypoints") or {}, angle,
                                  _norm_crop_box(plan.get("garment_box")))
        garment_box = _norm_crop_box(plan.get("garment_box"))
        if not garment_box and crops:
            xs = [c["box"][0] for c in crops]
            ys = [c["box"][1] for c in crops]
            x2s = [c["box"][0] + c["box"][2] for c in crops]
            y2s = [c["box"][1] + c["box"][3] for c in crops]
            x, y = max(0.0, min(xs) - 0.05), max(0.0, min(ys) - 0.05)
            x2, y2 = min(1.0, max(x2s) + 0.05), min(1.0, max(y2s) + 0.05)
            garment_box = _norm_crop_box([x, y, x2 - x, y2 - y])
        plans.append({
            "index": index,
            "angle": angle,
            "garment_box": garment_box or [0.0, 0.0, 1.0, 1.0],
            "crops": crops,
        })
    return {"plans": plans, "meta": {"images": len(images), "plans": len(plans), "mode": "keypoints"}}


CROP_CHECK_SYSTEM_PROMPT = """你是服装细节裁切的质检员。用户会给你若干张裁切图，每张图标注一个部位名。判断这张裁切是否真的包含该部位，且没有裁到大量背景、人脸或空白。
只输出一个 JSON 对象：
{"results":[{"index":0,"ok":true,"confidence":0.86}]}
index 从 0 开始；ok 为 true/false；confidence 0–1。看不清或不确定时 ok 可为 false 但 confidence 低。"""


def crop_check(payload):
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    items = [it for it in payload.get("items") or []
             if isinstance(it, dict) and str(it.get("data") or "").startswith("data:image/")]
    items = items[:12]
    if not items:
        raise ValueError("没有可检查的裁切图")
    content = [{"type": "text", "text": "逐张判断裁切是否包含标注部位；只输出 JSON。"}]
    for i, it in enumerate(items):
        label = str(it.get("label") or f"细节{i + 1}").strip()
        content.append({"type": "text", "text": f"图{i}：{label}"})
        content.append({"type": "image_url", "image_url": {"url": it["data"], "detail": "low"}})
    messages = [
        {"role": "system", "content": CROP_CHECK_SYSTEM_PROMPT},
        {"role": "user", "content": content},
    ]
    # 原实现 crop-check 不做「去掉 response_format 重发」，这里保持一致（retry_without_json=False）
    text, _ = llm_chat(messages, json_mode=True, thinking=False, temperature=0.1, max_tokens=1500,
                       timeout=VISION_TIMEOUT, route="crop-check", retry_without_json=False)
    v = parse_json(text)
    results = []
    for i, r in enumerate((v.get("results") or [])[:len(items)]):
        if not isinstance(r, dict):
            continue
        try:
            idx = int(r.get("index", i))
        except (TypeError, ValueError):
            idx = i
        if idx < 0 or idx >= len(items):
            idx = i
        try:
            conf = float(r.get("confidence", 0.6))
        except (TypeError, ValueError):
            conf = 0.6
        results.append({"index": idx, "ok": bool(r.get("ok", True)),
                        "confidence": round(min(1.0, max(0.0, conf)), 2)})
    return {"results": results, "meta": {"items": len(items)}}


def _crop_plan_candidates(images, cat_key, item_keys):
    labels = [ITEM_LABELS[k] for k in item_keys]
    if cat_key == "compose" and item_keys:
        parts = []
        for k in item_keys:
            parts.extend(CROP_PARTS.get(k, ()))
    else:
        parts = list(CROP_PARTS.get(cat_key, CROP_PARTS["top"]))
    parts = list(dict.fromkeys(parts))
    lines = [
        "品类：" + (CATS.get(cat_key, {}).get("label") if cat_key != "compose" else "组合替换"),
        "本次选中的单品：" + ("、".join(labels) if labels else "按图片判断"),
        "这张图适合找的细节（只标看得见的）：" + "、".join(parts),
        "每张完整图后面紧跟着它的候选细节表，候选编号 C1、C2…；请从该图自己的候选表里选。",
        "多张图请分别输出 plan；同一件衣服的多角度只选各自角度能看到的细节。",
        "请输出 JSON，candidate 必须是候选表里的编号。",
    ]
    cand_maps = []
    for im in images:
        cmap = {}
        for c in im.get("candidates") or []:
            cid = str(c.get("id") or c.get("candidate") or "").strip().upper()
            box = _norm_crop_box(c.get("box"))
            if cid and box:
                cmap[cid] = box
        cand_maps.append(cmap)
    content = [{"type": "text", "text": "\n".join(lines)}]
    for idx, im in enumerate(images):
        content.append({"type": "text", "text": f"图{idx + 1}：完整服装（image_index={idx}）"})
        content.append({"type": "image_url", "image_url": {"url": im["data"], "detail": "high"}})
        if im.get("sheet_data"):
            content.append({"type": "text", "text": f"图{idx + 1}的候选细节表：C1–C{len(cand_maps[idx])}"})
            content.append({"type": "image_url", "image_url": {"url": im["sheet_data"], "detail": "high"}})

    messages = [
        {"role": "system", "content": CROP_CANDIDATE_SYSTEM_PROMPT},
        {"role": "user", "content": content},
    ]
    text, _ = llm_chat(messages, json_mode=True, thinking=False, temperature=0.1, max_tokens=2500,
                       timeout=VISION_TIMEOUT, route="crop-plan.candidates")
    v = parse_json(text)
    raw = v.get("plans") or v.get("images") or []
    if isinstance(raw, dict):
        raw = [raw]
    if not raw and v.get("crops"):
        raw = [{"image_index": 0, "angle": v.get("angle"), "garment_box": v.get("garment_box"),
                "crops": v.get("crops")}]
    plans = []
    for pos, plan in enumerate(raw[:len(images)]):
        if not isinstance(plan, dict):
            continue
        try:
            index = int(plan.get("image_index", plan.get("index", pos)))
        except (TypeError, ValueError):
            index = pos
        if index < 0 or index >= len(images):
            index = pos
        angle = str(plan.get("angle") or "unknown").strip().lower()
        if angle not in ("front", "back", "side", "detail", "flat", "unknown"):
            angle = "unknown"
        crops = []
        seen = set()
        for c in plan.get("crops") or []:
            if not isinstance(c, dict):
                continue
            label = str(c.get("label") or c.get("name") or "").strip()
            cid = str(c.get("candidate") or c.get("candidate_id") or c.get("id") or "").strip().upper()
            box = cand_maps[index].get(cid) if cid else None
            box = (box
                   or _cells_to_box(c.get("cells"))
                   or _norm_crop_box(c.get("box") or c.get("bbox") or c.get("rect")))
            try:
                conf = float(c.get("confidence", 0.7))
            except (TypeError, ValueError):
                conf = 0.7
            if not label or not box or conf < 0.4:
                continue
            key = label.replace(" ", "")
            if key in seen:
                continue
            seen.add(key)
            crops.append({"label": label[:24], "box": box, "confidence": round(min(1.0, max(0.0, conf)), 2)})
        garment_box = (_cells_to_box(plan.get("garment_cells"))
                       or _norm_crop_box(plan.get("garment_box")))
        if not garment_box and crops:
            xs = [c["box"][0] for c in crops]
            ys = [c["box"][1] for c in crops]
            x2s = [c["box"][0] + c["box"][2] for c in crops]
            y2s = [c["box"][1] + c["box"][3] for c in crops]
            x, y = max(0.0, min(xs) - 0.06), max(0.0, min(ys) - 0.06)
            x2, y2 = min(1.0, max(x2s) + 0.06), min(1.0, max(y2s) + 0.06)
            garment_box = _norm_crop_box([x, y, x2 - x, y2 - y])
        plans.append({
            "index": index,
            "angle": angle,
            "garment_box": garment_box or [0.0, 0.0, 1.0, 1.0],
            "crops": crops[:4],
        })
    return {"plans": plans, "meta": {"images": len(images), "plans": len(plans)}}


def crop_plan(payload):
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    images = [im for im in payload.get("images") or []
              if isinstance(im, dict) and str(im.get("data") or "").startswith("data:image/")]
    images = images[:6]
    if not images:
        raise ValueError("没有可识别的服装图")

    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    if cat_key == "compose" and len(item_keys) == 1:
        res = _crop_plan_keypoints(payload, images, item_keys[0], item_keys)
        return _ensure_crop_plans(res, images, item_keys[0])
    if cat_key not in ("look", "compose"):
        res = _crop_plan_keypoints(payload, images, cat_key, item_keys)
        return _ensure_crop_plans(res, images, cat_key)
    if cat_key == "compose" and len(images) > 1:
        # 组合“分别上传”时每张图对应一个单品，分别按该单品品类找细节；
        # 不能把三张图一起当 compose 候选，否则模型容易只返回上衣。
        role_key = {v: k for k, v in ITEM_LABELS.items()}
        role_key.update({k: k for k in ITEM_LABELS})
        plans = []
        for idx, im in enumerate(images):
            role = str(im.get("role") or "")
            key = role_key.get(role)
            if not key:
                key = next((k for k, v in ITEM_LABELS.items() if v in role), None)
            if not key and idx < len(item_keys):
                # 前端旧版本不带 role 时，按上传顺序与选中单品一一对应
                key = item_keys[idx]
            if key not in item_keys:
                continue
            sub = _crop_plan_keypoints(payload, [im], key, [key])
            for _ in range(2):
                if any(p.get("crops") for p in sub.get("plans") or []):
                    break
                sub = _crop_plan_keypoints(payload, [im], key, [key])
            if not any(p.get("crops") for p in sub.get("plans") or []) and im.get("candidates"):
                alt = _crop_plan_candidates([im], key, [key])
                if any(p.get("crops") for p in alt.get("plans") or []):
                    sub = alt
            sub = _ensure_crop_plans(sub, [im], key)
            for plan in sub.get("plans") or []:
                plan["index"] = idx
                prefix = ITEM_LABELS[key]
                for c in plan.get("crops") or []:
                    if not str(c.get("label") or "").startswith(prefix):
                        c["label"] = prefix + "·" + str(c.get("label") or "细节")
                plans.append(plan)
        if plans:
            return {"plans": plans, "meta": {"images": len(images), "plans": len(plans), "mode": "per-item"}}
    return _crop_plan_candidates(images, cat_key, item_keys)


# 修复：产品交付的 serve_reverse-2026-10-30.1.py 在这里丢了函数头，函数体被孤立在 crop_plan 的 return 之后，
# 导致 user_prompt 调用时 NameError、文字降级从不真正调模型。按 2026-10-30.3.py 原样补回（2026-10-31）。
def _colors_text(palettes):
    parts = []
    for p in palettes or []:
        colors = p.get("colors") or []
        if not colors:
            continue
        desc = "、".join(
            f"{c.get('name') or '未知'} {int(c.get('pct') or 0)}%"
            for c in colors[:4] if c.get("name") and c.get("name") != "肤色"
        )
        if desc:
            parts.append(f"{p.get('role') or '服装图'}：{desc}")
    return "；".join(parts)


def user_prompt(payload):
    cat = CATS.get(payload.get("category") or "top", CATS["top"])
    lines = [f"品类：{cat['label']}"]
    if payload.get("fabric"):
        lines.append(f"面料：{payload['fabric']}")
    if payload.get("notes"):
        lines.append(f"补充说明：{payload['notes']}")
    if payload.get("category") == "set":
        mode = payload.get("set_mode") or "one"
        lines.append("上传方式：" + ("分别上传（文胸图 + 内裤图）" if mode == "two" else "一套合影（单张）"))
        if mode == "two":
            lines.append("要求：两件需要同时穿在模特身上，保持同一系列配色与工艺。")
    colors = _colors_text(payload.get("palettes"))
    if colors:
        lines.append(f"自动提取主色（可能含背景误差）：{colors}")
    lines.append("请输出 JSON。")
    return "\n".join(lines)


NOT_CONFIGURED = "未配置 AI 网关（AIG_BASE_URL / AIG_SECRET）"
INLINE_IMAGE_MAX_CHARS = 14_000_000  # 网关契约：单张 data: 图 ≤ ~10MiB（解码后）
GATEWAY_DETAILS = ("auto", "low", "high")


def llm_configured():
    return bool(AIG_BASE_URL and AIG_SECRET)


# ---------------------------------------------------------------- 单次请求的总截止时间

class RequestBudgetExceeded(Exception):
    """单次请求总预算（REVERSE_REQUEST_BUDGET_S）用尽。不是 AigError：不当作网关错误重试。"""


_REQ = threading.local()


def begin_request(budget_s=None):
    """在处理线程里开始计时；返回截止时刻（time.monotonic 基准）。"""
    _REQ.deadline = time.monotonic() + (REQUEST_BUDGET_S if budget_s is None else budget_s)
    return _REQ.deadline


def request_deadline():
    return getattr(_REQ, "deadline", None)


def set_request_deadline(deadline):
    """线程池 worker 里继承调用方的截止时间。"""
    _REQ.deadline = deadline


def request_remaining(deadline=None):
    """距截止还剩多少秒；没有截止时间（请求之外的调用）返回 None。"""
    d = deadline if deadline is not None else request_deadline()
    return None if d is None else d - time.monotonic()


def budget_sleep(seconds, deadline=None):
    """退避等待；剩余预算不够等（含至少 1 秒给下一次尝试）就抛 RequestBudgetExceeded，不白睡。"""
    rem = request_remaining(deadline)
    if rem is not None and rem < seconds + 1.0:
        raise RequestBudgetExceeded(_BUDGET_MSG % REQUEST_BUDGET_S)
    time.sleep(seconds)


_BUDGET_MSG = "处理超时：超过单次请求总时限 %d 秒，请稍后重试或减少图片数量"
MIN_RERUN_BUDGET_S = 40  # crop-plan 重跑所需的最小剩余预算
MIN_CALL_BUDGET_S = 5  # 剩余不足这个数就不再发起模型调用


def _aig_client():
    return aig.AigClient(AIG_BASE_URL, AIG_KEY_ID, AIG_SECRET, AIG_CALLER, wait_s=AIG_WAIT_S)


def _gateway_part(part):
    """OpenAI 形状的 content part → 网关契约 part（图片字段是 uri，不是 image_url.url）。"""
    if isinstance(part, dict) and part.get("type") == "image_url":
        iu = part.get("image_url")
        uri = iu.get("url") if isinstance(iu, dict) else (iu or part.get("uri"))
        uri = str(uri or "")
        if len(uri) > INLINE_IMAGE_MAX_CHARS:
            raise ValueError("单张图片过大")
        out = {"type": "image_url", "uri": uri}
        detail = iu.get("detail") if isinstance(iu, dict) else part.get("detail")
        if detail in GATEWAY_DETAILS:
            out["detail"] = detail
        return out
    if isinstance(part, dict):
        return {"type": "text", "text": str(part.get("text") or "")}
    return {"type": "text", "text": str(part or "")}


def _gateway_messages(messages):
    out = []
    for m in messages:
        content = m.get("content")
        if isinstance(content, list):
            parts = [_gateway_part(p) for p in content]
        else:
            parts = [{"type": "text", "text": str(content or "")}]
        out.append({"role": m.get("role") or "user", "content": parts})
    return out


def llm_chat(messages, *, json_mode, thinking, max_tokens, timeout, route,
             temperature=None, retry_without_json=True, deadline=None):
    """经 ai-gateway 调 chat.completion（模型组 REVERSE_MODEL_GROUP），返回 (text, usage)。

    messages 仍是原来的 OpenAI 形状（system/user，content 为字符串或 text/image_url parts），这里转换成网关契约。
    json_mode → input.response_format={"type":"json_object"}；thinking True/False → input.thinking "on"/"off"，
    None 不传（模型默认）；temperature/max_tokens 原样映射；timeout → constraints.deadline_at。
    text 取 result.message.content，为空时取 reasoning_content（与原实现一致）。
    JSON 模式被网关判 INPUT_REJECTED（且错误与 response_format/json 相关）时，去掉 JSON 模式重发一次。
    失败抛 aig.AigError 子类（AigNetworkError 相当于原 URLError，其余相当于原 HTTPError）。
    """
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    client = _aig_client()
    gw_messages = _gateway_messages(messages)
    if deadline is None:
        deadline = request_deadline()   # 当前请求的总截止时间；请求之外的调用为 None（只受 timeout 约束）

    def effective_timeout():
        """本次调用可用的秒数 = min(timeout, 请求剩余预算 - 网关多留的 15s 余量)。"""
        rem = request_remaining(deadline)
        if rem is None:
            return timeout, timeout + 15
        if rem < MIN_CALL_BUDGET_S:
            raise RequestBudgetExceeded(_BUDGET_MSG % REQUEST_BUDGET_S)
        t = max(MIN_CALL_BUDGET_S - 1, min(timeout, int(rem) - 2))
        return t, min(t + 15, rem)

    def body(json_on, timeout):
        inp = {"messages": gw_messages}
        if json_on:
            inp["response_format"] = {"type": "json_object"}
        if thinking is not None:
            inp["thinking"] = "on" if thinking else "off"
        if temperature is not None:
            inp["temperature"] = temperature
        if max_tokens:
            inp["max_tokens"] = int(max_tokens)
        constraints = {"deadline_at": aig.utc_deadline(timeout)}
        if ALLOWED_DEPLOYMENTS:
            constraints["allowed_deployments"] = list(ALLOWED_DEPLOYMENTS)
        return {
            "operation": "chat.completion",
            "model_group": MODEL_GROUP,
            "input": inp,
            "constraints": constraints,
            "labels": {"caller": AIG_CALLER, "route": route},
        }

    # budget 比 deadline_at 多留一点，让网关先给出 DEADLINE_EXCEEDED 终态；但不超过请求剩余预算
    t, budget = effective_timeout()
    try:
        rec = client.call(body(json_mode, t), idempotency_key=aig.new_idempotency_key(AIG_CALLER, route),
                          budget_s=budget)
    except AigError as e:
        if not (json_mode and retry_without_json and e.is_input_rejected
                and e.mentions("response_format", "json")):
            raise
        sys.stderr.write("llm %s · JSON 模式被拒（%s），去掉 response_format 重发\n" % (route, e.code))
        t, budget = effective_timeout()
        rec = client.call(body(False, t), idempotency_key=aig.new_idempotency_key(AIG_CALLER, route),
                          budget_s=budget)
    msg = ((rec.get("result") or {}).get("message")) or {}
    text = (msg.get("content") or "").strip()
    if not text:
        text = (msg.get("reasoning_content") or "").strip()
    return text, rec.get("usage") or {}


_READY_CACHE = {"at": 0.0, "key": None, "value": (None, "")}
READY_TTL = 60


def gateway_ready():
    """POST /v1/catalog/readiness（purpose=catalog），结果缓存 60s；未配置返回 (None, "not_configured")。"""
    if not llm_configured():
        return None, "not_configured"
    key = (AIG_BASE_URL, MODEL_GROUP)
    now = time.time()
    if _READY_CACHE["key"] == key and now - _READY_CACHE["at"] < READY_TTL:
        return _READY_CACHE["value"]
    try:
        value = _aig_client().readiness(MODEL_GROUP)
    except AigError as e:
        value = (False, "gateway_error:%s" % (e.error_code or e.code or type(e).__name__))
    except Exception as e:  # noqa: BLE001
        value = (False, "gateway_error:%s" % type(e).__name__)
    _READY_CACHE.update(at=now, key=key, value=value)
    return value


def deepseek_fields(payload):
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    messages = [{"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": user_prompt(payload)}]
    # 原实现无 thinking 字段、超时为 _post_json 默认 120s
    text, _ = llm_chat(messages, json_mode=True, thinking=None, temperature=0.3, max_tokens=2500,
                       timeout=120, route="analyze.text")
    return parse_json(text)


def parse_json(text):
    if not text:
        return {}
    text = re.sub(r"^```(?:json)?\s*|\s*```$", "", text.strip(), flags=re.MULTILINE)
    try:
        obj = json.loads(text)
        if isinstance(obj, dict):
            return obj
    except ValueError:
        pass
    m = re.search(r"\{.*\}", text, re.DOTALL)
    if m:
        try:
            obj = json.loads(m.group(0))
            if isinstance(obj, dict):
                return obj
        except ValueError:
            pass
    return {}


def _clean_list(value, limit=6):
    if not isinstance(value, list):
        return []
    out = []
    for v in value:
        if not isinstance(v, str):
            continue
        for part in re.split(r"[或/、，,；;]+", v):
            part = re.sub(r"\s+", "", part)
            if len(part) >= 2 and part not in out:
                out.append(part)
            if len(out) >= limit:
                return out
    return out


def _trim(s):
    return re.sub(r"[。；;，,、\s]+$", "", str(s or "").strip())


BANNED_WORDS = ("透视", "湿身", "紧身勾勒", "性感", "诱惑", "裸露", "私密", "半透明", "真空")
NEGATIONS = ("不", "无", "非", "避免", "禁止", "不要")


def _sanitize(text):
    if not text:
        return ""
    text = str(text).replace("半透明", "").replace("透明", "").replace("透纱", "网纱")
    clauses = re.split(r"(?<=[。；;，,])", text)
    kept = []
    for c in clauses:
        if not c.strip():
            continue
        bad = False
        for w in BANNED_WORDS:
            start = 0
            while True:
                idx = c.find(w, start)
                if idx == -1:
                    break
                if not any(n in c[max(0, idx - 3):idx] for n in NEGATIONS):
                    bad = True
                    break
                start = idx + 1
            if bad:
                break
        if not bad:
            kept.append(c)
    return "".join(kept)


def _sanitize_design(text):
    """设计细节里的“透视”是工艺描述，换成网纱拼接保留下来；其余违规词仍过滤。"""
    text = str(text or "").strip()
    if not text:
        return ""
    text = re.sub(r"(?<!不)透视网纱", "网纱", text)
    text = re.sub(r"(?<!不)透视", "网纱拼接", text)
    text = text.replace("半透明", "").replace("透明", "").replace("透纱", "网纱")
    return _sanitize(text).strip("；;，, ")


def _notes_covered(notes, text):
    chunks = [c for c in re.split(r"[，,；;。、\s]+", notes) if len(c) >= 2]
    if not chunks:
        return False
    hits = sum(1 for c in chunks if c in text)
    return hits >= max(1, int(len(chunks) * 0.6))


# ---------------------------------------------------------------- 读图

def _balance_crops(crops, limit):
    """按单品标签前缀轮询取细节图，避免只剩第一件单品的裁切。"""
    groups, order = {}, []
    for c in crops:
        key = str(c.get("label") or "").split("·")[0]
        if key not in groups:
            groups[key] = []
            order.append(key)
        groups[key].append(c)
    out = []
    round_no = 0
    while len(out) < limit and any(len(groups[k]) > round_no for k in order):
        for k in order:
            if len(groups[k]) > round_no and len(out) < limit:
                out.append(groups[k][round_no])
        round_no += 1
    return out


def select_images(payload):
    """从上传图里挑最多 9 张：主图 + 细节 + 模特 1，去重。"""
    raw = payload.get("images") or []
    imgs = []
    for im in raw:
        if not isinstance(im, dict):
            continue
        data = im.get("data")
        if isinstance(data, str) and data.startswith("data:image/"):
            imgs.append(im)
    if not imgs:
        return []

    mains = [im for im in imgs if im.get("role") not in ("局部细节", "人物模特")]
    crops = [im for im in imgs if im.get("role") == "局部细节"]
    models = [im for im in imgs if im.get("role") == "人物模特"]
    if not mains and not crops and not models:
        mains = imgs[:2]

    cat_key = payload.get("category") or "top"
    model_slots = 1 if models else 0
    if cat_key in ("look", "compose"):
        max_mains = min(6, len(mains))
    else:
        # 同一件衣服可上传正面/背面/侧面；最多 4 张主图
        max_mains = min(4, len(mains)) if len(mains) > 1 else min(2, len(mains))
    crop_limit = max(1, MAX_IMAGES - max_mains - model_slots)
    picked_crops = _balance_crops(crops, crop_limit)

    picked = mains[:max_mains] + picked_crops + models[:1]
    out, seen = [], set()
    for im in picked:
        h = hashlib.sha1(im["data"].encode("utf-8", "ignore")).hexdigest()
        if h in seen:
            continue
        seen.add(h)
        out.append(im)
    return out[:MAX_IMAGES]


def vision_content(payload, picked):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    cat = _compose_cat(item_keys) if cat_key == "compose" and item_keys else CATS.get(cat_key, CATS["top"])
    labels = []
    for i, im in enumerate(picked, 1):
        role = str(im.get("role") or "图片")
        label = str(im.get("label") or "")
        labels.append(f"图{i}：{role}" + (f"（{label}）" if label else ""))

    lines = [
        f"品类：{cat['label']}",
        f"面料（用户填写）：{payload.get('fabric') or '未填'}",
        f"补充说明（用户填写）：{payload.get('notes') or '无'}",
        f"用户指定长度：{payload.get('length_class') or '未指定'}",
        f"用户指定袖长：{payload.get('sleeve_class') or '未指定'}",
    ]
    if item_keys:
        lines.append("本次要替换的单品：" + "、".join(ITEM_LABELS[k] for k in item_keys))
    look_lengths = payload.get("look_lengths") or {}
    if look_lengths.get("bottom"):
        lines.append("用户指定下装长度：" + str(look_lengths["bottom"]))
    if look_lengths.get("dress"):
        lines.append("用户指定裙长：" + str(look_lengths["dress"]))
    lengths = payload.get("lengths") or {}
    sleeves = payload.get("sleeves") or {}
    item_size_parts = []
    for k in item_keys:
        lv = str(lengths.get(k) or "").strip()
        sv = str(sleeves.get(k) or "").strip()
        if lv:
            item_size_parts.append(f"{ITEM_LABELS[k]}长度={lv}")
        if sv:
            item_size_parts.append(f"{ITEM_LABELS[k]}袖长={sv}")
    if item_size_parts:
        lines.append("用户指定单品长度/袖长（以用户为准）：" + "；".join(item_size_parts))
    if cat["label"] == "文胸内裤两件套":
        mode = "分别上传（文胸图 + 内裤图）" if (payload.get("set_mode") or "one") == "two" else "一套合影（单张）"
        lines.append("上传方式：" + mode)
    elif cat["label"] == "整套 Look":
        mode = ("分别上传（按单品：上装/下装/连衣裙/外套/鞋子/配饰）"
                if (payload.get("set_mode") or "one") == "two" else "一套合影（单张）")
        lines.append("上传方式：" + mode)
    elif cat.get("compose"):
        mode = ("分别上传（按单品）" if (payload.get("set_mode") or "one") == "two" else "一套合影（单张）")
        lines.append("上传方式：" + mode)
    lines.append("角色说明（非常重要）：")
    lines.append("- 标为「人物模特」的图是本次要换装的模特；model_outfit、pose、background、light 只能来自这张图。")
    lines.append("- 标为「上装/下装/连衣裙/文胸/内裤/鞋子/...」的图是服装参考图；即使图里有人穿着它，那个人也不是本次模特。garment_colors、material、pattern、structure 只能来自这些服装参考图，不能把参考图里人物的衣服写进 model_outfit，也不能从人物模特图取色。")
    lines.append("- 没有人物模特图时，model_outfit 留空。")
    lines.append("图片顺序：")
    lines.extend(labels)
    lines.append("请分析服装并输出 JSON。用户填写的信息只作参考，画面事实以图片为准。")

    deep = bool(payload.get("deep"))
    content = [{"type": "text", "text": "\n".join(lines)}]
    for im in picked:
        role = str(im.get("role") or "")
        detail = "high" if deep or role != "人物模特" else "low"
        content.append({"type": "image_url",
                        "image_url": {"url": im["data"], "detail": detail}})
    return content


def vision_fields(payload, picked):
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    deep = bool(payload.get("deep"))
    messages = [
        {"role": "system", "content": VISION_SYSTEM_PROMPT},
        {"role": "user", "content": vision_content(payload, picked)},
    ]
    last = {}
    for attempt in range(2):
        # deep 时原实现不传 thinking（模型默认），非 deep 关闭 thinking
        text, _ = llm_chat(messages, json_mode=True, thinking=(None if deep else False), temperature=0.1,
                           max_tokens=VISION_MAX_TOKENS if deep else 4000,
                           timeout=VISION_TIMEOUT, route="analyze.vision")
        last = parse_json(text)
        if last:
            return last
        if attempt < 1:
            budget_sleep(0.6)
    return last


CATEGORY_HINTS = {
    "top": ("上装", "毛衣", "针织", "衬衫", "T恤", "t恤", "卫衣", "外套", "夹克", "大衣", "背心", "吊带", "polo", "POLO"),
    "bottom": ("下装", "裤", "半身裙", "短裙", "长裙", "A字裙", "包臀裙"),
    "dress": ("连衣裙",),
    "look": ("整套", "穿搭", "look", "Look", "搭配"),
    "bra": ("文胸", "胸罩"),
    "panties": ("内裤", "三角裤", "平角裤"),
    "set": ("两件套", "套装"),
}


def _category_match(cat_key, garment_type, model_match):
    """模型按具体款式返回 false 时，用大类词表纠偏；真正的错品类仍然 false。"""
    if model_match:
        return True
    t = str(garment_type or "")
    if cat_key == "bottom" and "连衣裙" in t:
        return False
    if cat_key == "compose":
        return bool(model_match)
    if cat_key == "set" and "文胸" in t and "内裤" in t:
        return True
    if cat_key == "look":
        parts = ("上装", "下装", "连衣裙", "外套", "鞋子", "配饰")
        if sum(1 for p in parts if p in t) >= 2:
            return True
    return any(h in t for h in CATEGORY_HINTS.get(cat_key, ()))


def _clean_outfit(outfit):
    outfit = _trim(_sanitize(outfit))
    if not outfit or outfit in ("无", "未穿", "没有", "无模特图"):
        return ""
    outfit = re.sub(r"^(模特)?(当前)?(身上)?(身穿|穿着|穿的是|穿了|穿)[:：]?", "", outfit).strip()
    outfit = re.sub(r"^一套(为)?", "", outfit).strip()
    parts = [p.strip("，, ") for p in re.split(r"[；;。]", outfit) if p.strip()]
    drop = ("图1", "图2", "图3", "未在模特", "不在模特", "服装图", "商品图", "模特图",
            "必须", "需要", "移除", "替换", "不同", "不一致", "上传的", "图中", "另一")
    parts = [p for p in parts if not any(w in p for w in drop)]
    return "，".join(parts)[:90]


NECKLINE_GROUPS = [
    ("一字肩", ("一字肩",)),
    ("挂脖", ("挂脖",)),
    ("圆领", ("圆领",)),
    ("V领", ("V领", "V 领")),
    ("方领", ("方领",)),
    ("翻领", ("翻领",)),
    ("高领", ("高领",)),
]
LOOK_PREFIXES = ("上装：", "上衣：", "下装：", "裤子/裙子：", "连衣裙：", "外套：", "鞋子：", "配饰：", "文胸：", "内裤：")
LOOK_BASIS = {
    "上装": "领口、门襟、袖型与袖口、下摆与长度",
    "下装": "腰头、裤型或裙型、长度、裤脚或裙摆",
    "连衣裙": "领口、腰线、裙长、裙摆",
    "外套": "领口、门襟、袖型、长度与廓形",
    "鞋子": "鞋型、鞋面、鞋跟、鞋底",
    "配饰": "材质、尺寸、佩戴位置",
}
LOOK_ONE_BASIS = "以合影中实际出现的单件为准，逐件还原领口、袖型、腰头、裤型、裙长、鞋型与配色关系"

LENGTH_ANCHORS = {
    "top": {
        "及腰": "下摆落在自然腰线",
        "及胯": "下摆落在胯部中段",
        "遮臀": "下摆盖过臀部",
        "长款": "下摆落在大腿中部",
    },
    "bottom": {
        "短裤": "裤脚在大腿中部以上",
        "五分裤": "裤脚在膝盖上方约10厘米",
        "七分裤": "裤脚在小腿中部",
        "九分裤": "裤脚在脚踝上方约5厘米",
        "长裤": "裤脚盖住脚踝、到鞋面",
        "短裙": "裙摆在大腿中部、膝盖上方约15厘米",
        "中裙": "裙摆到膝盖或膝盖下方约5厘米",
        "长裙": "裙摆在小腿中部或脚踝",
    },
    "dress": {
        "膝上": "裙摆在膝盖上方约15厘米",
        "及膝": "裙摆到膝盖",
        "小腿中部": "裙摆在小腿中部",
        "及踝": "裙摆到脚踝",
        "及地": "裙摆触及地面",
    },
}
LENGTH_RATIOS = {
    "top": {"及腰": "1:0.5", "及胯": "1:0.75", "遮臀": "1:0.95", "长款": "1:1.3"},
    "bottom": {"短裤": "1:0.5", "五分裤": "1:0.8", "七分裤": "1:1.15", "九分裤": "1:1.35", "长裤": "1:1.5",
               "短裙": "1:0.55", "中裙": "1:1.05", "长裙": "1:1.55"},
    "dress": {"膝上": "1:1.0", "及膝": "1:1.35", "小腿中部": "1:1.8", "及踝": "1:2.3", "及地": "1:2.5"},
}
RATIO_LABELS = {
    "top": "肩线到腰线 : 衣长",
    "bottom": "肩线到腰线 : 腰线到裙摆/裤脚",
    "dress": "肩线到腰线 : 腰线到裙摆",
    "outer": "肩线到腰线 : 外套长度",
}


def _anchor_for(item_key, value):
    anchors = LENGTH_ANCHORS.get(item_key) or LENGTH_ANCHORS.get("top", {})
    ratios = LENGTH_RATIOS.get(item_key) or LENGTH_RATIOS.get("top", {})
    return anchors.get(value), ratios.get(value)



def _fix_necklines(structure):
    out = []
    for s in structure:
        text = s
        hits = []
        for name, terms in NECKLINE_GROUPS:
            for t in terms:
                if t in text:
                    hits.append(name)
                    text = text.replace(t, "")
                    break
        if len(hits) >= 2:
            replacement = "领口款式以参考图为准，不要混写"
            if not out or out[-1] != replacement:
                out.append(replacement)
        else:
            out.append(s)
    return out


def _normalize_look_structure(structure):
    out, last = [], None
    for s in structure:
        prefix = next((p for p in LOOK_PREFIXES
                       if s.startswith(p) or s.startswith(p[:-1] + ":")), None)
        if prefix:
            last = prefix
            out.append(s)
        elif last:
            out.append(last + s)
        else:
            out.append(s)
    return out


ALL_ITEM_PREFIXES = ("文胸：", "内裤：", "上装：", "上衣：", "下装：", "裤子/裙子：", "连衣裙：", "外套：", "鞋子：", "配饰：")


BRA_TERMS = ("杯", "肩带", "下围", "背扣", "侧比", "钢圈")
PANTY_TERMS = ("裆部", "裤口", "腰头", "包覆", "侧边", "底裆", "后片", "前片")


def _normalize_set_structure(structure):
    out = []
    for s in structure:
        if s.startswith("文胸") or s.startswith("内裤"):
            out.append(s)
            continue
        has_p = any(w in s for w in PANTY_TERMS)
        has_b = any(w in s for w in BRA_TERMS)
        if has_p and not has_b:
            out.append("内裤：" + s)
        elif has_b and not has_p:
            out.append("文胸：" + s)
        # 跨件不明确的碎片直接丢弃，避免把内裤部件写进文胸
    return out


def _merge_prefixed(structure):
    order, groups = [], {}
    for s in structure:
        prefix = next((x for x in ALL_ITEM_PREFIXES
                       if s.startswith(x) or s.startswith(x[:-1] + ":")), None)
        if prefix:
            key = prefix if prefix.endswith("：") else prefix[:-1] + "："
            val = s[len(prefix):] if s.startswith(prefix) else s[len(prefix[:-1] + ":"):]
        else:
            key = order[-1] if order else ""
            val = s
        val = val.strip("；;，, ")
        if key not in groups:
            groups[key] = []
            order.append(key)
        groups[key].append(val)
    return [key + "、".join(groups[key]) for key in order]


def _uploaded_items(payload):
    items = []
    for im in payload.get("images") or []:
        role = str(im.get("role") or "")
        if role and role not in ("局部细节", "人物模特", "整套穿搭", "look", "套装") and role not in items:
            items.append(role)
    return items


def vision_to_fields(v, payload):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    cat = _compose_cat(item_keys) if cat_key == "compose" and item_keys else CATS.get(cat_key, CATS["top"])
    fabric = (payload.get("fabric") or "").strip()
    notes = (payload.get("notes") or "").strip()
    match = _category_match(payload.get("category") or "top",
                            v.get("garment_type"), v.get("garment_match") is not False)

    colors = []
    for c in (v.get("garment_colors") or v.get("colors") or []):
        c = str(c).strip()
        if c and c != "肤色" and c not in colors:
            colors.append(c)
    colors = colors[:3]
    material = str(v.get("material") or "").strip()
    pattern = str(v.get("pattern") or "").strip()
    structure = [str(s).strip() for s in (v.get("structure") or []) if str(s).strip()]
    structure = [s for s in structure
                 if not any(w in s for w in ("未上传", "无法提取", "无法识别", "没有上传"))]
    structure = _fix_necklines(structure)
    if cat_key in ("look", "compose"):
        structure = _normalize_look_structure(structure)
    if cat_key == "set":
        structure = _normalize_set_structure(structure)
    if cat_key == "bra":
        structure = [s.replace("下摆", "下围").replace("领口", "杯口").replace("衣身", "杯身") for s in structure]
        material = material.replace("下摆", "下围").replace("领口", "杯口").replace("衣身", "杯身")
    elif cat_key == "set":
        structure = [
            (s.replace("下摆", "下围").replace("领口", "杯口").replace("衣身", "杯身")
             if s.startswith("文胸") else s.replace("下摆", "裤口") if s.startswith("内裤") else s)
            for s in structure
        ]
    elif cat_key == "compose":
        new = []
        for st in structure:
            if st.startswith("文胸"):
                st = st.replace("下摆", "下围").replace("领口", "杯口").replace("衣身", "杯身")
            elif st.startswith("内裤"):
                st = st.replace("下摆", "裤口")
            new.append(st)
        structure = new
    elif cat_key == "panties":
        structure = [s.replace("下摆", "裤口") for s in structure]
    if cat_key in ("look", "set", "compose"):
        structure = _merge_prefixed(structure)
    structure = structure[:(12 if cat_key == "compose" else 8)]

    item_lengths = {}
    raw_item_lengths = v.get("item_lengths")
    if isinstance(raw_item_lengths, dict):
        for k, val in raw_item_lengths.items():
            k = str(k).strip()
            k = "上衣" if k == "上装" else k
            val = str(val or "").strip()
            if k in ITEM_LABELS.values() and val and val not in ("—", "-", "无", "空", "以参考图为准"):
                item_lengths[k] = val
    item_sleeves = {}
    raw_item_sleeves = v.get("item_sleeves")
    if isinstance(raw_item_sleeves, dict):
        for k, val in raw_item_sleeves.items():
            k = str(k).strip()
            k = "上衣" if k == "上装" else k
            val = str(val or "").strip()
            if k in ITEM_LABELS.values() and val and val not in ("—", "-", "无", "空", "以参考图为准"):
                item_sleeves[k] = val
    item_design_details = {}
    raw_item_design = v.get("item_design_details")
    if isinstance(raw_item_design, dict):
        for k, arr in raw_item_design.items():
            k = str(k).strip()
            k = "上衣" if k == "上装" else k
            if k not in ITEM_LABELS.values():
                continue
            if isinstance(arr, str):
                arr = [arr]
            vals = [_sanitize_design(str(x)) for x in (arr or [])]
            vals = [x for x in vals if x]
            if vals:
                item_design_details[k] = vals

    raw_props = v.get("proportions") or []
    if isinstance(raw_props, str):
        raw_props = [raw_props]
    proportions = []
    for x in raw_props:
        x = _trim(_sanitize(str(x)))
        if x:
            proportions.append(x)
    proportions = proportions[:6]

    raw_design = v.get("design_details") or []
    if isinstance(raw_design, str):
        raw_design = [raw_design]
    design_details = []
    if item_design_details:
        for label, arr in item_design_details.items():
            for x in arr:
                design_details.append(f"{label}：{x}")
    else:
        for x in raw_design:
            x = _sanitize_design(str(x))
            if x:
                design_details.append(x)
    design_details = [d for d in design_details if not d.endswith("无明显特殊设计")]
    seen_design = set()
    deduped = []
    for d in design_details:
        key = re.sub(r"[\s，,。；;：:、·]+", "", d)
        if key and key not in seen_design:
            seen_design.add(key)
            deduped.append(d)
    design_details = deduped[:(12 if cat_key == "compose" else 8)]

    if not design_details or design_details == ["无明显特殊设计"]:
        design_words = ("不对称", "开衩", "镂空", "绑带", "抽绳", "褶皱", "拼接", "撞色",
                        "刺绣", "印花", "五金", "异形", "贴袋", "翻领", "荷叶边", "流苏",
                        "抽褶", "收褶", "扣环", "拉链", "口袋", "可拆", "窄带", "绕颈",
                        "系带", "大摆", "A字", "垂褶")
        candidates = []
        if pattern and not any(w in pattern for w in ("无印花", "无图案", "纯色")) \
                and any(w in pattern for w in design_words):
            candidates.append(pattern)
        for st in structure:
            if any(w in st for w in design_words) and not any(
                    w in st for w in ("以参考图为准", "未见", "不可见", "不确定")):
                candidates.append(st)
        seen = set()
        design_details = []
        for x in candidates:
            x = _sanitize_design(x)
            if x and x not in seen:
                seen.add(x)
                design_details.append(x)
        design_details = design_details[:6]

    length_class = str(payload.get("length_class") or v.get("length_class") or "").strip()
    sleeve_class = str(payload.get("sleeve_class") or v.get("sleeve_class") or "").strip()

    tagged_fabric = {}
    for chunk in re.split(r"[；;\n]", fabric or ""):
        m_tag = re.match(r"\s*(上装|上衣|下装|连衣裙|外套|鞋子|配饰|文胸|内裤)[：:]\s*(.*)", chunk)
        if m_tag and m_tag.group(2).strip():
            tagged_fabric["上衣" if m_tag.group(1) == "上装" else m_tag.group(1)] = m_tag.group(2).strip()
    tagged_fabric_line = "；".join(f"{lbl} {val}" for lbl, val in tagged_fabric.items())

    parts = []
    if match:
        if tagged_fabric_line and material:
            parts.append(f"用户标注面料（按件）：{tagged_fabric_line}；图中可见{material}，以参考图为准")
        elif tagged_fabric_line:
            parts.append(f"用户标注面料（按件）：{tagged_fabric_line}，以参考图质感为准")
        elif fabric and material:
            parts.append(f"用户标注面料为{fabric}，图中可见{material}，以参考图为准")
        elif fabric:
            parts.append(f"用户标注面料为{fabric}，以参考图质感为准")
        elif material:
            if cat_key == "compose" and re.match(
                    r"\s*(上衣|上装|下装|连衣裙|外套|鞋子|配饰|文胸|内裤)[：:]", material):
                parts.append(material)
            elif material.startswith(("上装", "下装", "连衣裙", "外套", "鞋子", "配饰")):
                parts.append(material)
            else:
                parts.append(f"面料为{material}")
        if colors:
            parts.append("主色为" + "、".join(colors))
        if pattern:
            parts.append(pattern)
    material_text = "；".join(parts)

    details = "；".join(structure) if match else ""
    if notes and match:
        details = (details + "；用户补充（如与参考图冲突，以参考图为准）：" + notes) if details else notes

    # 动作沿用参考模特图；内衣/Look 仍用棚拍光，不沿用户外光
    has_model = any(str(im.get("role")) == "人物模特" for im in payload.get("images") or [])
    styling = str(v.get("pose") or "").strip()
    if any(w in styling for w in ("平铺", "人台", "挂拍", "无模特")):
        styling = ""
    if cat["lingerie"] and any(w in styling for w in ("躺", "坐", "跪", "趴", "扭腰", "撩", "抬腿", "张腿", "蹲")):
        styling = ""
    for w in ("仰拍", "俯拍", "低角度", "高角度"):
        styling = styling.replace(w, "平视")
    if cat["lingerie"] or cat_key in ("look", "compose"):
        light = ""
    else:
        light = str(v.get("light") or "").strip()
    background = _trim(_sanitize(v.get("background"))) if has_model else ""

    return {
        "material": material_text,
        "details": details,
        "styling": styling,
        "light": light,
        "background": background,
        "negatives": v.get("negatives"),
        "existing_outfit": (str(v.get("model_outfit") or "").strip() if has_model else ""),
        "proportions": proportions,
        "design_details": design_details,
        "item_lengths": item_lengths,
        "item_sleeves": item_sleeves,
        "item_design_details": item_design_details,
        "length_class": length_class,
        "sleeve_class": sleeve_class,
        "_vision": True,
        "_match": match,
    }


OUTFIT_TAIL = {
    "top": "需要替换为参考图中的{subject}，不能保留原有领口、袖子、下摆或图案；如模特穿外套、披肩或长围巾，需在穿着参考图上装的前提下移除遮挡物，不能出现未穿上衣；最终画面必须是穿着上装的完整状态，不能出现裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除，只移除模特原有的、参考图没有的配饰。",
    "bottom": "需要替换为参考图中的{subject}，不能保留原有腰头、裤型、裤脚或图案；上装、鞋子与配饰保持模特图原样，遮住腰胯的长上衣或外套需换成短款，不要为了展示下装而脱掉上装；最终画面必须是穿着下装的完整状态，不能出现未穿下装、裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除。",
    "dress": "需要替换为参考图中的{subject}，不能保留原有领口、腰线、裙摆或图案；需在穿着参考图连衣裙的前提下移除原有外套、腰带、围巾等遮挡物，不能出现未着装；最终画面必须是穿着连衣裙的完整状态，不能出现裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除，只移除模特原有的、参考图没有的配饰。",
    "bra": "必须替换为参考图中的文胸（胸罩），不能保留其领口、袖子、下摆或任何部分；最终画面必须是穿着文胸的完整状态，不能出现未穿上衣、裸露或脱衣过程；下装、包、鞋子与配饰保持原样；参考图自带的肩带、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "panties": "必须替换为参考图中的内裤，不能保留其腰头、裤腿或任何部分；最终画面必须是穿着内裤的完整状态，不能出现未穿下装、裸露或脱衣过程；上衣、包、鞋子与配饰保持原样；参考图自带的腰头、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "set": "必须替换为参考图中的文胸与内裤两件套，不能保留其领口、袖子、裤腿或任何部分；最终画面必须是两件都穿好的完整状态，不能出现未穿、裸露或脱衣过程；包、鞋子与配饰保持原样；参考图自带的肩带、腰头、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "look": "需要替换为参考图中的整套穿搭，不能保留原有任何单品、层叠关系或图案；最终画面必须是完整穿着状态，不能出现脱衣过程或裸露；参考图自带的配饰、五金、刺绣、拼接等设计元素不得移除。",
}

OUTFIT_FALLBACK = {
    "top": "模特原有的上装需要替换为参考图中的上装，不能保留原有领口、袖子、下摆或图案；如模特穿外套、披肩或长围巾，需在穿着参考图上装的前提下移除遮挡物，不能出现未穿上衣；最终画面必须是穿着上装的完整状态，不能出现裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除。",
    "bottom": "模特原有的下装需要替换为参考图中的下装，不能保留原有腰头、裤型、裤脚或图案；遮住腰胯的长上衣或外套需换成短款，不要为了展示下装而脱掉上装；最终画面必须是穿着下装的完整状态，不能出现未穿下装、裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除。",
    "dress": "模特原有的服装需要替换为参考图中的连衣裙，不能保留原有领口、腰线、裙摆或图案；需在穿着参考图连衣裙的前提下移除原有外套、腰带、围巾等遮挡物，不能出现未着装；最终画面必须是穿着连衣裙的完整状态，不能出现裸露或脱衣过程；参考图服装自带的腰带、绑带、五金、刺绣、拼接等设计元素不得移除。",
    "bra": "模特原有的上衣必须替换为参考图中的文胸（胸罩）；下装、包、鞋子与配饰保持原样；最终画面必须是穿着文胸的完整状态，不能出现未穿上衣、裸露或脱衣过程；参考图自带的肩带、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "panties": "模特原有的下装必须替换为参考图中的内裤；上衣、包、鞋子与配饰保持原样；最终画面必须是穿着内裤的完整状态，不能出现未穿下装、裸露或脱衣过程；参考图自带的腰头、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "set": "模特原有的上衣与下装必须替换为参考图中的文胸与内裤两件套；包、鞋子与配饰保持原样；最终画面必须是两件都穿好的完整状态，不能出现未穿、裸露或脱衣过程；参考图自带的肩带、腰头、绑带、五金、蕾丝、拼接等设计元素不得移除。",
    "look": "模特原有的服装需要替换为参考图中的整套穿搭，不能保留原有单品、层叠关系或图案；最终画面必须是完整穿着状态，不能出现脱衣过程或裸露；参考图自带的配饰、五金、刺绣、拼接等设计元素不得移除。",
}


def _outfit_line(cat_key, cat, outfit):
    outfit = _clean_outfit(outfit)
    if not outfit:
        return ""
    tail = OUTFIT_TAIL.get(cat_key, OUTFIT_TAIL["top"]).format(subject=cat["subject"])
    return f"【原有服装】模特当前穿着{outfit}，{tail}"



def build_prompt_old(payload, fields):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    imgs = payload.get("_picked") or payload.get("images") or []
    if cat_key == "compose" and item_keys:
        cat = _compose_cat(item_keys)
    else:
        cat = CATS.get(cat_key, CATS["top"])
    selected_labels = [ITEM_LABELS[k] for k in item_keys]
    preserved_keys = [k for k in ITEM_LABELS if k not in item_keys]
    if "bra" in item_keys:
        preserved_keys = [k for k in preserved_keys if k != "top"]
    if "panties" in item_keys:
        preserved_keys = [k for k in preserved_keys if k != "bottom"]
    if "dress" in item_keys:
        preserved_keys = [k for k in preserved_keys if k not in ("top", "bottom")]
    preserved_labels = [ITEM_LABELS[k] for k in preserved_keys]
    fabric = (payload.get("fabric") or "").strip()
    notes = (payload.get("notes") or "").strip()
    length_class = fields.get("length_class") or payload.get("length_class") or ""
    sleeve_class = fields.get("sleeve_class") or payload.get("sleeve_class") or ""
    look_lengths = payload.get("look_lengths") or {}
    lengths = payload.get("lengths") or {}
    sleeves = payload.get("sleeves") or {}
    item_lengths = fields.get("item_lengths") or {}
    item_sleeves = fields.get("item_sleeves") or {}
    proportions = fields.get("proportions") or []
    design_details = fields.get("design_details") or []
    if not design_details:
        for label, arr in (fields.get("item_design_details") or {}).items():
            if not isinstance(arr, list):
                arr = [arr]
            design_details.extend(f"{label}：{x}" for x in arr if str(x).strip())
    prop_parts = []
    if cat_key == "compose":
        for k in item_keys:
            lbl = ITEM_LABELS[k]
            lv = str(lengths.get(k) or item_lengths.get(lbl) or "").strip()
            sv = str(sleeves.get(k) or item_sleeves.get(lbl) or "").strip()
            if lv:
                prop_parts.append(f"{lbl}长度：{lv}")
            if sv:
                prop_parts.append(f"{lbl}袖长：{sv}")
        if len(item_keys) == 1:
            k = item_keys[0]
            if not str(lengths.get(k) or "").strip() and length_class:
                prop_parts.append("长度：" + length_class)
            if not str(sleeves.get(k) or "").strip() and sleeve_class:
                prop_parts.append("袖长：" + sleeve_class)
    else:
        if lengths.get(cat_key):
            length_class = str(lengths[cat_key])
        if sleeves.get(cat_key):
            sleeve_class = str(sleeves[cat_key])
        if cat_key == "look":
            lb0 = str(look_lengths.get("bottom") or "").strip()
            ld0 = str(look_lengths.get("dress") or "").strip()
            if lb0:
                prop_parts.append("下装长度：" + lb0)
            if ld0:
                prop_parts.append("裙长：" + ld0)
            if not (lb0 or ld0) and length_class:
                prop_parts.append("长度：" + length_class)
        elif length_class:
            prop_parts.append("长度：" + length_class)
        if sleeve_class:
            prop_parts.append("袖长：" + sleeve_class)
    prop_parts += proportions
    if prop_parts:
        proportion_line = ("【比例锚点】" + "；".join(prop_parts) +
                           "。以模特身体为尺，裙摆/下摆/裤脚必须落在上述位置；不要自行加长、缩短或改变比例。")
        if (payload.get("length_class") or payload.get("sleeve_class")
                or look_lengths.get("bottom") or look_lengths.get("dress")
                or lengths or sleeves):
            proportion_line += "用户指定的长度/袖长优先于读图比例；如冲突，以用户指定为准。"
    else:
        proportion_line = "【比例锚点】以参考图的长度与比例为准；不要自行加长、缩短或改变比例。"

    design_lines = []
    if design_details:
        design_lines.append("【设计细节保留】以下是这件衣服的识别点，必须原样保留，不得简化、抹除、移位、改色或当成瑕疵/背景/其他部件：")
        design_lines.extend(f"{i}. {d}" for i, d in enumerate(design_details, 1))
    else:
        design_lines.append("【设计细节保留】参考图中的不对称、开衩、镂空、绑带、抽绳、褶皱、拼接、撞色、刺绣、印花、五金、异形下摆/领口/袖口等设计元素，必须原样保留，不得简化成基础款或抹除。")
    design_lines.append("参考图服装自带的腰带、绑带、胸针、五金、刺绣、拼接、不对称结构属于设计，不得移除；只移除模特原有的、参考图没有的配饰；不要自行添加参考图中没有的设计元素。")

    LEATHER_WORDS = ("皮革", "牛皮", "羊皮", "真皮", "皮衣", "皮夹克", "皮质", "皮面", "leather")
    leather_text = (fabric + " " + (fields.get("material") or "") + " " +
                    (fields.get("details") or "") + " " + notes).lower()

    def _tagged_leather(text):
        found = []
        for chunk in re.split(r"[；;\n]", text or ""):
            m = re.match(r"\s*(上装|上衣|下装|连衣裙|外套|鞋子|配饰|文胸|内裤)[：:]\s*(.*)", chunk)
            if m and any(w in m.group(2).lower() for w in LEATHER_WORDS):
                found.append(m.group(1))
        return found

    def _tagged_fabric_map(text):
        """把「上衣：真丝；鞋子：皮革」拆成 {件名: 面料}；上装统一成上衣。"""
        out = {}
        for chunk in re.split(r"[；;\n]", text or ""):
            m = re.match(r"\s*(上装|上衣|下装|连衣裙|外套|鞋子|配饰|文胸|内裤)[：:]\s*(.*)", chunk)
            if m and m.group(2).strip():
                label = "上衣" if m.group(1) == "上装" else m.group(1)
                out[label] = m.group(2).strip()
        return out

    leather_main_labels, leather_minor_labels = [], []
    leather_unattributed = False
    if cat_key == "look":
        for st in (fields.get("details") or "").split("；"):
            m = re.match(r"(上装|下装|连衣裙|外套)：(.*)", st)
            if m and any(w in m.group(2).lower() for w in LEATHER_WORDS):
                if m.group(1) not in leather_main_labels:
                    leather_main_labels.append(m.group(1))
        for lbl, val in _tagged_fabric_map(fabric).items():
            if not any(w in val.lower() for w in LEATHER_WORDS):
                continue
            if lbl in ("上衣", "下装", "连衣裙", "外套"):
                if lbl not in leather_main_labels:
                    leather_main_labels.append(lbl)
            elif lbl not in leather_minor_labels:
                leather_minor_labels.append(lbl)
        if (not leather_main_labels and not leather_minor_labels
                and any(w in fabric.lower() for w in LEATHER_WORDS)):
            leather_unattributed = True
    elif cat_key == "compose" or cat.get("compose"):
        tagged = (_tagged_leather(fields.get("details"))
                  + _tagged_leather(fields.get("material"))
                  + _tagged_leather(notes)
                  + _tagged_leather(fabric))
        for tag in tagged:
            key = "top" if tag == "上装" else next(
                (k for k, label in ITEM_LABELS.items() if label == tag), None)
            if not key or key not in item_keys:
                continue
            label = ITEM_LABELS[key]
            if key in ("top", "bottom", "dress", "outer"):
                if label not in leather_main_labels:
                    leather_main_labels.append(label)
            elif label not in leather_minor_labels:
                leather_minor_labels.append(label)
        if not (leather_main_labels or leather_minor_labels):
            keyword_map = {
                "top": ("上衣", "上装", "衬衫", "T恤", "t恤", "针织", "毛衣", "卫衣", "背心", "吊带"),
                "bottom": ("下装", "裤", "半身裙", "短裙", "长裙"),
                "dress": ("连衣裙",),
                "outer": ("外套", "夹克", "大衣", "风衣", "皮衣", "皮夹克"),
                "shoes": ("鞋", "靴"),
                "accessory": ("包", "腰带", "帽", "围巾", "首饰", "配饰"),
                "bra": ("文胸", "胸罩"),
                "panties": ("内裤", "三角裤", "平角裤", "丁字裤"),
            }
            for chunk in re.split(r"[；;\n]", (fields.get("material") or "") + "；" + (fields.get("details") or "")):
                if not any(w in chunk.lower() for w in LEATHER_WORDS):
                    continue
                for key in item_keys:
                    words = keyword_map.get(key, ())
                    if not words or not any(w in chunk for w in words):
                        continue
                    label = ITEM_LABELS[key]
                    if key in ("top", "bottom", "dress", "outer"):
                        if label not in leather_main_labels:
                            leather_main_labels.append(label)
                    elif label not in leather_minor_labels:
                        leather_minor_labels.append(label)
        if not (leather_main_labels or leather_minor_labels) and any(
                w in leather_text for w in LEATHER_WORDS):
            leather_unattributed = True
    else:
        if any(w in leather_text for w in LEATHER_WORDS):
            if cat_key in ("top", "bottom", "dress", "outer"):
                leather_main_labels = [cat["subject"]]
            else:
                leather_minor_labels = [cat["subject"]]

    leather = bool(leather_main_labels or leather_minor_labels or leather_unattributed)
    embossed = any(w in leather_text for w in ("压花", "浮雕", "老花", "monogram"))
    leather_subject = ("参考图中的皮革单品（" + "、".join(leather_main_labels) + "）") if leather_main_labels else ""
    leather_lines = []
    if leather_main_labels:
        if embossed:
            surface = ("压花图案凸起约 1–2mm，边缘圆润隆起、凹陷处有柔和阴影；"
                       "压花凸起处有细微油润光泽，凹陷处更深。")
        else:
            surface = "表面为细密不规则粒面，可见毛孔与天然纹理；接缝、袋盖、下摆边缘有自然厚度与轻微高光。"
        light_surface = "粒面与压花" if embossed else "粒面与接缝"
        lead = (leather_subject + "：") if leather_subject else ""
        leather_lines.extend([
            "【皮革质感】" + lead + "材质是真实皮革，不是 PU/塑料/化纤："
            + surface +
            "下摆、袖口、袋盖边缘可见厚度与深色封边；肩部、肘部、腰侧有自然折痕与轻微应力纹。",
            "【皮革光线】用低角度侧光掠过表面，让" + light_surface + "产生细小高光与阴影；高光宽而柔、不过曝；"
            "金属拉链有细微拉丝/氧化，不是镜面镀铬；不要磨皮、降噪或过度锐化抹平纹理。",
        ])
    if leather_unattributed:
        leather_lines.append(
            "【皮革单品】参考图中标注为皮革的单品按真实皮革处理：保留细密粒面、毛孔、边缘厚度与自然折痕；"
            "不要 PU 亮面或塑料反光；其他单品材质不变。")
    if leather_minor_labels:
        minor = "、".join(leather_minor_labels)
        emboss = "压花/浮雕按参考图凸起，边缘圆润；" if embossed else ""
        leather_lines.append(
            f"【皮革单品】参考图中的{minor}是真实皮革，不是 PU/塑料：保留细密粒面、毛孔、自然折痕与边缘厚度；"
            f"{emboss}不要镜面反光；其他单品材质不变。")

    items = _uploaded_items(payload)
    basis = cat["basis"]
    if cat_key == "look":
        if items:
            bp = [f"{it}的{LOOK_BASIS[it]}" for it in items if it in LOOK_BASIS]
            basis = ("；".join(bp) + "，以及整套的配色与材质关系") if bp else LOOK_ONE_BASIS
        else:
            basis = LOOK_ONE_BASIS

    length_parts = []
    if cat_key == "compose":
        for k in item_keys:
            lbl = ITEM_LABELS[k]
            lv = str(lengths.get(k) or item_lengths.get(lbl) or "").strip()
            if not lv:
                continue
            a, r = _anchor_for(k, lv)
            if a:
                length_parts.append(f"{lbl}：{lv}（{a}）")
            if r and k in RATIO_LABELS:
                length_parts.append(f"{lbl}：{RATIO_LABELS[k]} ≈ {r}")
    elif cat_key == "look":
        lb = str(look_lengths.get("bottom") or "").strip()
        ld = str(look_lengths.get("dress") or "").strip()
        if lb:
            a = LENGTH_ANCHORS["bottom"].get(lb)
            r = LENGTH_RATIOS["bottom"].get(lb)
            if a:
                length_parts.append(f"下装：{lb}（{a}）")
            if r:
                length_parts.append(f"肩线到腰线 : 腰线到裙摆/裤脚 ≈ {r}")
        if ld:
            a = LENGTH_ANCHORS["dress"].get(ld)
            r = LENGTH_RATIOS["dress"].get(ld)
            if a:
                length_parts.append(f"裙长：{ld}（{a}）")
            if r:
                length_parts.append(f"肩线到腰线 : 腰线到裙摆 ≈ {r}")
    else:
        a, r = _anchor_for(cat_key, length_class)
        if a:
            length_parts.append(f"长度：{length_class}（{a}）")
        if r:
            length_parts.append(f"{RATIO_LABELS.get(cat_key, '')} ≈ {r}")

    if cat["lingerie"]:
        length_lock = ""
    elif length_parts:
        length_lock = ("【长度锁定】" + "；".join(length_parts) +
                       "。下摆/裙摆/裤脚的位置不得移动，不得加长、缩短、改短或改成另一种长度；参考图产品图的下摆位置是唯一标准。")
    else:
        length_lock = "【长度锁定】下摆/裙摆/裤脚位置以参考图产品图为准：不得加长、缩短、改短或移动下摆。"

    separation = ""
    if cat_key == "look":
        if "上装" in items and "下装" in items:
            separation = ("【单品分离】上装与下装是两件独立单品，不能合并成连衣裙、衬衫裙或连体衣；"
                          "腰线处应看到上装下摆与下装腰头/腰带的分界。")
        elif "连衣裙" in items:
            separation = "【单品分离】连衣裙是独立一件，不要拆成上下装；不要把连衣裙改成两件套。"

    fabric_tags = _tagged_fabric_map(fabric)
    fabric_tag_line = "；".join(f"{lbl} {val}" for lbl, val in fabric_tags.items())
    material = _trim(_sanitize(fields.get("material"))).rstrip("。")
    if not material:
        material = "颜色、图案与质感以参考图为准"
    if fabric_tag_line:
        material = f"{material}；用户标注面料（按件）：{fabric_tag_line}，以参考图质感为准"
    elif fabric:
        material = f"{material}；用户标注面料：{fabric}，以参考图质感为准"
    details = _trim(_sanitize(fields.get("details")))
    if not details or (not notes and not fields.get("_vision")):
        # 没有读图、也没有补充说明时，不让文字模型猜具体部件
        details = "按参考图核对还原全部部件，不做自行设计"
    styling = _trim(_sanitize(fields.get("styling"))) or "正面或微侧 3/4 自然站姿，镜头平视"
    light_raw = _trim(_sanitize(fields.get("light")))
    background_raw = _trim(_sanitize(fields.get("background")))
    extras = _clean_list(fields.get("negatives"), 6)
    has_model = any(str(im.get("role")) == "人物模特" for im in imgs)
    ref_person = "参考模特图" if has_model else "参考图中的人物"
    if has_model:
        hard_lock = (f"【硬约束】保持{ref_person}原有动作、站姿、身体朝向和手部位置；"
                     "最终画面必须是完整穿着状态，不能脱衣、裸露或展示换衣过程。")
    else:
        hard_lock = ("【硬约束】最终画面必须是完整穿着状态，不能脱衣、裸露或展示换衣过程；"
                     "人物姿势自然，双手自然下垂或按商品图常见站姿。")
    if cat["lingerie"]:
        if "bra" in item_keys and "panties" not in item_keys:
            hard_lock += "产品必须完整穿在模特身上，不能出现未穿上衣。"
        elif "panties" in item_keys and "bra" not in item_keys:
            hard_lock += "产品必须完整穿在模特身上，不能出现未穿下装。"
        else:
            hard_lock += "产品必须完整穿在模特身上，不能出现未穿上衣/未穿下装。"
    if leather:
        if leather_main_labels:
            leather_lead = leather_subject
        elif leather_minor_labels:
            leather_lead = "参考图中的" + "、".join(leather_minor_labels)
        else:
            leather_lead = "参考图中标注为皮革的单品"
        hard_lock += f" {leather_lead}是真实皮革，不是平面印花：必须保留粒面、边缘厚度和自然折痕，其他单品材质不变。"
    if cat.get("compose") or (cat_key == "set" and item_keys) or (len(item_keys) == 1 and item_keys[0] == cat_key):
        outfit = _clean_outfit(fields.get("existing_outfit")) if has_model else ""
        chosen = "、".join(selected_labels)
        if not has_model:
            outfit_line = (f"【原有服装】本次只替换：{chosen}；其他单品、包、姿势按本次生成的人物与构图保持一致；"
                           "没有模特图时按商品图自然站姿生成。")
        elif outfit:
            outfit_line = f"【原有服装】模特当前穿着{outfit}。本次只替换：{chosen}；除上述单品外的其他单品、包、姿势保持原样。"
        else:
            outfit_line = f"【原有服装】本次只替换：{chosen}；除上述单品外的其他单品、包、姿势保持原样。"
        if "dress" in item_keys:
            outfit_line += "连衣裙覆盖上装与下装，原有上装/下装由连衣裙替代；外套、包、鞋子、配饰与姿势保持原样。"
        if cat["lingerie"]:
            outfit_line += "对应区域原有衣物可替换为内衣；最终必须是穿着状态，不能出现未穿、裸露或脱衣过程。"
    elif cat_key == "look" and (payload.get("set_mode") or "one") == "one" and not has_model:
        outfit_line = "【原有服装】以参考图中的整套穿搭为准，不引入参考图以外的任何单品、层叠关系或图案。"
    else:
        outfit_line = _outfit_line(cat_key, cat, fields.get("existing_outfit"))
        if not outfit_line:
            fallback = OUTFIT_FALLBACK.get(cat_key, OUTFIT_FALLBACK["top"])
            outfit_line = "【原有服装】" + fallback.format(subject=cat["subject"])

    forbid = cat["forbidden"]
    terms = forbid.split("、")
    keep = []
    for x in extras:
        if x in forbid or any(x in f for f in terms) or any(f in x for f in terms):
            continue
        cross = {"panties": ("文胸", "胸罩"), "bra": ("内裤", "三角裤", "平角裤", "丁字裤")}
        if any(w in x for w in cross.get(cat_key, ())):
            continue
        if any(w and w in x for w in selected_labels):
            continue
        if any(w in x for w in ("透明", "透视", "半透明", "真空")):
            continue
        if x not in keep:
            keep.append(x)
    keep = keep[:4]
    extra_txt = ("另外禁止：" + "、".join(keep) + "。") if keep else ""

    selected_labels = [ITEM_LABELS[k] for k in item_keys]
    keep_tail = "保持与参考模特图一致。" if has_model else "按本次生成的人物与构图保持一致。"
    cat_lock = cat["lock"]
    if not has_model:
        cat_lock = cat_lock.replace("参考模特图", "本次生成的人物与构图")
    compose_lines = []
    if cat.get("compose"):
        compose_lines = [
            "【本次替换】" + "、".join(selected_labels) + "。",
            "【保持不变】除上述单品外的所有服装、鞋子、配饰、包、姿势、双手与手指、头发、妆容、背景、光线、机位、构图" + keep_tail,
            "【单品锁定】" + "；".join(ITEM_LOCKS[k] for k in item_keys) + "。",
        ]
        if len(item_keys) >= 2:
            if (payload.get("set_mode") or "one") == "two":
                compose_lines.append("【组合参考】参考图中的单品是按单品分别上传的，需要同时穿在模特身上，保持各件的款式、配色与层叠关系。")
            else:
                compose_lines.append("【组合参考】参考图为多件同框合影，按图中各单品的搭配关系、层叠顺序与配色同时还原，不要只取其中一件。")
            compose_lines.append("【组合单品】本次选中的 " + str(len(item_keys)) + " 件：" + "、".join(selected_labels)
                                 + "；必须全部出现在画面中，不能漏掉、替换或用其他款式代替。")
        if "bra" in item_keys or "panties" in item_keys:
            compose_lines.append("对应区域原有外层衣物可替换为内衣；其他部位/单品保持原样；最终必须是穿着状态，不能出现未穿、裸露或脱衣过程。")
        if "top" in item_keys and "bottom" in item_keys:
            compose_lines.append("【单品分离】上装与下装是两件独立单品，不能合并成连衣裙、衬衫裙或连体衣；腰线处应看到上装下摆与下装腰头/腰带的分界。")
    elif cat_key == "set" and len(item_keys) >= 2:
        compose_lines = [
            "【本次替换】" + "、".join(selected_labels) + "。",
            "【保持不变】除上述单品外的所有服装、鞋子、配饰、包、姿势、双手与手指、头发、妆容、背景、光线、机位、构图" + keep_tail,
            "【单品锁定】" + "；".join(ITEM_LOCKS[k] for k in item_keys) + "。",
            "对应区域原有外层衣物可替换为内衣；其他部位/单品保持原样；最终必须是穿着状态，不能出现未穿、裸露或脱衣过程。",
        ]
    elif len(item_keys) == 1 and item_keys[0] == cat_key:
        k = item_keys[0]
        compose_lines = [
            f"【本次替换】只替换{ITEM_LABELS[k]}。",
            "【保持不变】除上述单品外的所有服装、鞋子、配饰、包、姿势、双手与手指、头发、妆容、背景、光线、机位、构图" + keep_tail,
            f"【单品锁定】{ITEM_LOCKS[k]}。",
        ]
        if k in ("bra", "panties"):
            compose_lines.append("对应区域原有外层衣物可替换为内衣；其他部位/单品保持原样；最终必须是穿着状态，不能出现未穿、裸露或脱衣过程。")

    ref_lines = []
    for i, im in enumerate(imgs, 1):
        role = str(im.get("role") or "")
        label = str(im.get("label") or "")
        if role == "人物模特":
            desc = "模特图（只提供人物、姿势、场景、光线，不作为服装依据）"
        elif role == "局部细节":
            desc = f"局部细节（{label}）" if label else "局部细节"
        elif role in ("整套单品", "group"):
            desc = (f"组合参考图（{'、'.join(selected_labels)}同框）"
                    if len(selected_labels) > 1 else "组合参考图")
        elif role in ("整套穿搭", "look"):
            desc = "整套穿搭合影（多件同框）"
        elif role in ("套装", "文胸内裤套装"):
            desc = "套装合影（文胸+内裤同框）"
        else:
            desc = f"整件服装（{role or '服装'}）"
        ref_lines.append(f"参考图{i}：{desc}")
    ref_map = ""
    if ref_lines:
        ref_map = ("【参考图】" + "；".join(ref_lines) +
                   "。生成时保持参考图顺序与此一致；局部细节只用于结构/纹理，模特图只用于人物/姿势/场景。")

    main_roles = [str(im.get("role") or "") for im in imgs
                  if str(im.get("role") or "") not in ("局部细节", "人物模特")]
    multi_cat = cat_key
    if cat_key == "compose" and len(item_keys) == 1:
        # 组合里只选一件时，多角度就是这件单品自己的多角度
        multi_cat = item_keys[0]
    multi_angle_line = ""
    if len(main_roles) > 1 and multi_cat not in ("look", "compose", "set"):
        multi_angle_line = ("【多角度参考】参考图中的多张整件服装是同一件衣服的不同角度/细节，不是不同单品；"
                            "正面优先确认主结构、颜色和图案，背面补后领、育克、后背拼接、后腰、后片，"
                            "侧面补侧缝、开衩、袖型和廓形。不要把不同角度当成两件衣服，"
                            "也不要因为背面看不到就删掉正面已经确认的部件。")

    lines = [
        f"【任务】{cat['task']}",
        "",
        hard_lock,
        "",
        *([ref_map, ""] if ref_map else []),
        *([multi_angle_line, ""] if multi_angle_line else []),
        f"【品类锁定】{cat_lock}",
        "",
        *compose_lines,
        f"【款式依据】以参考图中的{cat['subject']}为唯一款式依据，严格 1:1 还原：{basis}。{details}。",
        proportion_line,
        *([length_lock] if length_lock else []),
        *([separation] if separation else []),
        *design_lines,
        f"【面料与颜色】{material}。",
        *leather_lines,
        f"【穿着边界】{cat['boundary']}",
    ]
    if outfit_line:
        lines.append(outfit_line)
    lines.append(f"【禁止项】不要生成{forbid}。{extra_txt}{cat['tail']}")
    no_scene = ("【禁止画面】不要出现未着装、未穿上衣、未穿下装、裸露、脱衣或换衣过程、"
                "用手或道具遮挡身体、动作/站姿/身体朝向与参考图不一致、重新摆姿势。")
    if cat_key == "look":
        no_scene += "不得把上装与下装合并成连衣裙/衬衫裙。"
    if not cat["lingerie"]:
        no_scene += "不得加长或缩短裙摆/下摆、不得改变手的位置或包的拿法。"
    if cat.get("compose") or (cat_key == "set" and item_keys) or (len(item_keys) == 1 and item_keys[0] == cat_key):
        no_scene += "不得改动未选中的单品、姿势、包或背景。"
    lines.append(no_scene)
    if leather:
        lines.append("【皮革排除】不要 PU 亮面、塑料反光、镜面高光、平面印花、热转印、"
                     "磨皮/降噪抹平粒面、油亮涂层、过度锐化、CG/3D 渲染、纸板感。")
    if cat["label"] == "文胸内裤两件套":
        if (payload.get("set_mode") or "one") == "two":
            lines.append("【两件参考】参考图中的文胸图与内裤图分别是两件单品，需要同时穿在模特身上，保持同一系列的配色与工艺。")
        else:
            lines.append("【两件参考】参考图为成套合影，按图中两件的配色与系列关系还原，不要拆成不同系列。")
    elif cat["label"] == "整套 Look":
        if (payload.get("set_mode") or "one") == "two":
            lines.append("【整套参考】参考图中的单品是按上装/下装/连衣裙/外套/鞋子/配饰分别上传的，需要同时穿在模特身上，保持原图的搭配关系、层叠顺序与配色。")
        else:
            lines.append("【整套参考】参考图为整套穿搭合影，按图中各单品的搭配关系、层叠顺序与配色还原，不要拆开或替换。")
        if items:
            lines.append("【整套单品】本次参考图包含：" + "、".join(items) + "；必须全部出现在画面中，不能漏掉或替换。")
        else:
            lines.append("【整套单品】以参考图中实际出现的单品为准，逐件还原，不得漏掉或替换。")
    if notes and not _notes_covered(notes, material + details):
        lines.append(f"【补充说明】{notes}")

    compliance = LINGERIE_COMPLIANCE if cat["lingerie"] else GENERAL_COMPLIANCE
    combined = (fields.get("details") or "") + (fields.get("material") or "") + notes + "".join(design_details)
    if leather:
        detail_text = ("皮革粒面、压花浮雕、缝线针孔与金属拉链质感清晰"
                       if embossed else "皮革粒面、缝线针孔与金属拉链质感清晰")
    elif "蕾丝" in combined:
        detail_text = "面料纹理、蕾丝细节与走线清晰"
    elif any(w in combined for w in ("刺绣", "拼接", "五金", "镂空", "绑带", "褶皱", "抽绳", "撞色", "不对称")):
        detail_text = "面料纹理、设计细节与缝线走线清晰"
    else:
        detail_text = "面料纹理、结构缝线与拼接走线清晰"
    ref_person = "参考模特图" if has_model else "参考图中的人物"
    action_lock = (f"【动作锁定】模特的站姿、身体朝向、双手与手指的位置、头部角度、双脚站姿必须与{ref_person}一致；"
                   "不要重新设计动作、不要转身、不要改变重心；镜头为平视商品图。")
    if cat["lingerie"]:
        action_lock += "不要为了展示产品而改变动作、转身或脱衣。"
    elif "accessory" in item_keys:
        action_lock += "配饰/包的佩戴位置与拿法保持不变，配饰本身按参考图替换。"
    else:
        action_lock += "包的拿法与位置保持不变。"
    replace_lock = ("【局部替换】只替换目标服装；人物的姿势、双手与手指、头部角度、双脚站姿、背景、光线、机位必须与"
                    f"{ref_person}一致；不要重新构图、不要重绘人物；除目标服装外，其他单品/道具按【原有服装】处理。")
    expression_lock = (f"【表情锁定】面部表情、眼神方向、眉形、嘴角与{ref_person}一致；"
                       "不要改变表情、不要新增微笑/大笑/皱眉/闭眼，不要换脸或改五官。")
    silhouette_lock = ("【版型锁定】服装版型必须与参考图一致：肩线位置、袖笼与袖型、胸腰臀余量、下摆宽度与弧度、"
                       "腰头高度、裤型/裙型、裤脚/裙摆宽度；不得改紧身或宽松、不得缩短或加长、"
                       "不得改变轮廓与垂坠。")

    consistency = (f"模特的脸型、发型、肤色、动作、站姿与身体朝向必须与{ref_person}一致；只更换服装，不改动人物本身。")
    if has_model:
        light_line = (f"【光线与背景】{light_raw or '柔和均匀'}；背景沿用{ref_person}，"
                      "不要改成纯色棚拍背景；画面干净无杂物，聚焦产品。")
        background_line = (f"【背景锁定】参考模特图的背景：{background_raw or '以参考模特图为准'}；"
                           "保持背景的墙体/地面/家具/道具/颜色/材质/景深/位置不变，"
                           "不要换成纯色、白底或新的棚拍背景；只替换目标服装，不重绘背景。")
    else:
        light_line = (f"【光线与背景】{light_raw or '柔和均匀的商业棚拍光，纯色或浅灰背景'}；"
                      "画面干净无杂物，聚焦产品。")
        background_line = ""
    if has_model:
        model_line = (f"【模特与姿态】人物、脸型、发型与表情沿用{ref_person}，{styling}；"
                      f"取景{cat.get('frame', '腰部以上')}，重点展示服装结构与面料。")
    else:
        model_line = (f"【模特与姿态】成年女性模特，{styling}；表情平静自然；"
                      f"取景{cat.get('frame', '腰部以上')}，重点展示服装结构与面料。")
    if has_model:
        output_line = ("【输出】写实商业摄影质感，" + detail_text +
                       "，颜色准确，不磨皮过度；画幅比例与参考模特图一致，主体位置与构图不变，"
                       "适合电商主图与详情页。")
    else:
        output_line = ("【输出】写实商业摄影质感，" + detail_text +
                       "，颜色准确，不磨皮过度；3:4 竖构图，主体居中，画面上方留白，"
                       "适合电商主图与详情页。")
    lines += [
        "【画面人数】画面只出现一位模特，单人构图；不要拼图、不要分屏、不要重复模特、不要多人合影。",
        model_line,
        action_lock,
        expression_lock,
        silhouette_lock,
        replace_lock,
        f"【合规】{compliance}",
        light_line,
        *([background_line] if background_line else []),
        output_line,
        f"【一致性】{consistency}",
    ]
    return "\n".join(lines)



# ---------------------------------------------------------------- 新版提示词（实验）
# 与 build_prompt_old 并存；只有 payload.prompt_version == "new" 时才走这里。
# 设计依据：编辑场景先写“只改什么/其他不动”，再给版型与设计细节；Nano 用更短的语义蒙版式句子。

def _prompt_sections(text):
    out = {}
    cur = None
    for line in (text or "").splitlines():
        m = re.match(r"^【([^】]+)】", line.strip())
        if m:
            cur = m.group(1).strip()
            out.setdefault(cur, "")
            out[cur] += line.strip() + "\n"
        elif cur:
            out[cur] += line + "\n"
    return {k: v.strip() for k, v in out.items()}


def _section_body(section):
    return re.sub(r"^【[^】]+】", "", section or "").strip()


def _picked_images(payload):
    return payload.get("_picked") or payload.get("images") or []


def _model_image_index(payload):
    for i, im in enumerate(_picked_images(payload), 1):
        if str(im.get("role") or "") == "人物模特":
            return i
    return len(_picked_images(payload))


def _item_image_map(payload):
    """返回 {单品 key: 参考图序号}，用于组合时写清“哪件用哪张图”。"""
    item_keys = _item_keys(payload)
    mapping = {}
    for i, im in enumerate(_picked_images(payload), 1):
        role = str(im.get("role") or "")
        if role in ("局部细节", "人物模特"):
            continue
        for k in item_keys:
            if k in mapping:
                continue
            if role == k or role.startswith(k) or ITEM_LABELS[k] in role:
                mapping[k] = i
                break
    return mapping


def _new_target_phrase(payload):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    labels = [ITEM_LABELS[k] for k in item_keys]
    if cat_key == "look":
        return "整套穿搭", "参考图1中的整套服装"
    if cat_key == "accessory":
        return "包/配饰", "参考图1中的包/配饰"
    if cat_key == "shoes":
        return "鞋", "参考图1中的鞋"
    if cat_key == "set":
        return "文胸和内裤", "参考图1、参考图2中的文胸和内裤"
    if cat_key == "compose" and len(item_keys) > 1:
        m = _item_image_map(payload)
        parts = []
        for k in item_keys:
            parts.append(f"{ITEM_LABELS[k]}用参考图{m.get(k, 1)}")
        return "、".join(labels), "、".join(parts)
    if labels:
        return labels[0], "参考图1中的服装"
    return "服装", "参考图1中的服装"


def _new_edit_instruction(payload):
    cat_key = payload.get("category") or "top"
    model_idx = _model_image_index(payload)
    target, source = _new_target_phrase(payload)
    if cat_key == "accessory":
        return (f"以参考图{model_idx}为底图，只把人物身上的包/配饰换成{source}；"
                "包的拿法、佩戴位置和人物本身保持原样。")
    if cat_key == "shoes":
        return (f"以参考图{model_idx}为底图，只把人物脚上的鞋换成{source}；"
                "裤脚/裙摆、双脚位置、站姿和人物本身保持原样。")
    if cat_key == "look":
        return (f"以参考图{model_idx}为底图，只把人物身上的整套穿搭换成{source}；"
                "人物本身和其他所有元素保持原样。")
    return (f"以参考图{model_idx}为底图，只把人物身上的{target}换成{source}；"
            "人物本身和其他所有元素保持原样。")


def _new_details(payload, fields, old_sections):
    basis = _section_body(old_sections.get("款式依据", ""))
    material = _section_body(old_sections.get("面料与颜色", ""))
    design = []
    raw_design = _section_body(old_sections.get("设计细节保留", ""))
    for line in raw_design.splitlines():
        m = re.match(r"^\d+\.\s*(.+)$", line.strip())
        if m:
            design.append(m.group(1).strip())
    if not design and raw_design:
        design = [raw_design[:120]]
    prop = _section_body(old_sections.get("比例锚点", ""))
    length = _section_body(old_sections.get("长度锁定", ""))
    # 长度锁定只取第一句，避免和比例锚点重复
    length = length.split("。")[0].strip()
    return basis, material, prop, length, design[:4]


def _new_preserve_sentence(payload):
    item_keys = _item_keys(payload)
    if any(k in ("bra", "panties") for k in item_keys):
        return ("人物脸、表情、眼神、发型、姿势、双手、身体、其他服装、鞋、包、配饰、背景、光线、机位和画幅保持完全不变；"
                "最终画面必须是完整穿着状态，不裸露、不脱衣、不展示换衣过程。")
    return ("人物脸、表情、眼神、发型、姿势、双手、身体、其他服装、鞋、包、配饰、背景、光线、机位和画幅保持完全不变。")


def _new_ref_line(old_sections):
    ref = old_sections.get("参考图", "")
    multi = old_sections.get("多角度参考", "")
    return "\n".join(x for x in (ref, multi) if x)


def _new_scope_sentence(payload):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    if cat_key == "dress":
        return "连衣裙覆盖并替代模特原有的上衣和下装；不要保留原上衣、原裤子或原裙子。"
    if cat_key == "top":
        return "只替换上衣；下装、外套、鞋、包和其他配饰保持原样。"
    if cat_key == "bottom":
        return "只替换下装；上衣、外套、鞋、包保持原样。"
    if cat_key == "outer":
        return "只替换外套；内搭、下装、鞋、包保持原样。"
    if cat_key == "look":
        return "整套穿搭替换；鞋和包按参考图，人物、姿势和背景保持原样。"
    if cat_key == "shoes":
        return "只替换鞋；裤脚或裙摆、双脚位置和站姿保持原样。"
    if cat_key == "accessory":
        return "只替换包或配饰；拿法和佩戴位置保持原样。"
    if cat_key == "set" or any(k in ("bra", "panties") for k in item_keys):
        return "文胸和内裤都穿好，外层衣物按参考图替换；最终必须是完整穿着状态。"
    return "只替换选中的单品；未选中的衣服、鞋、包和配饰保持原样。"


def build_prompt_new(payload, fields, target_model=""):
    old = build_prompt_old(payload, fields)
    sections = _prompt_sections(old)
    instruction = _new_edit_instruction(payload)
    ref = _new_ref_line(sections)
    basis, material, prop, length, design = _new_details(payload, fields, sections)
    preserve = _new_preserve_sentence(payload)
    output = _section_body(sections.get("输出", "")) or "写实商业摄影质感，颜色准确，不磨皮过度，适合电商主图与详情页。"
    detail_text = "；".join(design)
    is_nano = "nano" in str(target_model or "").lower()

    if is_nano:
        short = basis
        if detail_text:
            short = (short + "；必须保留：" + "、".join(design[:2])) if short else ("必须保留：" + "、".join(design[:2]))
        if len(short) > 260:
            cut = short[:260]
            pos = max(cut.rfind("；"), cut.rfind("。"))
            short = (cut[:pos] if pos > 120 else cut).rstrip("；，,。 ") + "。"
        lines = [
            instruction,
            _new_scope_sentence(payload),
            preserve,
            ("目标单品细节：" + short) if short else "",
            "不要改人物、未选中的单品、背景或光线；不要新增文字、logo、配饰或多余人物。",
        ]
        return "\n".join(x for x in lines if x)

    # Seedream 版：压缩到 8 个块，保留款式/比例/设计，去掉重复否定与重复【原有服装】
    lines = [
        "【编辑指令】" + instruction,
        "",
        *([ref, ""] if ref else []),
        "【目标单品】" + basis.rstrip("；。 ") +
        (("；面料与颜色：" + material.rstrip("；。 ")) if material else "") + "。",
        "【版型与长度】" + "；".join(x.rstrip("；。 ") for x in (prop, length) if x) +
        "。不得改紧、改松、改长、改短或改变轮廓。",
        "【设计细节】" + (detail_text or "参考图中的图案、五金、拼接、开衩、褶皱等设计元素原样保留。") +
        "。不得简化、抹除、移位、改色或当成瑕疵。",
        "【必须保持】" + preserve,
        "【不要改变】不要改人物身份、脸、表情、姿势、手和身体；不要改未选中的衣服、鞋、包、配饰、背景、光线、机位和画幅；不要新增参考图没有的单品、图案、文字或 logo；不要拼接、分屏或出现多位模特。",
        "【输出】" + output,
    ]
    return "\n".join(x for x in lines if x is not None).replace("\n\n\n", "\n\n")


def build_prompt(payload, fields):
    version = str(payload.get("prompt_version") or "").lower()
    if version == "new":
        return build_prompt_new(payload, fields, str(payload.get("target_model") or ""))
    return build_prompt_old(payload, fields)


# ---------------------------------------------------------------- 姿势裂变

POSE_SOURCE_SYSTEM_PROMPT = """你是电商人像摄影的视觉分析员。用户上传一张原图，后续要在保持人物、服装、场景、背景、光线、机位完全不变的前提下，只替换模特的动作。请把原图中所有需要锁定的视觉事实提取出来，供图像生成提示词使用。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。键名必须严格使用下面给出的英文键名。
2. 只写图中看得见的事实。看不清的细节写“以原图为准”，不要凭常识补写或猜测。
3. 不描述身体敏感部位，不使用挑逗性、性暗示措辞；正常描述肤色、妆容与服装即可。
4. 服装逐件描述：品类、颜色、材质或肌理、版型、长度、领型与领口、门襟与开合方式（拉链/纽扣/系带/套头/交叉绑带）、袖型与袖口形状、腰头、裤脚或裙摆、口袋、开衩、接缝与结构线（公主线、拼缝、褶裥方向）、五金（拉链头、扣子、铆钉、抽绳头）、图案与纹理方向、穿着方式（塞入、敞开、挽起等）。看得见的细节必须写全，尤其是「会不会被模型改掉」的地方：袖口是收口还是喇叭、门襟是拉链还是系带、领口有没有翻领或打结。配饰与鞋子单列，写清颜色、材质与佩戴位置；鞋要写清鞋型、鞋跟、靴筒高度、绑带数量。只写服装本身与穿法，不写手的动作、身体姿态或人物正在做的动作。
   所有看得见的服装必须全部写入 outfit 数组，不能只写一件，也不能把服装写进 note。裙装与裤装必须区分：有裙摆、无裤裆的是裙子；有两条裤腿、有裤裆的是裤子；看不清具体品类时写“下装（裙或裤，以原图为准）”，不要猜成另一种。
5. scene 写地点类型与主要实体（墙面、窗户、地面、家具、道具等）；background 写背景元素分布与虚化程度。
6. camera 写机位高度、镜头感觉、取景范围（全身/七分身/半身/近景）、人物在画面中的位置、画幅比例。另外必须用 framing 字段单选取景级别：画面下缘在胸部以上写“近景”，在腰部附近写“半身”，在大腿中部附近写“七分身”，到脚或以下写“全身”。framing 必须与画面实际下缘一致，不要写和画面矛盾的取景描述。
7. pose 只写原图当前的动作，供后续替换时对照，不展开。
8. props 只写道具本身、外观与原位置，不写手怎么拿、手臂怎么抱或怎么扶。
9. note 只写多人、遮挡、被画幅裁切、水印等需要说明的画面情况，不写服装、配饰、道具、手部或姿势细节。
10. 如果图中没有人物，has_person 为 false；如果有多个主要人物，只分析最居中的那一位，并在 note 说明。
11. 所有字段值用简体中文。

输出 JSON（键名固定）：
{
  "has_person": true,
  "person": {
    "gender_age": "性别与年龄段",
    "face": "脸型、五官、眉形、眼型、鼻型、唇形",
    "skin": "肤色与肤质",
    "hair": "发型、发色、长度、分缝与碎发",
    "makeup": "妆容",
    "body": "身形与身材比例",
    "expression": "表情与神态"
  },
  "outfit": ["每件服装一条，含品类、颜色、材质、版型、长度、领口袖型、图案与穿法"],
  "accessories": ["每件配饰或鞋子一条，含颜色、材质与佩戴位置"],
  "props": "人物手持或身边的道具与摆放位置（不是配饰）；没有就留空",
  "scene": "地点类型与主要实体",
  "background": "背景元素分布与虚化程度",
  "light": "光线方向、软硬、色温与明暗对比",
  "camera": "机位高度、镜头感、取景范围、人物位置与画幅比例",
  "framing": "近景/半身/七分身/全身（单选，按画面下缘判断）",
  "style": "影调、色彩倾向与画面质感",
  "pose": "原图当前的动作概述",
  "note": ""
}"""

POSE_REF_SYSTEM_PROMPT = """你是人体姿势与神态分析员。用户会给你姿势参考图，可能是低清、模糊、背景杂乱、服装与本次任务无关的网图。你只需要提取图中人物的动作、姿态以及表情神态，完全不关心人物身份、长相、服装、场景、背景、道具和画质。

硬性要求：
1. 只输出一个 JSON 对象，不要 Markdown，不要解释，不要代码块。键名必须严格使用下面给出的英文键名，所有字段值用简体中文。
2. 只分析画面中最主要的那个人。若有多人，选择占据画面最大、姿势最完整的一位，并在 note 说明。
3. 先判断身体相对镜头的朝向，再写动作。orientation_deg 只能从「正面0° / 前侧45° / 正侧90° / 背侧135° / 背面180°」里单选，表示身体正面相对镜头转过去多少度；facing 写面部朝向。侧身、回眸、转身、行走的图必须如实写成 45° / 90° / 135° / 180°，不要写成正面站立。
4. 动作要写到可以照着复刻：站/坐/蹲/倚靠、重心落在哪条腿；双腿前后关系、步幅（双腿并拢/小步/跨步/单腿抬起）、膝盖弯曲、脚尖方向、脚跟是否离地；双臂抬起高度与肘部角度、左右差异；双手位置、手掌朝向、手指状态；躯干直立/前倾/后仰/侧弯/扭转；双肩平齐或一高一低；头部朝向与倾斜；视线方向；整体动势（静态摆姿/行走中定格/转身中/跑动跳跃/坐定/倚靠）。所有左右一律用画面左右描述：写「画面左侧的手」「面部朝向画面右侧」「靠画面左侧的腿在前」，不要写「人物自身的左手/右手/左侧/右侧」；分不清时写「靠近镜头的一侧 / 远离镜头的一侧」，避免后续生成镜像。
5. 表情与神态分项写清楚：brows 眉毛（平展/上扬/微蹙/放松）、eyes 眼睛（睁大/微眯/下垂/上抬、眼神强弱）、mouth 嘴部（嘴角上扬/平直/下撇、是否露齿、唇部是否放松）、jaw 下颌（放松/收紧/抬起/内收），再在 expression 用一句自然语言概括整体表情，在 mood 用 2–4 个词写整体情绪与气质。脸部被裁掉或看不清时，这几项都写“不可见”，不要猜。
6. outline 用一句话写身体剪影，例如「侧身跨步、上身前倾、一臂前摆、回头看向镜头」。
7. 不写服装、长相、脸型、五官形状、年龄、身份、场景、背景、道具或画质。
8. 如果人物拿着或扶着东西，只写手的位置、角度、朝向与手型，不要提及任何物体、道具或包袋，也不要求保留它们；不写“搭在物体上”这类说法。
9. 被画面裁掉或被遮挡的部位，在 occluded 写清哪些部位不可见；不要编造这些部位的具体动作。
10. 看不清时 confidence 写“低”，不要猜手指等细节。
11. 如果图中没有人物，has_person 为 false，其余动作字段留空。
12. 所有字段值用简体中文。

输出 JSON（键名固定，值用简体中文）：
{
  "has_person": true,
  "orientation": "身体相对镜头的朝向，写清转过去多少度、面朝画面哪一侧",
  "orientation_deg": "正面0°/前侧45°/正侧90°/背侧135°/背面180°（单选）",
  "facing": "正对镜头/画面左侧/画面右侧/背对镜头",
  "stance": "站立/坐/蹲/倚靠等，重心落在哪条腿（用画面左右描述）",
  "gait": "双腿并拢/小步/跨步/单腿抬起/站定无步伐，写清画面左侧还是右侧的腿在前",
  "legs": "双腿分开或交叉、前后关系、步幅、膝盖弯曲程度、脚尖方向、脚跟是否离地",
  "arms": "双臂抬起/下垂/叉腰等，肘部角度与画面左右差异",
  "hands": "双手位置（画面左/右侧、靠近/远离镜头）、手掌朝向、手指状态",
  "spine": "躯干直立/前倾/后仰/侧弯/扭转",
  "shoulders": "双肩平齐/一高一低/耸起/后展",
  "head": "头部朝向（画面左/右）、倾斜角度、下巴高度",
  "face_visible": "回望或侧脸时画面中露出的脸颊（左脸颊/右脸颊/不可见）",
  "near_side": "靠近镜头的一侧（画面左侧/画面右侧/正对）",
  "gaze": "视线方向",
  "outline": "一句话写身体剪影",
  "brows": "眉部状态",
  "eyes": "眼睛状态与眼神强弱",
  "mouth": "嘴部状态：嘴角、是否露齿、唇部松紧",
  "jaw": "下颌状态",
  "expression": "整体表情，一句自然语言",
  "mood": "整体神态与情绪，2–4 个词",
  "dynamic": "静态摆姿/行走中定格/转身中/跑动跳跃/坐定/倚靠",
  "framing": "参考图取景范围",
  "occluded": "被裁掉或遮挡、无法看清的部位",
  "confidence": "高/中/低",
  "note": ""
}"""

POSE_SOURCE_MAX_TOKENS = 4000
POSE_REF_MAX_TOKENS = 3200
POSE_TIMEOUT = 180

POSE_COMPLIANCE = (
    "正常电商人像摄影，不是情色内容。画面不展示身体敏感部位；"
    "姿态自然，不使用透视、湿身、过度紧身或挑逗性姿势。"
)
POSE_LINGERIE_COMPLIANCE = (
    "本图用于电商内衣类目商品展示，属于正常商业产品摄影，不是情色内容。"
    "画面不展示身体敏感部位；姿态自然，不使用透视、湿身、过度紧身勾勒、挑逗性姿势或表情；"
    "遵守主流电商平台内衣类目审核规范。"
)
POSE_LINGERIE_WORDS = ("文胸", "胸罩", "内裤", "内衣", "bra", "panties", "泳装", "比基尼")

POSE_FIELD_LABELS = [
    ("orientation", "身体朝向"),
    ("stance", "姿态与重心"),
    ("gait", "步伐"),
    ("legs", "双腿"),
    ("arms", "双臂"),
    ("hands", "双手"),
    ("spine", "躯干"),
    ("shoulders", "肩部"),
    ("head", "头部"),
    ("gaze", "视线"),
    ("outline", "身体剪影"),
    ("dynamic", "整体动势"),
]

POSE_KEY_ALIASES = {
    "orientation": ("orientation", "朝向", "身体朝向", "身体方向", "body_orientation"),
    "orientation_deg": ("orientation_deg", "朝向角度", "身体角度", "转角", "角度", "body_angle"),
    "facing": ("facing", "面部朝向", "面朝", "face_direction"),
    "stance": ("stance", "站姿", "站立", "重心", "posture", "standing"),
    "gait": ("gait", "步伐", "步态", "迈步", "stride"),
    "legs": ("legs", "腿部", "双腿", "leg_position", "legs_position"),
    "arms": ("arms", "手臂", "双臂", "arm_position", "arms_position"),
    "hands": ("hands", "手部", "双手", "手", "hand_position", "hands_position"),
    "spine": ("spine", "脊柱", "躯干", "上身", "torso", "torso_position"),
    "shoulders": ("shoulders", "肩部", "双肩", "肩", "shoulder_position"),
    "head": ("head", "头部", "头", "head_position"),
    "face_visible": ("face_visible", "露出的脸颊", "可见脸颊", "脸颊"),
    "near_side": ("near_side", "靠近镜头的一侧", "近侧", "近侧方向"),
    "gaze": ("gaze", "目光", "视线", "眼神", "eye_direction"),
    "outline": ("outline", "剪影", "轮廓", "身体剪影", "silhouette"),
    "brows": ("brows", "眉毛", "眉部", "眉"),
    "eyes": ("eyes", "眼睛", "眼部", "眼神细节"),
    "mouth": ("mouth", "嘴部", "嘴角", "口部", "唇部"),
    "jaw": ("jaw", "下颌", "下巴状态"),
    "expression": ("expression", "表情", "面部表情", "神态", "神情", "facial_expression"),
    "mood": ("mood", "情绪", "气质", "整体神态", "情绪氛围", "demeanor"),
    "dynamic": ("dynamic", "动态", "动势", "动作", "movement"),
    "framing": ("framing", "构图", "取景", "取景范围", "crop", "shot_type"),
    "occluded": ("occluded", "遮挡", "被遮挡", "被裁掉", "occlusion", "hidden"),
    "confidence": ("confidence", "置信度", "可信度"),
    "has_person": ("has_person", "有人物", "含人物"),
    "note": ("note", "备注", "说明"),
}

POSE_MISSING_WORDS = (
    "未入镜", "不在画面", "不可见", "无法判断", "无法看清", "看不清", "被裁", "被遮挡",
    "未拍到", "没拍到", "未出现", "画面外", "无法辨认", "难以辨认", "看不出",
    "unknown", "n/a", "none", "not visible", "cannot",
)

POSE_CAMERA_WORDS = (
    "仰拍", "俯拍", "低角度", "高角度", "仰角", "俯角", "仰度", "广角", "长焦", "鱼眼",
    "视角", "拍摄", "机位", "近景", "特写", "半身", "全身", "七分身", "肖像",
)
POSE_PROP_WORDS = ("物体", "道具", "杂物")
POSE_UNCERTAIN_WORDS = ("难以判断", "无法确定", "不确定", "似乎", "感觉", "推测", "看起来像", "可能是")
POSE_GARMENT_WORDS = (
    "裙摆", "裙", "衣摆", "衣角", "衣襟", "外套", "大衣", "风衣", "衬衫", "T恤", "t恤",
    "毛衣", "卫衣", "裤脚", "裤腿", "裤子", "短裤", "长裤", "牛仔裤", "鞋", "靴", "袜",
    "腰带", "领带", "围巾", "帽子", "帽兜", "手套", "墨镜", "手提包", "背包", "单肩包",
    "腰包", "手包", "包袋", "饰品", "装饰", "珠宝", "手链", "手表", "腕花", "手花",
    "胸花", "花束", "项链", "耳环", "手镯", "戒指", "袖口", "袖子", "领口",
)
SUPPORT_SEAT_POSE = ("坐姿", "坐着", "坐下")
SUPPORT_LEAN_POSE = ("倚靠", "靠在", "斜倚")
SUPPORT_FLOOR_POSE = ("蹲", "半蹲", "跪", "半跪")
SUPPORT_SEAT_SCENE = ("椅", "凳", "沙发", "床", "吧台", "柜台", "台阶", "石阶", "桌")
SUPPORT_LEAN_SCENE = ("墙", "栏杆", "扶手", "窗台", "台面", "柱子")


def _strip_pose_text(text):
    """动作里混进来的机位、道具、服装、表情与不确定表述去掉，只保留能执行的姿态。"""
    text = str(text or "")
    for word in POSE_PROP_WORDS:
        text = text.replace(word, "身前")
    text = text.replace("相机", "镜头")
    text = re.sub(r"[、；;]+", "，", text)
    parts = []
    for part in text.split("，"):
        part = part.strip()
        if not part:
            continue
        if any(w in part for w in POSE_UNCERTAIN_WORDS):
            continue
        if any(w in part for w in POSE_CAMERA_WORDS):
            continue
        if any(w in part for w in POSE_GARMENT_WORDS):
            continue
        if re.fullmatch(r"(身体|人物|画面|整体)?(略微|轻微|稍微|稍|略)?(角度|方向)?", part):
            continue
        part = re.sub(r"^(呈的|呈小幅的?|且稍带|稍带)", "", part).strip()
        part = re.sub(r"(且|并|而|稍带|略带)$", "", part).strip()
        if part in ("", "身体", "人物", "画面", "整体", "动作", "效果", "镜头"):
            continue
        parts.append(part)
    return "，".join(parts).strip("，；;。 ") or ""


def _pick(d, *names):
    for name in names:
        v = d.get(name)
        if isinstance(v, str) and v.strip():
            v = v.strip()
            if v not in ("无", "没有", "未填写", "-", "—"):
                return v
    return ""


def _flag_false(v):
    if v is False:
        return True
    if isinstance(v, str):
        return v.strip().lower() in ("false", "0", "否", "无", "没有", "none", "no")
    return False


def _norm_framing(raw, camera):
    """取景级别只认四个值，模型漏填时从 camera 文本里兜底。"""
    text = _pick(raw, "framing", "取景", "取景级别", "shot")
    for name in ("近景", "半身", "七分身", "全身"):
        if name in text:
            return name
    for name in ("近景", "半身", "七分身", "全身"):
        if name in camera:
            return name
    return ""


POSE_DEG_VALUES = ("正面0°", "前侧45°", "正侧90°", "背侧135°", "背面180°")
POSE_TURN_WORDS = ("侧身", "侧面", "回眸", "回头", "转身", "扭转", "背对", "背向", "斜侧", "三分之四", "3/4")
POSE_WALK_WORDS = ("行走", "走路", "迈步", "跨步", "大步", "步行", "跑动", "奔跑", "跳跃", "走动")


def _deg_from_text(text):
    text = str(text or "")
    if not text.strip():
        return ""
    if "135" in text:
        return "背侧135°"
    if "180" in text:
        return "背面180°"
    if "90" in text:
        return "正侧90°"
    if "45" in text:
        return "前侧45°"
    if any(w in text for w in ("背对镜头", "背向镜头", "背面", "背影")):
        return "背面180°"
    if any(w in text for w in ("背侧", "侧后", "侧背")):
        return "背侧135°"
    if any(w in text for w in ("正侧", "侧身", "侧面", "全侧")):
        return "正侧90°"
    if any(w in text for w in ("前侧", "斜侧", "三分之四", "3/4", "四分之三")):
        return "前侧45°"
    if any(w in text for w in ("0°", "0度", "正面", "正对镜头")):
        return "正面0°"
    return ""


def _norm_deg(raw, *fallbacks):
    """朝向角度只认五档；显式字段优先，漏填时按其它字段的文字兜底。"""
    explicit = _pick(raw, "orientation_deg", "朝向角度", "身体角度", "角度", "body_angle")
    deg = _deg_from_text(explicit)
    if deg:
        return deg
    return _deg_from_text("；".join(str(x or "") for x in fallbacks))


def _as_text(v):
    if isinstance(v, str):
        return re.sub(r"\s+", " ", v).strip("，；;。 ")
    if isinstance(v, (int, float)):
        return str(v)
    if isinstance(v, dict):
        parts = []
        for _, item in v.items():
            t = _as_text(item)
            if t and t not in parts:
                parts.append(t)
        return "，".join(parts)
    if isinstance(v, list):
        parts = [_as_text(item) for item in v]
        return "，".join(x for x in parts if x)
    return ""


def _as_list(v):
    out = []
    if isinstance(v, str):
        v = [v]
    if not isinstance(v, list):
        return out
    for item in v:
        text = _as_text(item)
        if text and text not in out:
            out.append(text)
    return out


OUTFIT_POSE_RE = re.compile(
    r"(双手|单手|一手|手指|手掌|手插|插兜|插入[^，；。]*口袋|插在[^，；。]*口袋|手扶|手放|手臂|手肘|手腕|"
    r"站姿|站立|姿势|动作|侧身|转身|叉腰|抱胸)")
ACCESSORY_POSE_RE = re.compile(
    r"(双手|单手|一手|手指|手掌|手插|插兜|插入[^，；。]*口袋|插在[^，；。]*口袋|手扶|手放|手臂|手肘|"
    r"站姿|站立|姿势|动作|侧身|转身|叉腰|抱胸)")
NOTE_BAD_RE = re.compile(
    r"(水印|文字|logo|双手|单手|手指|手掌|手部|手腕|手扶|插入|插袋|姿势|动作|表情|道具|"
    r"过曝|欠曝|噪点|压缩|模糊|仰拍|俯拍|透视|拉长)")
SHARED_ITEM_WORDS = (
    "靴", "高跟鞋", "运动鞋", "凉鞋", "乐福鞋", "围巾", "腰带", "腰包", "背包",
    "手提包", "手袋", "单肩包", "连裤袜", "打底裤", "帽子", "手套", "墨镜", "项链", "耳环", "手镯",
)


def _dedupe_accessories(outfit, accessories):
    outfit_text = "".join(outfit)
    out = []
    for item in accessories:
        if any(w in item and w in outfit_text for w in SHARED_ITEM_WORDS):
            continue
        if item not in out:
            out.append(item)
    return out


def _clean_source_note(text):
    text = str(text or "").replace("裸露的", "").replace("裸露", "")
    parts = [p.strip() for p in re.split(r"[；;，,。]", text) if p.strip()]
    parts = [p for p in parts if not NOTE_BAD_RE.search(p)]
    parts = [p for p in parts if p not in ("属身体特征", "属于身体特征", "需保留", "需保留该特征")]
    return "；".join(dict.fromkeys(parts))


def _strip_look_pose(text, strict=True):
    """服装/配饰描述里混进来的动作与画面坐标去掉，否则它会和新姿势打架。"""
    pattern = OUTFIT_POSE_RE if strict else ACCESSORY_POSE_RE
    text = re.sub(r"（[^）]*画面[^）]*）", "", str(text or ""))
    text = text.replace("在画面中", "").replace("画面中", "").replace("画面内", "")
    parts = []
    for part in re.split(r"[；;，,]", text):
        part = part.strip()
        if not part or pattern.search(part):
            continue
        if re.search(r"^(位置在)?画面(下方|上方|左侧|右侧|中央|中)", part):
            continue
        part = re.sub(r"画面(下方|上方|左侧|右侧|中央|中)?", "", part).strip()
        if "画面" in part:
            continue
        parts.append(part)
    return "，".join(parts)


def _strip_prop_pose(text):
    text = str(text or "").replace("抱在", "位于").replace("环抱", "")
    parts = []
    for part in re.split(r"[；;，,]", text):
        part = part.strip()
        if not part or re.search(r"(动态|模糊|虚化|过曝|欠曝)", part):
            continue
        if re.search(r"^无(手持|手部|人物手持)?道具", part):
            continue
        part = re.sub(
            r"[，,]?(至|到|在)[^，；。]*?"
            r"(手边|手上|手中|手里|手扶|手托|手臂|双手|单手|手腕|手指|手掌)[^，；。]*",
            "", part)
        part = re.sub(
            r"[，,]?由?(双臂|双手|手臂|单手|手持|手扶|手托|手腕|手指|手掌|肘部)[^，；。]*",
            "", part)
        part = part.strip("，；; ")
        if not part or part in ("无", "没有", "无道具"):
            continue
        parts.append(part)
    return "，".join(parts)


def _norm_source(raw):
    raw = raw if isinstance(raw, dict) else {}
    person = raw.get("person")
    if not isinstance(person, dict):
        person = {}
        if isinstance(raw.get("person"), str):
            person["face"] = raw["person"]

    def person_field(*names):
        # 首次分析返回 person 子对象；回传时是拍平后的顶层字段
        return _pick(person, *names) or _pick(raw, *names)

    outfit = [x for x in (
        _strip_look_pose(item, True) for item in
        _as_list(raw.get("outfit") or raw.get("clothing") or raw.get("garments"))) if x]
    accessories = [x for x in (
        _strip_look_pose(item, False) for item in
        _as_list(raw.get("accessories") or raw.get("accessory") or raw.get("shoes")))
        if x and not re.search(r"(同上|见\s*outfit|见outfit|见上)", x)]
    camera_text = _pick(raw, "camera", "机位", "镜头", "构图")

    src = {
        "has_person": not _flag_false(raw.get("has_person", raw.get("有人物", raw.get("含人物")))),
        "gender_age": person_field("gender_age", "性别年龄", "性别", "年龄", "gender"),
        "face": person_field("face", "五官", "脸型", "脸型与五官", "facial_features"),
        "skin": person_field("skin", "肤色", "肤色与肤质"),
        "hair": person_field("hair", "发型", "发型发色"),
        "makeup": person_field("makeup", "妆容"),
        "body": person_field("body", "身形", "身材", "身形比例"),
        "expression": person_field("expression", "表情", "神态"),
        "outfit": outfit,
        "accessories": _dedupe_accessories(outfit, accessories),
        "props": _strip_prop_pose(
            _as_text(raw.get("props")) or _pick(raw, "道具", "手持物", "手持道具")),
        "scene": _pick(raw, "scene", "场景", "环境"),
        "background": _pick(raw, "background", "背景"),
        "light": _pick(raw, "light", "光线", "lighting"),
        "camera": camera_text,
        "framing": _norm_framing(raw, camera_text),
        "style": _pick(raw, "style", "风格", "影调", "质感"),
        "pose": _pick(raw, "pose", "原姿势", "动作"),
        "note": _clean_source_note(_pick(raw, "note", "备注", "说明")),
    }
    return src


def _norm_pose(raw):
    raw = raw if isinstance(raw, dict) else {}
    # 模型偶尔把动作字段包在 pose / action 里
    nested = raw.get("pose") or raw.get("action") or raw.get("posture")
    if isinstance(nested, dict):
        merged = dict(nested)
        for k, v in raw.items():
            merged.setdefault(k, v)
        raw = merged

    out = {"has_person": not _flag_false(raw.get("has_person", raw.get("有人物", raw.get("含人物"))))}
    missing = []
    for key, aliases in POSE_KEY_ALIASES.items():
        text = _pick(raw, *aliases)
        if key in ("has_person", "note"):
            continue
        if key == "confidence":
            out[key] = text or "中"
            continue
        if key == "occluded":
            out[key] = re.sub(r"(物体|道具|杂物)", "", text)
            continue
        if text and any(w in text.lower() for w in POSE_MISSING_WORDS):
            missing.append(text)
            text = ""
        out[key] = _strip_pose_text(text) if key not in ("occluded",) else text
    if missing and not out.get("occluded"):
        out["occluded"] = "；".join(dict.fromkeys(missing))
    out["orientation_deg"] = _norm_deg(
        raw, out.get("orientation_deg"), out.get("orientation"), out.get("facing"), out.get("stance"))
    out["note"] = _pick(raw, "note", "备注", "说明")
    if out.get("confidence") not in ("高", "中", "低"):
        out["confidence"] = "中"
    return out


def _image_url(value):
    if not isinstance(value, str) or not value.startswith("data:image/"):
        raise ValueError("缺少图片数据")
    if len(value) > 12 * 1024 * 1024:
        raise ValueError("单张图片过大")
    return value


def _pose_llm_json(system, text, image, max_tokens):
    if not llm_configured():
        raise RuntimeError(NOT_CONFIGURED)
    content = [
        {"type": "text", "text": text},
        {"type": "image_url", "image_url": {"url": _image_url(image), "detail": "high"}},
    ]
    messages = [
        {"role": "system", "content": system},
        {"role": "user", "content": content},
    ]
    last, usage = {}, None
    attempts = 4
    route = "pose.source" if system is POSE_SOURCE_SYSTEM_PROMPT else "pose.prompt"
    for attempt in range(attempts):
        try:
            # 原实现无 thinking 字段（模型默认）；JSON 模式被拒时 llm_chat 内部去掉重发
            reply, usage = llm_chat(messages, json_mode=True, thinking=None, temperature=0.1,
                                    max_tokens=max_tokens, timeout=POSE_TIMEOUT, route=route)
        except aig.AigNetworkError:
            if attempt < attempts - 1:
                budget_sleep(1.0 * (attempt + 1))
                continue
            raise
        except aig.AigRetryable:
            # 对应原 408/429/5xx：退避后用新幂等键重提
            if attempt < attempts - 1:
                budget_sleep(2.5 * (attempt + 1))
                continue
            raise
        last = parse_json(reply)
        if last:
            return last, usage
        if attempt < attempts - 1:
            budget_sleep(0.6)
    return last, usage


def pose_source(image):
    started = time.time()
    raw, usage = _pose_llm_json(
        POSE_SOURCE_SYSTEM_PROMPT,
        "请分析这张原图，提取姿势裂变时需要保持不变的全部视觉信息，只输出 JSON。",
        image, POSE_SOURCE_MAX_TOKENS)
    if not raw:
        raise RuntimeError("原图分析结果为空")
    source = _norm_source(raw)
    if not source["has_person"]:
        raise ValueError("原图中没有识别到人物，请换一张有模特的照片")
    if any(w in source["gender_age"] for w in ("儿童", "小孩", "未成年", "幼儿", "婴儿", "宝宝")):
        raise ValueError("暂不支持未成年人图片，请上传成年模特的原图")
    if not source["outfit"] and not source["face"]:
        raise RuntimeError("原图分析信息过少，请换一张更清晰的照片")
    return {
        "source": source,
        "meta": {
            "model": MODEL_GROUP,
            "elapsed_ms": int((time.time() - started) * 1000),
            "usage": usage or {},
        },
    }


def pose_prompt(payload):
    started = time.time()
    source = _norm_source(payload.get("source"))
    if not source["has_person"]:
        raise ValueError("原图信息无效，请重新上传原图")
    raw, usage = _pose_llm_json(
        POSE_REF_SYSTEM_PROMPT,
        "请提取这张姿势参考图中人物的动作与姿态，只输出 JSON。忽略服装、长相、场景、背景与画质。",
        payload.get("image"), POSE_REF_MAX_TOKENS)
    if not raw:
        raise RuntimeError("姿势参考图分析结果为空")
    pose = _norm_pose(raw)
    if not pose["has_person"]:
        raise ValueError("姿势参考图中没有识别到人物，请换一张")
    if not any(pose.get(k) for k, _ in POSE_FIELD_LABELS):
        raise RuntimeError("没有从姿势参考图中提取到有效动作，请换一张更清晰的图")
    notes = str(payload.get("notes") or "").strip()
    if str(payload.get("mode") or "").strip().lower() == "short":
        prompt = build_pose_prompt_short(source, pose, notes)
    else:
        prompt = build_pose_prompt(source, pose, notes)
    return {
        "prompt": prompt,
        "pose": pose,
        "meta": {
            "model": MODEL_GROUP,
            "elapsed_ms": int((time.time() - started) * 1000),
            "usage": usage or {},
        },
    }


POSE_EXPR_STRONG = ("露齿", "大笑", "灿烂", "咧嘴", "笑开", "惊喜", "惊讶", "愤怒",
                    "悲伤", "哭", "嘟嘴", "挑眉", "冷峻", "严肃", "魅惑", "慵懒")


def _expr_strong(text):
    return any(w in str(text or "") for w in POSE_EXPR_STRONG)




def _short_clause(text, n=26):
    text = re.split(r"[；;，,。]", str(text or ""))[0].strip()
    return text[:n]


def build_pose_prompt_short(source, pose, notes=""):
    """短版姿势提示词：先声明图1/图2角色与改动，再给姿势要点，其余靠图1。

    面向 Seedream / Nano 的编辑场景：控制在几百字，避免长提示词稀释。
    """
    s = _norm_source(source)
    p = _norm_pose(pose)
    deg = p.get("orientation_deg") or ""
    pose_all = "；".join(str(p.get(k) or "") for k, _ in POSE_FIELD_LABELS)

    facing = p.get("facing") or ""
    orient = "，".join(x for x in (deg, facing) if x)
    bits = []
    for key, label in (("stance", "重心"), ("gait", "步伐"), ("legs", "腿"),
                       ("arms", "手臂"), ("hands", "手"), ("head", "头"),
                       ("gaze", "视线"), ("dynamic", "动势")):
        v = _short_clause(p.get(key))
        if v:
            bits.append(f"{label}：{v}")
    pose_text = "；".join(bits) or "按图2的动作"

    def _clean_expr(text):
        text = re.sub(r"^(面部)?表情[:：]?", "", str(text or "").strip())
        text = re.sub(r"^(整体)?(神态|情绪)[:：]?", "", text).strip()
        return text
    expr = _clean_expr(p.get("expression")) or _clean_expr(p.get("mood"))
    expr = _short_clause(expr, 30)

    expr_head = ""
    if expr and _expr_strong(expr):
        expr_head = f"表情先改：图2的表情是{expr}；不要沿用图1原表情，表情肌要变到同等程度。\n\n"
    lines = [
        expr_head + "图1是原图，图2是姿势参考图。只改人物的姿势和表情：把图1人物的动作换成图2的动作；"
        "脸、发型、服装（款式、材质、颜色、领口袖口、鞋）、背景、光线、机位、画幅都保持图1不变。"
        "不要换人、换脸、改衣服、改场景，不要镜像。",
        "",
    ]
    head = f"新姿势：{orient + '；' if orient else ''}{pose_text}。"
    if expr:
        head += f"表情按图2：{expr}。"
    lines.append(head)

    deg_side = deg in ("前侧45°", "正侧90°", "背侧135°", "背面180°")
    if deg_side:
        lines.append("身体要真正侧转：肩线、髋线、胸口一起转，不要只转头；不要回正为正面直立。")

    if any(w in pose_all for w in SUPPORT_SEAT_POSE + SUPPORT_LEAN_POSE + SUPPORT_FLOOR_POSE):
        lines.append("保留图2的坐/倚靠/蹲跪姿势；原图没有支撑物时只补一件最简中性支撑物，不照搬图2的家具。")

    if any(w in pose_all for w in ("帽", "包", "栏杆", "杯")):
        lines.append("图2若扶帽檐/包带/栏杆而图1没有该物，手停在额侧/髋侧/身侧同高，不凭空添加物品。")

    if notes:
        lines.append("补充：" + str(notes).strip())

    lines.append("不要多腿多手、重复或融合的鞋、反向关节；不要改服装细节和配饰。")
    return "\n".join(lines)


def build_pose_prompt(source, pose, notes=""):
    s = _norm_source(source)
    p = _norm_pose(pose)

    person = "；".join(x for x in (
        s["gender_age"], s["face"], s["skin"], s["hair"], s["makeup"], s["body"],
    ) if x)
    if not person:
        person = "人物的性别、脸型、五官、肤色、发型、妆容与身材比例，全部以图1（原图）为准"

    outfit = "；".join(s["outfit"]) or "服装以图1（原图）为准，款式、颜色、材质、图案与穿法完全不变"
    accessories = "；".join(s["accessories"])

    scene_all = "；".join(x for x in (s["scene"], s["background"], s["props"]) if x)
    pose_all = "；".join(str(p.get(k) or "") for k, _ in POSE_FIELD_LABELS)
    needs_sit = any(w in pose_all for w in SUPPORT_SEAT_POSE)
    needs_lean = any(w in pose_all for w in SUPPORT_LEAN_POSE)
    needs_floor = any(w in pose_all for w in SUPPORT_FLOOR_POSE)
    has_seat = any(w in scene_all for w in SUPPORT_SEAT_SCENE)
    has_lean = has_seat or any(w in scene_all for w in SUPPORT_LEAN_SCENE)
    support_note = ""
    if needs_sit and not has_seat:
        support_note = (
            "【支撑处理】图2（姿势参考图）是坐姿或倚靠姿势，需要支撑物。"
            "保留图2的坐/倚靠姿势，不要改成站立，也不要为了避开支撑物而改写动作。"
            "图1（原图）场景里没有可见的座椅、台面或可倚靠物时，在人物身下或身侧补一件最简单、中性的支撑物"
            "（如无扶手素色椅、圆凳、矮台），只用于完成动作，不遮挡人物，不改变图1的背景、光线、机位与画幅；"
            "支撑物的款式、颜色和位置不要照搬图2，也不要带入图2的其它物品。"
            "图1场景已有座椅、台面或可倚靠物时，直接沿用它，不另外添加。")
    elif needs_lean and not has_lean:
        support_note = (
            "【支撑处理】图2（姿势参考图）是倚靠姿势，需要支撑物。"
            "保留图2的倚靠姿势，不要改成站立。图1（原图）场景里没有可倚靠的墙面、栏杆或台面时，"
            "在人物身侧补一件最简单、中性的支撑物（如素色墙面、窄台、栏杆），只用于完成动作，"
            "不遮挡人物，不改变图1的背景、光线、机位与画幅；款式与位置不要照搬图2。")
    elif needs_floor and not has_seat:
        support_note = (
            "【支撑处理】图2（姿势参考图）是蹲姿或跪姿。保留图2的蹲/跪姿势，直接在图1的地面完成，"
            "不要改成站立；不添加椅子、凳子、垫子等新道具；图1的地面、背景、光线、机位与画幅保持不变。")

    deg = p.get("orientation_deg") or ""
    facing = p.get("facing") or ""
    orientation_bits = [x for x in (deg, p.get("orientation")) if x]
    orientation_line = "，".join(dict.fromkeys(orientation_bits))
    if facing and facing not in orientation_line and "面" not in orientation_line:
        orientation_line = f"{orientation_line}，面部朝向{facing}" if orientation_line else f"面部朝向{facing}"

    pose_lines = []
    if orientation_line:
        pose_lines.append(f"身体朝向：{orientation_line}")
    for key, label in POSE_FIELD_LABELS:
        if key == "orientation":
            continue
        val = p.get(key) or ""
        if val:
            pose_lines.append(f"{label}：{val}")
    pose_text = "；".join(pose_lines) or "按图2（姿势参考图）的动作自然还原"

    side_pose = deg in ("前侧45°", "正侧90°", "背侧135°", "背面180°") or (
        not deg and any(w in pose_all for w in POSE_TURN_WORDS))
    moving_pose = any(w in pose_all for w in POSE_WALK_WORDS)

    extra = []
    if p.get("occluded"):
        extra.append(p["occluded"])
    extra_text = "；".join(x for x in dict.fromkeys(extra) if x).strip("。；;，, ")

    reference_pose = s["pose"]

    def _first_expr_clause(text):
        for part in re.split(r"[；;，,。]", str(text or "")):
            if any(w in part for w in (
                    "微笑", "笑容", "笑意", "大笑", "浅笑", "笑", "冷峻", "严肃",
                    "惊讶", "慵懒", "亲和", "平静", "自信", "神情")):
                return part.strip()
        return ""

    def _clean_expr(text):
        text = re.sub(r"^(面部)?表情[:：]?", "", str(text or "").strip())
        text = re.sub(r"^(整体)?(神态|情绪)[:：]?", "", text).strip()
        text = re.sub(r"^(眉部|眉毛|眼部|眼睛|眼神|嘴部|嘴角|口部|唇部|下颌|下巴)[:：]?", "", text).strip()
        return text

    ref_expr = _clean_expr(p.get("expression"))
    ref_mood = _clean_expr(p.get("mood"))
    src_expr = _clean_expr(s["expression"])
    if not ref_expr:
        for key, _ in POSE_FIELD_LABELS:
            hit = _first_expr_clause(p.get(key))
            if hit:
                ref_expr = hit
                break
    detail_bits = []
    for key, label in (("brows", "眉"), ("eyes", "眼"), ("mouth", "嘴"), ("jaw", "下颌")):
        val = _clean_expr(p.get(key))
        if val and val not in ("不可见", "看不清", "无法判断"):
            detail_bits.append(f"{label}：{val}")
    detail_text = "；".join(detail_bits)
    if ref_expr or ref_mood or detail_text:
        expr_bits = [x for x in (detail_text, ref_expr) if x]
        expr_text = "；".join(expr_bits)
        if ref_mood:
            expr_text = f"{expr_text}；整体神态：{ref_mood}" if expr_text else f"整体神态：{ref_mood}"
        expr_line = (
            "【表情与神态】图1（原图）的人物身份、脸型、五官结构与骨相、肤色、妆容不变，"
            "但面部表情必须按图2（姿势参考图）重新做，不要沿用图1的原表情。"
            f"图2的表情与神态：{expr_text}。"
            "表情属于动作的一部分，不受「与图1保持一致」约束：眉毛、眼睛、嘴角、下颌与面颊的走向都要跟着改变，"
            "表情变化要清楚可辨，不要只在原表情上做细微调整。"
            "只改变表情肌与头部朝向，不改变五官结构、脸型、骨相、肤色与妆容；不换脸。")
        if src_expr:
            expr_line += f"图1原表情为：{src_expr}，本张不要沿用，以图2的表情与神态为准。"
    else:
        expr_line = (
            "【表情与神态】图1（原图）的脸型、五官、骨相、肤色、妆容与人物身份不变；"
            f"图2没有提供清晰可辨的表情时，表情与神态沿用图1：{src_expr or '以图1为准'}。不换脸。")

    priority = (
        "【优先级】本张的首要目标是还原图2（姿势参考图）的动作：身体朝向、转向角度、重心、步伐、四肢位置、头部朝向、视线与整体动势必须逐项命中，"
        "并且与图1（原图）的原姿势明显不同。还原动作优先于「保持构图」「保持站姿」「与原图一致」："
        "宁可让动作因取景被裁切，也不要把侧身、背身、扭转、跨步、行走改写成正面直立站姿。"
        "侧身以肩线、胸廓、髋线与脚尖方向同时转过去为准，只转头或只转上半身不算侧身。")

    check_items = []
    if orientation_line:
        check_items.append(f"身体朝向 {orientation_line}")
    for key, label in (("gait", "步伐"), ("hands", "双手"),
                       ("head", "头部"), ("gaze", "视线"), ("dynamic", "整体动势")):
        val = p.get(key) or ""
        if val:
            check_items.append(f"{label} {val}")
    restore = ["【姿势还原】逐项核对，全部命中才算完成。"]
    if check_items:
        restore.append("必须命中：" + "；".join(check_items) + "。")
    if p.get("outline"):
        restore.append(f"人物剪影：{p['outline']}；生成结果的身体轮廓、四肢角度与朝向应与图2高度重合。")
    else:
        restore.append("生成结果的身体轮廓、四肢角度与朝向应与图2高度重合。")
    if side_pose:
        restore.append("图2是侧身或转身姿态，生成结果必须保留同样的侧转角度，不得回正为正面。")
    if moving_pose:
        restore.append("图2是行走或动态抓拍，生成结果必须保留迈步与动势，不得改成双脚并拢的静态站立。")
    restore_line = "".join(restore)

    dir_text = " ".join(x for x in (facing, p.get("orientation"), p.get("gaze"), p.get("head")) if x)
    side_side = ""
    if "画面左" in dir_text or "向左" in dir_text:
        side_side = "画面左侧"
    elif "画面右" in dir_text or "向右" in dir_text:
        side_side = "画面右侧"
    toward = side_side or "行进方向"
    visibility_line = ""
    if deg == "正侧90°":
        visibility_line = (
            f"【侧身可见性】生成结果必须是真正的正侧身：面部呈侧脸，鼻尖朝向{toward}，画面中只看到一只耳朵；"
            "近侧肩膀在前，远侧肩膀与远侧手臂大部分被躯干挡住；服装的正面门襟只露出侧面。"
            "如果仍能看到左右对称的双肩和完整正脸，说明身体没有转过去，属于失败。")
    elif deg == "背侧135°":
        visibility_line = (
            f"【侧身可见性】生成结果必须是背侧身：后背与侧后方朝向镜头，面部回望{toward}，"
            "画面中主要看到后背、肩胛与一侧手臂；前胸与服装正面大部分被身体挡住。"
            "如果仍看到对称的完整正脸和前胸，说明身体没有转过去，属于失败。")
    elif deg == "背面180°":
        visibility_line = (
            "【侧身可见性】生成结果必须是背影或近背影：后背完全朝向镜头，主要看到后背、后脑与两侧肩线，"
            "前胸与服装正面不可见；面部只在回望时露出侧脸。如果仍看到完整正脸与前胸，说明身体没有转过去，属于失败。")
    elif deg == "前侧45°":
        visibility_line = (
            f"【侧身可见性】生成结果必须是四分之三侧身：面部呈四分之三侧脸，鼻尖偏向{toward}，"
            "近侧肩膀在前，远侧肩膀略被躯干挡住，两侧肩线明显前后错开。"
            "如果仍是对称的完整正脸，说明身体没有转过去，属于失败。")
    if visibility_line:
        visibility_line += (
            "机位、镜头与背景完全不动，是人物在原地转身，不是镜头绕着人转；背景的墙体、地平线、光斑与投影位置保持不变。"
            "肩线与髋线要一起转，侧身后双肩在画面上的横向宽度明显小于图1（原图）的正面宽度；不允许只转头不转身。")
        if p.get("face_visible") and p["face_visible"] not in ("不可见", "看不清"):
            visibility_line += f"回望时画面中露出的脸颊与图2一致（{p['face_visible']}），不要镜像到另一侧。"
        if p.get("near_side"):
            visibility_line += f"靠近镜头的一侧是{p['near_side']}，近侧肩膀与手臂在前，远侧被身体挡住。"

    head = "【姿势替换】"
    def _clip(text, n=18):
        text = re.split(r"[；;，,。]", str(text or ""))[0].strip()
        return text[:n]
    key_points = [x for x in (
        deg,
        _clip(p.get("gait")) if p.get("gait") else "",
        _clip(p.get("hands")) if p.get("hands") else "",
        _clip(p.get("head") or p.get("gaze")) if (p.get("head") or p.get("gaze")) else "",
        _clip(p.get("dynamic")) if p.get("dynamic") else "",
    ) if x]
    if key_points:
        head += "动作要点：" + "｜".join(dict.fromkeys(key_points)) + "。"
    if reference_pose:
        head += f"图1（原图）的姿势为：{reference_pose}，该姿势不再保留。"
        if side_pose or moving_pose:
            head += "也不要默认回到正面直立站姿。"
    head += f"新姿势（只有动作来自图2）：{pose_text}。"
    extra_lead = (extra_text + "。") if extra_text else ""
    fill_line = (
        f"【动作补全】{extra_lead}图2中看不到或看不清的部位，按该动作的自然人体姿态补全；重心合理、关节角度真实，不臆造夸张动作。"
        "以图1的取景范围为准，只采用落在图1画面内的动作；图2中超出图1取景的肢体动作（例如图1只到大腿而图2拍到双脚）不要求出现在画面中，"
        "也不得为此拉远镜头或改变机位与焦距；被取景裁切时，身体朝向、重心与整体动势仍必须保留。")
    prop_line = (
        "【参考图道具】图2（姿势参考图）中人物手里、身前或身边的物品（包袋、手机、杯子等）不属于动作，不要出现在画面中；只还原双手的位置、角度与手型。"
        "椅子、凳子、矮台、台阶这类用于完成坐/跪/倚靠动作的支撑物例外：按【支撑处理】保留或补一件最简支撑物，不要照搬图2的款式、颜色或场景，也不要带入图2的其它物品。"
        "如果图2的动作依赖图1（原图）没有的物品或服饰（帽子、墨镜、包袋、栏杆、杯子等），把动作迁移到身体的等效位置：扶帽檐→手停在额侧或发际线同一高度；扶包带→手停在髋侧同一高度；扶栏杆→手停在身侧同一高度。"
        "不要因此凭空添加该物品，也不要改变手臂抬起的高度、肘部角度与手型；另一只手同样按图2的位置，不要默认叉腰、插兜或自然下垂。")
    limb_line = (
        "【肢体结构】画面中只有两条腿、两只脚、两只手，每只手五根手指；同一侧只有一条腿、一只鞋。每只脚只穿一只鞋，左右两只鞋各自独立、朝向正确，"
        "不合并、不粘连、不重复、不交换。膝盖、脚踝、脚掌的朝向与弯曲必须符合该动作的解剖结构；不得出现第三条腿、重复的鞋、融合或错位的脚、反向关节。"
        "腿部或脚部被衣物、姿势或画面边缘遮挡时，宁可少露一点，也不要补出结构错误的腿脚；鞋型、鞋跟高度、靴筒高度与绑带数量沿用图1（原图）。")

    expr_head = ""
    if (ref_expr or ref_mood or detail_text) and _expr_strong(expr_text):
        expr_head = "表情按图2，不要沿用图1；表情变化要明显。\n"

    lines = [
        expr_head,
        "【任务】以图1（原图）为唯一基准做姿势裂变：保持同一个人物、同一套服装、同一场景、同一背景、同一光线、同一机位与画幅，只把模特的动作为图2（姿势参考图）中的动作，并复刻图2的表情与神态。生成结果应像图1（原图）同一次拍摄中换了姿势与表情的另一张画面。",
        "",
        "【生成方式】以图1（原图）作为图生图的原图/首帧，在图1基础上只调整人物动作与表情神态；不得重新构图、重新设计画面或更换人物。",
        "",
        "【参考图分工】图1（第一张）是原图：人物、五官、服装、配饰、场景、背景、光线、机位与画幅都只以它为唯一依据；图2（第二张）是姿势参考图：只提取动作与表情神态，其他任何元素（人物身份、长相、服装、道具、场景、背景、光线、画质、构图）都不要参考。若模型只支持一张参考图，只提交图1；此时下方所有「图2」都按文字动作与表情描述理解，不要寻找第二张图。",
        "",
        priority,
        "",
        head,
        restore_line,
    ]
    if visibility_line:
        lines.append(visibility_line)
    lines.append(fill_line)
    lines.append(limb_line)
    if support_note:
        lines.append(support_note)
    lines.append(prop_line)
    lines.append("")
    lines += [
        f"【身份锁定】{person}。五官、脸型、骨相、肤色、发型、发色、妆容与身材比例必须与图1（原图）完全一致，不得被图2（姿势参考图）中的人物替换；身份锁定只约束长相与妆容，不约束表情。",
        expr_line,
        "",
        f"【服装锁定】{outfit}。" + (f"配饰与鞋履：{accessories}。" if accessories else ""),
        "服装的款式、颜色、材质、图案、长度、版型、穿着层次、配饰数量与佩戴方式保持不变；只允许因动作产生的自然褶皱、垂坠与遮挡变化，不得增减、替换、改短、改长或改变穿法；配饰位置随身体动作自然变化。图1（原图）中看不到的衣物细节，按图1同一件服装的款式自然延续，不得换成其他款式、新增单品或改变长度；保持图1取景。",
        "【服装细节不可发明】领型与领口开合方式、门襟（拉链/纽扣/系带/交叉绑带）、袖型与袖口形状、接缝与结构线、罗纹与织纹方向、下摆与口袋、五金数量与位置，必须逐项与图1（原图）一致。手臂交叉、抬起或被遮挡时，袖子只随手臂自然弯折与堆叠，不得把收口袖改成喇叭袖、荷叶边、灯笼袖或系带袖；不得新增蝴蝶结、系带、褶边、口袋、腰带或任何装饰；不得把拉链改成系带、把套头改成开衫、把翻领改成系带领。图案与纹理的大小、密度、方向、位置不变，颜色与面料光泽不变。文字对服装的描述只用于提醒，与图1（原图）画面不一致时以图1画面为准；拿不准的细节保持图1的结构，不要自行设计。",
        "",
    ]
    if s["props"]:
        lines += [f"【原图道具】图1（原图）中的道具：{s['props']}。道具属于图1，优先保留；不要为了持握或保留道具而改变动作、扭曲肢体或增减其他道具。新动作让道具无法自然保留时，把道具放在画面内不遮挡人物的原有位置（如桌面、地面、身侧），或让该道具不出现在画面中。", ""]
    scene_bits = "；".join(x for x in (s["scene"], s["background"]) if x)
    if scene_bits:
        lines += [f"【场景与背景锁定】{scene_bits}。场景、背景元素、道具位置、景深与虚化程度保持不变，不引入图2（姿势参考图）中的任何场景、背景或物品（姿势必需的支撑物见【支撑处理】）。", ""]
    if s["note"]:
        lines += [f"【原图备注】{s['note']}", ""]
    light_bits = "；".join(x for x in (s["light"], s["style"]) if x)
    if light_bits:
        lines += [f"【光线与画质】{light_bits}。光线方向、软硬、色温、明暗对比、色调与质感与图1（原图）一致。", ""]
    if s["camera"]:
        lines += [f"【机位与画幅】{s['camera']}。机位高度、镜头焦段感、取景范围、人物在画面中的位置与画幅比例与图1（原图）一致；不要为了容纳新姿势而拉远镜头、改变取景或改成其他构图。", ""]
    if s["framing"]:
        outside = {"近景": "腰部以下", "半身": "大腿以下", "七分身": "小腿以下", "全身": ""}.get(s["framing"], "")
        fit = f"【取景适配】图1（原图）为{s['framing']}构图。只采用落在图1画面内的动作"
        if outside:
            fit += f"；{outside}不要求出现在画面内"
        fit += "，也不得为了展示取景外的部位拉远镜头、改变机位或画幅；动作被取景裁切时，身体朝向与整体动势仍必须保留。"
        lines += [fit, ""]

    if p.get("confidence") == "低":
        lines.append("【参考质量】图2（姿势参考图）较模糊，只按可辨识的动作轮廓还原，不猜测手指等细节。")
    if notes:
        lines.append(f"【补充要求】{notes}")
    outfit_all = "".join(s["outfit"]).lower()
    compliance = (POSE_LINGERIE_COMPLIANCE
                  if any(w in outfit_all for w in POSE_LINGERIE_WORDS)
                  else POSE_COMPLIANCE)
    forbid_extra = ""
    if side_pose or moving_pose:
        forbid_extra += "不要把图2的侧身、背身、扭转、跨步或行走动作回正为正面直立、双脚并拢、双臂自然下垂的静态站姿；"
    if ref_expr or ref_mood or detail_text:
        forbid_extra += "不要沿用图1的原表情，也不要把表情做得比图2更轻微；"
    lines += [
        "",
        f"【合规】{compliance}",
        "",
        "【一致性】新画面必须像同一位模特、同一套服装、同一场景的连续拍摄，只换了动作、表情与神态。人物身份、服装、配饰、场景、背景、光线、机位与画幅以图1（原图）为准；动作、身体朝向、步伐、四肢位置、动势与表情神态一律以图2（姿势参考图）为准，两者冲突时以图2为准。",
        "",
        f"【禁止项】不要换人、换脸或改变五官结构、脸型、肤色、发型、妆容、身材比例；表情与神态按图2复刻；不要改变服装的款式、颜色、材质、图案、长度、穿法与配饰；不要把收口袖改成喇叭袖、荷叶边或系带袖，不要把拉链改成系带或蝴蝶结，不要把翻领改成系带领，不要新增任何装饰、褶边、口袋或腰带；不要无故改变场景、背景、原有道具、光线、色调、机位、镜头、取景与画幅；{forbid_extra}不要引入图2（姿势参考图）中的任何人物、服装、背景或元素，也不要把图2的手持物品带进画面（姿势必需的支撑物见【支撑处理】）；不要添加图1没有的文字、水印或 logo；不要水平翻转或镜像人物与画面，头部转向、手位与步伐的左右关系必须与图2一致；不要出现第三条腿、重复或融合的鞋、肢体扭曲、关节反折、多余手指、手部穿模或服装穿模。",
        "",
        "【输出】画面质感、清晰度、色彩与图1（原图）一致，像同一张图的连续拍摄；动作、身体朝向与动势按图2还原，重心合理、肢体物理正确；表情与神态按图2复刻且清楚可辨；服装纹理与走线准确；画幅比例与图1一致。",
    ]
    return "\n".join(lines)


def analyze(payload):
    cat_key = payload.get("category") or "top"
    item_keys = _item_keys(payload)
    cat = _compose_cat(item_keys) if cat_key == "compose" and item_keys else CATS.get(cat_key, CATS["top"])
    warning = ""
    fields = {}
    meta = {
        "category": payload.get("category") or "top",
        "model": MODEL_GROUP,
        "degraded": False,
        "vision": False,
        "images": 0,
    }

    picked = select_images(payload)
    if picked:
        try:
            v = vision_fields(payload, picked)
            if not v:
                raise RuntimeError("读图结果为空")
            fields = vision_to_fields(v, payload)
            meta["vision"] = True
            meta["images"] = len(picked)
            match = fields.get("_match", True)
            meta["garment_match"] = match
            meta["garment_type"] = str(v.get("garment_type") or "")
            if not match:
                actual = str(v.get("garment_type") or "其他品类")
                if cat_key == "look":
                    warning = f"参考图看起来只有{actual}，整套 Look 需要至少两件单品"
                else:
                    warning = f"参考图看起来是{actual}，与所选「{cat['label']}」不一致，请确认品类"
        except aig.AigNetworkError as e:
            warning = f"读图连接失败（{getattr(e, 'reason', e)}），已改用文字信息生成"
            meta["degraded"] = True
        except AigError as e:
            warning = f"读图返回 {e.code}，已改用文字信息生成"
            meta["degraded"] = True
        except Exception as e:  # noqa: BLE001
            warning = f"读图失败（{e}），已改用文字信息生成"
            meta["degraded"] = True

    if not fields:
        try:
            fields = deepseek_fields(payload)
        except aig.AigNetworkError as e:
            warning = (warning + "；" if warning else "") + f"文字模型连接失败（{getattr(e, 'reason', e)}），已用模板生成"
            meta["degraded"] = True
        except AigError as e:
            warning = (warning + "；" if warning else "") + f"文字模型返回 {e.code}，已用模板生成"
            meta["degraded"] = True
        except Exception as e:  # noqa: BLE001
            warning = (warning + "；" if warning else "") + f"{e}，已用模板生成"
            meta["degraded"] = True

    payload["_picked"] = picked
    prompt = build_prompt(payload, fields)
    return {"prompt": prompt, "meta": meta, "warning": warning}


def analyze_batch(payload):
    """一对多：同一组服装/裁切，按多个模特分别生成提示词；最多 9 个模特。"""
    raw_imgs = payload.get("images") or []
    imgs = [im for im in raw_imgs if isinstance(im, dict) and str(im.get("data") or "").startswith("data:image/")]
    models = [m for m in (payload.get("models") or [])
              if isinstance(m, dict) and str(m.get("data") or "").startswith("data:image/")]
    if not models:
        models = [im for im in imgs if str(im.get("role") or "") == "人物模特"]
    models = models[:9]
    if not models:
        raise ValueError("没有可用的模特图")
    base = [im for im in imgs if str(im.get("role") or "") != "人物模特"]
    concurrency = max(1, min(6, int(payload.get("concurrency") or 3)))

    req_deadline = request_deadline()

    def one(i, model):
        set_request_deadline(req_deadline)   # 线程池 worker 继承本请求的总截止时间
        sub = dict(payload)
        sub.pop("models", None)
        sub["images"] = base + [{
            "role": "人物模特",
            "label": str(model.get("label") or ""),
            "data": model["data"],
        }]
        try:
            r = analyze(sub)
            return {
                "index": i,
                "model_id": str(model.get("id") or model.get("model_id") or i),
                "prompt": r.get("prompt") or "",
                "meta": r.get("meta") or {},
                "warning": r.get("warning") or "",
            }
        except Exception as e:  # noqa: BLE001
            return {
                "index": i,
                "model_id": str(model.get("id") or model.get("model_id") or i),
                "prompt": "",
                "meta": {},
                "warning": "",
                "error": str(e),
            }

    results = []
    with ThreadPoolExecutor(max_workers=min(concurrency, len(models))) as ex:
        futs = [ex.submit(one, i, m) for i, m in enumerate(models)]
        for f in as_completed(futs):
            try:
                results.append(f.result())
            except Exception as e:  # noqa: BLE001
                results.append({"index": -1, "model_id": "", "prompt": "", "error": str(e)})
    results.sort(key=lambda x: int(x.get("index", 0)))
    return {
        "results": results,
        "meta": {
            "models": len(models),
            "concurrency": concurrency,
            "prompt_version": str(payload.get("prompt_version") or "old"),
            "target_model": str(payload.get("target_model") or ""),
        },
    }


# ---------------------------------------------------------------- 图片 data URL 归一与校验

_DATA_URL_RE = re.compile(r"^data:image/([A-Za-z0-9.+-]+)((?:;[^;,]*)*);base64,(.*)$", re.S | re.I)
_MIME_ALIAS = {"jpg": "jpeg", "pjpeg": "jpeg"}
GATEWAY_IMAGE_TYPES = ("png", "jpeg", "webp", "gif")   # ai-gateway 支持的图片格式
_WS_RE = re.compile(r"\s")


def normalize_data_url(value):
    """归一一个 data:image/... URL：mime 小写、jpg→jpeg；网关不支持的格式、base64 含空白抛 ValueError（中文文案）。"""
    m = _DATA_URL_RE.match(value)
    if not m:
        raise ValueError("图片不是合法的 base64 data URL（需形如 data:image/png;base64,...）")
    sub = m.group(1).lower()
    sub = _MIME_ALIAS.get(sub, sub)
    if sub not in GATEWAY_IMAGE_TYPES:
        raise ValueError("不支持的图片格式 image/%s，仅支持 PNG / JPEG / WebP / GIF" % sub)
    data = m.group(3)
    if _WS_RE.search(data):
        raise ValueError("图片 base64 数据含空白字符（空格或换行），请去掉后重试")
    return "data:image/%s;base64,%s" % (sub, data)


def normalize_payload_images(obj):
    """递归把请求里所有 data:image/ 字符串就地归一；任一不合规抛 ValueError（路由回 422）。"""
    items = obj.items() if isinstance(obj, dict) else enumerate(obj) if isinstance(obj, list) else ()
    for k, v in list(items):
        if isinstance(v, str):
            if v[:11].lower() == "data:image/":
                obj[k] = normalize_data_url(v)
        elif isinstance(v, (dict, list)):
            normalize_payload_images(v)


# ---------------------------------------------------------------- HTTP

def _gateway_display():
    """只回 scheme://host[:port][/path]，去掉可能的 userinfo / query。"""
    if not AIG_BASE_URL:
        return ""
    u = urlsplit(AIG_BASE_URL)
    host = u.hostname or ""
    if u.port:
        host += ":%d" % u.port
    return "%s://%s%s" % (u.scheme, host, u.path)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    def send_json(self, obj, status=200):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == "/api/status":
            ready, ready_reason = gateway_ready()
            return self.send_json({
                "service": "prompt-reverse",
                "version": VERSION,
                "mock": False,
                "llm_configured": llm_configured(),
                "model": MODEL_GROUP,
                "model_group": MODEL_GROUP,
                "model_label": "DeepSeek 读图 · 经 AI 网关（%s）" % MODEL_GROUP,
                # 排障时只走的部署（REVERSE_ALLOWED_DEPLOYMENTS）；空列表 = 组内正常路由
                "allowed_deployments": list(ALLOWED_DEPLOYMENTS),
                # 模型组由网关侧声明支持读图，这里固定 true
                "vision": True,
                "gateway": _gateway_display(),
                "gateway_ready": ready,
                "gateway_reason": ready_reason,
            })
        return self.send_json({"error": "not found"}, 404)

    def do_POST(self):
        begin_request()   # 总预算从请求开始算
        path = urlsplit(self.path).path
        if path in ("/api/pose/source", "/api/pose/prompt"):
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                return self.send_json({"error": "payload too large"}, 413)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8") or "{}")
            except ValueError:
                return self.send_json({"error": "invalid json"}, 400)
            try:
                normalize_payload_images(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            started = time.time()
            try:
                result = (pose_source(payload.get("image")) if path == "/api/pose/source"
                          else pose_prompt(payload))
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            except aig.AigNetworkError as e:
                return self.send_json(
                    {"error": f"读图连接失败（{getattr(e, 'reason', e)}）"}, 502)
            except AigError as e:
                return self.send_json({"error": f"读图返回 {e.code}，请稍后重试"}, 502)
            except RequestBudgetExceeded as e:
                return self.send_json({"error": str(e)}, 504)
            except Exception as e:  # noqa: BLE001
                return self.send_json({"error": f"分析失败：{e}"}, 502)
            elapsed = int((time.time() - started) * 1000)
            sys.stderr.write("pose %s · %dms · %s\n" % (
                path.rsplit("/", 1)[-1], elapsed,
                ("confidence=" + str(result.get("pose", {}).get("confidence")))
                if path == "/api/pose/prompt" else
                ("outfit=%d" % len(result.get("source", {}).get("outfit", [])))
            ))
            return self.send_json(result)
        if path == "/api/reverse/crop-check":
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                return self.send_json({"error": "payload too large"}, 413)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8") or "{}")
            except ValueError:
                return self.send_json({"error": "invalid json"}, 400)
            try:
                normalize_payload_images(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            started = time.time()
            try:
                result = crop_check(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            except aig.AigNetworkError as e:
                return self.send_json({"error": f"读图连接失败（{getattr(e, 'reason', e)}）"}, 502)
            except AigError as e:
                return self.send_json({"error": f"读图返回 {e.code}，请稍后重试"}, 502)
            except RequestBudgetExceeded as e:
                return self.send_json({"error": str(e)}, 504)
            except Exception as e:  # noqa: BLE001
                return self.send_json({"error": f"裁切质检失败：{e}"}, 502)
            result["meta"]["elapsed_ms"] = int((time.time() - started) * 1000)
            sys.stderr.write("crop-check · %dms · items=%d\n" % (
                result["meta"]["elapsed_ms"], result["meta"].get("items", 0)))
            return self.send_json(result)
        if path == "/api/reverse/crop-plan":
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                return self.send_json({"error": "payload too large"}, 413)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8") or "{}")
            except ValueError:
                return self.send_json({"error": "invalid json"}, 400)
            try:
                normalize_payload_images(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            started = time.time()
            try:
                result = crop_plan(payload)
                # 没找到任何裁切框时重跑一次；剩余预算不够一次完整调用就不重跑，直接返回首轮结果
                rem = request_remaining()
                if (not any((p.get("crops") or []) for p in (result.get("plans") or []))
                        and (rem is None or rem > MIN_RERUN_BUDGET_S)):
                    budget_sleep(0.6)
                    result = crop_plan(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            except aig.AigNetworkError as e:
                return self.send_json({"error": f"读图连接失败（{getattr(e, 'reason', e)}）"}, 502)
            except AigError as e:
                return self.send_json({"error": f"读图返回 {e.code}，请稍后重试"}, 502)
            except RequestBudgetExceeded as e:
                return self.send_json({"error": str(e)}, 504)
            except Exception as e:  # noqa: BLE001
                return self.send_json({"error": f"找细节失败：{e}"}, 502)
            elapsed = int((time.time() - started) * 1000)
            result["meta"]["elapsed_ms"] = elapsed
            sys.stderr.write("crop-plan · %dms · images=%d · plans=%d\n" % (
                elapsed, result["meta"].get("images", 0), result["meta"].get("plans", 0)))
            return self.send_json(result)
        if path == "/api/reverse/analyze":
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                return self.send_json({"error": "payload too large"}, 413)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8") or "{}")
            except ValueError:
                return self.send_json({"error": "invalid json"}, 400)
            try:
                normalize_payload_images(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            started = time.time()
            try:
                result = analyze(payload)
            except RequestBudgetExceeded as e:
                return self.send_json({"error": str(e)}, 504)
            except Exception as e:  # noqa: BLE001 与其他路由同口径：内部异常回 502 JSON，不让连接被直接断开
                return self.send_json({"error": f"分析失败：{e}"}, 502)
            result["meta"]["elapsed_ms"] = int((time.time() - started) * 1000)
            sys.stderr.write("analyze %s · %s · %dms · images=%d · vision=%s%s\n" % (
                result["meta"]["category"],
                "degraded" if result["meta"]["degraded"] else "llm",
                result["meta"]["elapsed_ms"],
                result["meta"].get("images", 0),
                result["meta"].get("vision", False),
                " · " + result["warning"] if result["warning"] else "",
            ))
            return self.send_json(result)
        if path == "/api/reverse/analyze-batch":
            length = int(self.headers.get("Content-Length") or 0)
            if length > MAX_BODY:
                return self.send_json({"error": "payload too large"}, 413)
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8") or "{}")
            except ValueError:
                return self.send_json({"error": "invalid json"}, 400)
            try:
                normalize_payload_images(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 422)
            started = time.time()
            try:
                result = analyze_batch(payload)
            except ValueError as e:
                return self.send_json({"error": str(e)}, 400)
            except RequestBudgetExceeded as e:
                return self.send_json({"error": str(e)}, 504)
            except Exception as e:  # noqa: BLE001
                return self.send_json({"error": "批量反推失败：" + str(e)}, 502)
            result["meta"]["elapsed_ms"] = int((time.time() - started) * 1000)
            sys.stderr.write("analyze-batch · models=%d · %dms\n" % (
                result["meta"].get("models", 0), result["meta"].get("elapsed_ms", 0)))
            return self.send_json(result)
        return self.send_json({"error": "not found"}, 404)


if __name__ == "__main__":
    host, _, port = BIND.rpartition(":")
    host = host or "127.0.0.1"
    print(f"prompt-reverse {VERSION} → http://{host}:{port}")
    if ALLOWED_DEPLOYMENTS:
        print(f"  只走部署（REVERSE_ALLOWED_DEPLOYMENTS）: {', '.join(ALLOWED_DEPLOYMENTS)}")
    print(f"  反推模型组: {MODEL_GROUP} · 网关 {_gateway_display() or '未配置'} · "
          f"secret {'已配置' if AIG_SECRET else '未配置'}")
    ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
