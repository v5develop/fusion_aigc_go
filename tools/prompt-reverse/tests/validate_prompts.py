#!/usr/bin/env python3
"""姿势裂变提示词验收脚本

用法：
    python3 validate_prompts.py prompts.json [source.json]

- prompts.json：字符串数组，每条是一段提示词；空字符串会跳过。
- source.json（可选）：/api/pose/source 的响应（或其中的 source 对象），
  用于放行原图本身就有的词（例如原图就是黄色卫衣、红色礼服），
  避免把原图元素误报成姿势图泄漏。

输出每条提示词的错误/提示，最后统计错误数；有错误时退出码 1。
"""
import json
import re
import sys

REQUIRED = ["任务", "生成方式", "参考图分工", "优先级", "身份锁定", "表情与神态", "服装锁定", "场景与背景锁定",
            "光线与画质", "机位与画幅", "取景适配", "姿势替换", "姿势还原", "动作补全", "肢体结构",
            "服装细节不可发明", "参考图道具",
            "合规", "一致性", "禁止项", "输出"]
POSE_FIELDS = ["身体朝向", "姿态与重心", "步伐", "双腿", "双臂", "双手", "躯干", "肩部", "头部",
               "视线", "身体剪影", "整体动势"]
# 示例泄漏词表：按自己的姿势素材扩充；原图里本来有的词会由 source.json 放行。
LEAK = ["黄色", "姜黄", "卫衣", "束脚裤", "米兰", "大教堂", "鸽子", "篮球架", "篮球场",
        "黑色背心", "牛仔", "蓝色大衣", "风衣", "围巾", "墨镜", "腰包", "短靴", "高跟鞋",
        "飞鸟", "文件夹", "礼服", "红色长裙", "花园", "草地", "树木", "金发", "手捧花",
        "腕花", "蓬裙", "长裙"]
BROKEN = ["呈的", "呈小幅", "且稍带", "相机", "仰度", "。。", "；；", "，，", "在延伸", "长度到外", "处入"]
OUTFIT_POSE = ["双手", "单手", "一手", "手指", "手掌", "手插", "插兜", "手臂", "手肘",
               "站立", "站姿", "姿势", "动作", "叉腰", "抱胸"]
GENERIC_OK = {"帽子", "墨镜", "包袋", "栏杆", "杯子", "手机"}
POSE_LEAK = ["仰拍", "俯拍", "低角度", "近景", "特写", "全身",
             "半身", "视角", "拍摄", "感觉", "难以判断", "道具", "物体"]


def section(p, name):
    m = re.search(r"【" + re.escape(name) + r"】(.*?)(?=\n【|\Z)", p, re.S)
    return m.group(1).strip() if m else ""


def clause_hits(text, words):
    hits = []
    for part in re.split(r"[；;，,。]", text):
        for w in words:
            if w in part:
                hits.append((w, part.strip()[:80]))
                break
    return hits


def validate(p, allowed=""):
    errs, warns = [], []
    for k in REQUIRED:
        if f"【{k}】" not in p:
            errs.append(f"缺少段落【{k}】")
    ident = section(p, "身份锁定")
    expression = section(p, "表情与神态")
    priority = section(p, "优先级")
    restore = section(p, "姿势还原")
    outfit = section(p, "服装锁定")
    scene = section(p, "场景与背景锁定")
    camera = section(p, "机位与画幅")
    pose = section(p, "姿势替换")
    note = section(p, "原图备注")
    prop = section(p, "原图道具")
    if len(ident) < 80:
        errs.append("身份锁定过短/过于泛化")
    if "全部以原图为准" in ident and len(ident) < 120:
        errs.append("身份锁定泛化")
    if not priority:
        errs.append("缺少优先级")
    elif not ("还原图2" in priority or "图2" in priority) or "正面直立" not in priority:
        errs.append("优先级缺少「还原图2动作优先」或「禁止回正正面直立」")
    if not restore:
        errs.append("缺少姿势还原")
    elif "身体朝向" not in restore:
        errs.append("姿势还原缺少身体朝向")
    limb = section(p, "肢体结构")
    outfit_detail = section(p, "服装细节不可发明")
    if "第三条腿" not in limb or "每只脚只穿一只鞋" not in limb:
        errs.append("肢体结构缺少「不得多腿/一鞋一脚」约束")
    if "不得新增" not in outfit_detail or "喇叭袖" not in outfit_detail:
        errs.append("服装细节缺少「不得改袖口/新增装饰」约束")
    if not expression:
        errs.append("缺少表情与神态")
    elif not ("按图2" in expression or "复刻图2" in expression or "沿用图1" in expression):
        errs.append("表情与神态缺少图2复刻/图1回退")
    elif "复刻" in expression or "按图2" in expression:
        if "不要沿用图1的原表情" not in expression and "不要沿用" not in expression:
            warns.append("表情与神态未明确禁止沿用图1原表情")
    if len(outfit) < 60:
        errs.append("服装锁定过短")
    outfit_items = outfit.split("服装的款式、颜色")[0]
    for w, part in clause_hits(outfit_items, OUTFIT_POSE):
        errs.append(f"服装锁定混入动作词「{w}」：{part}")
    if len(scene) < 40:
        errs.append("场景锁定过短")
    if len(camera) < 30:
        errs.append("机位锁定过短")
    fields = [f for f in POSE_FIELDS if f + "：" in pose]
    if len(fields) < 3:
        errs.append(f"姿势字段不足（{len(fields)}）")
    new_pose = re.split(r"新姿势（只有动作来自[^）]*）：", pose)[-1] if "新姿势" in pose else pose
    for w, part in clause_hits(new_pose, POSE_LEAK):
        errs.append(f"姿势混入非动作词「{w}」：{part}")
    for w in BROKEN:
        if w in p:
            errs.append(f"残句「{w}」")
    for w in LEAK:
        if w in p and w not in allowed and w not in GENERIC_OK:
            errs.append(f"疑似泄漏姿势参考图元素「{w}」")
    if note:
        for w, part in clause_hits(note, ["水印", "文字", "logo", "双手", "手部", "手腕",
                                           "插入", "姿势", "动作", "过曝", "模糊"]):
            errs.append(f"原图备注混入「{w}」：{part}")
    if prop:
        for w, part in clause_hits(prop, ["手边", "双手", "单手", "手扶", "手托", "手臂",
                                           "手腕", "手持", "动态", "模糊"]):
            errs.append(f"原图道具混入动作/画质词「{w}」：{part}")
        if "优先保留" not in prop:
            warns.append("原图道具未写保留优先级")
    if not ("图1（第一张）是原图" in p and "图2（第二张）是姿势参考图" in p):
        errs.append("缺少图1/图2映射")
    if "只提交图1" not in p:
        errs.append("缺少单图回退说明（只支持一张参考图时只提交图1）")
    if "不属于动作" not in p:
        errs.append("缺少参考图道具限制")
    if "正常电商" not in section(p, "合规") and "电商内衣" not in section(p, "合规"):
        errs.append("缺少合规")
    if not (1500 <= len(p) <= 5600):
        warns.append(f"长度 {len(p)} 不在 1500–5600")
    return errs, warns, fields


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    path = sys.argv[1]
    with open(path, encoding="utf-8") as f:
        prompts = json.load(f)
    if isinstance(prompts, dict):
        prompts = prompts.get("prompts") or []
    if not isinstance(prompts, list):
        print("prompts.json 必须是字符串数组")
        return 2

    allowed = ""
    if len(sys.argv) > 2:
        with open(sys.argv[2], encoding="utf-8") as f:
            raw = json.load(f)
        src = raw.get("source", raw)
        allowed = json.dumps(src, ensure_ascii=False)

    total_err = 0
    for i, p in enumerate(prompts, 1):
        if not isinstance(p, str) or not p.strip():
            print(f"\n=== 姿势 {i} · 空槽，跳过")
            continue
        errs, warns, fields = validate(p, allowed)
        total_err += len(errs)
        print(f"\n=== 姿势 {i} · {len(p)} 字 · 动作字段 {len(fields)}/{len(POSE_FIELDS)}")
        print("  错误:", "；".join(errs) if errs else "无")
        print("  提示:", "；".join(warns) if warns else "无")
    print(f"\n总计：{len(prompts)} 条提示词，{total_err} 个错误")
    return 1 if total_err else 0


if __name__ == "__main__":
    sys.exit(main())
