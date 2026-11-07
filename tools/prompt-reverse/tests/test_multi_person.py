# -*- coding: utf-8 -*-
"""多人姿势裂变：离线提示词与配对测试（不调用外部模型）。

来源：prism-pose-fission-delivery-2026-11-07.1/tests/test_multi_person.py，改为加载同级目录的
serve_reverse.py；另加 pose_prompt 的 mapping 入参/回参测试（读图用桩函数替换，不联网）。

    python3 -I tests/test_multi_person.py      # 在 tools/prompt-reverse/ 下
"""
import importlib.util
import os

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.join(os.path.dirname(HERE), "serve_reverse.py")
spec = importlib.util.spec_from_file_location("serve_reverse", SERVER)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

SRC2 = {
    "has_person": True, "person_count": 2,
    "group": "两人并肩，P1 在左，P2 在右", "interaction": "无",
    "scene": "灰色影棚", "background": "纯色墙面", "light": "柔光", "camera": "平视全身",
    "framing": "全身",
    "people": [
        {"label": "P1", "position": "画面左侧", "primary": False,
         "person": {"gender_age": "女", "hair": "金色直发", "face": "鹅蛋脸"},
         "outfit": ["灰色条纹西装外套", "灰色短裤"], "accessories": ["白色短靴"], "pose": "站立"},
        {"label": "P2", "position": "画面右侧", "primary": True,
         "person": {"gender_age": "女", "hair": "红棕色长卷发", "face": "椭圆脸"},
         "outfit": ["米白条纹西装外套", "米白短裤"], "accessories": ["白色短靴"], "pose": "站立"},
    ],
}

REF2 = {
    "has_person": True, "person_count": 2, "group": "两人一前一后，P2 在前", "interaction": "背靠背",
    "people": [
        {"label": "P1", "position": "画面左侧", "primary": False,
         "orientation_deg": "正侧90°", "facing": "画面右侧", "stance": "站立",
         "legs": "双腿并拢", "arms": "一臂抬至颈前", "hands": "手在颈前",
         "head": "转向画面右侧", "gaze": "看画面右侧", "expression": "平静",
         "outline": "侧身站立，一臂抬至颈前"},
        {"label": "P2", "position": "画面右侧", "primary": True,
         "orientation_deg": "正面0°", "stance": "站立", "legs": "双腿分开",
         "arms": "双手叉腰", "hands": "双手在髋部", "head": "正对镜头",
         "gaze": "看镜头", "expression": "冷峻", "outline": "正面站立，双手叉腰"},
    ],
}


def test_two_person_prompt():
    prompt = m.build_pose_prompt(SRC2, REF2, mapping=[0, 1])
    for token in ("【人物配对】", "【人数与身份锁定】", "【服装锁定】", "【多人站位与互动】",
                  "人物A", "人物B", "灰色条纹西装外套", "米白条纹西装外套", "背靠背"):
        assert token in prompt, token
    assert "画面中只能有图1的 2 位人物" in prompt


def test_reverse_mapping():
    prompt = m.build_pose_prompt(SRC2, REF2, mapping=[1, 0])
    line = next(x for x in prompt.split("\n") if x.startswith("【人物配对】"))
    assert "人物A（P1，图1画面左侧） ← 图2 P2" in line
    assert "人物B（P2，图1画面右侧） ← 图2 P1" in line


def test_extra_ref_people_ignored():
    ref = dict(REF2)
    ref["person_count"] = 3
    ref["people"] = REF2["people"] + [
        {"label": "P3", "position": "画面右侧", "primary": False,
         "orientation_deg": "正面0°", "stance": "站立", "expression": "笑"}]
    prompt = m.build_pose_prompt(SRC2, ref, mapping=[0, 1])
    assert "图2共有 3 人" in prompt and "P3 不要出现在画面中" in prompt
    assert "画面中只能有图1的 2 位人物" in prompt


def test_single_person_unchanged():
    src1 = {"has_person": True, "people": [dict(SRC2["people"][1], primary=True)],
            "outfit": SRC2["people"][1]["outfit"], "accessories": [], "gender_age": "女"}
    ref1 = {"has_person": True, "person_count": 1, "people": [dict(REF2["people"][1], primary=True)]}
    prompt = m.build_pose_prompt(src1, ref1)
    assert "【人物配对】" not in prompt
    assert "【任务】" in prompt and "同一位" in prompt


def test_explicit_keep_original_pose_not_overridden():
    """显式配对里的 -1 = 保持原姿势，不能被默认配对补位。"""
    d3 = [0, 1, 2]
    assert m._norm_pose_mapping([0, 1, -1], 3, 3, d3) == [0, 1, -1]
    assert m._norm_pose_mapping([2, -1, -1], 3, 3, d3) == [2, -1, -1]
    assert m._norm_pose_mapping([0, -1], 2, 2, [0, 1]) == [0, -1]
    assert m._norm_pose_mapping([-1, 0], 2, 2, [0, 1]) == [-1, 0]


def test_mapping_falls_back_to_default_only_when_absent_or_invalid():
    d = [0, 1]
    assert m._norm_pose_mapping(None, 2, 2, d) == [0, 1]
    assert m._norm_pose_mapping([], 2, 2, d) == [0, 1]
    assert m._norm_pose_mapping([-1, -1], 2, 2, d) == [0, 1]       # 全 -1
    assert m._norm_pose_mapping([0, 2], 2, 2, d) == [0, 1]         # 越界
    assert m._norm_pose_mapping([1, 1], 2, 2, d) == [0, 1]         # 非负值重复
    assert m._norm_pose_mapping([-2, 0], 2, 2, d) == [0, 1]        # 小于 -1
    assert m._norm_pose_mapping([1], 2, 2, d) == [0, 1]            # 长度不等
    assert m._norm_pose_mapping([1, 0, -1], 2, 2, d) == [0, 1]
    assert m._norm_pose_mapping(["x", 0], 2, 2, d) == [0, 1]       # 非整数
    assert m._norm_pose_mapping([True, 0], 2, 2, d) == [0, 1]
    assert m._norm_pose_mapping("1,0", 2, 2, d) == [0, 1]
    assert m._norm_pose_mapping([1, 0], 2, 2, d) == [1, 0]
    # 3v3 全 -1 → 默认
    assert m._norm_pose_mapping([-1, -1, -1], 3, 3, [0, 1, 2]) == [0, 1, 2]


def test_keep_original_pose_prompt():
    prompt = m.build_pose_prompt(SRC2, REF2, mapping=[0, -1])
    line = next(x for x in prompt.split("\n") if x.startswith("【人物配对】"))
    assert "人物A（P1，图1画面左侧） ← 图2 P1" in line, line
    assert "图2 P2" not in line, line


def _stub_pose_llm(ref):
    calls = []

    def fake(system, text, image, max_tokens):
        calls.append(system is m.POSE_SOURCE_SYSTEM_PROMPT)
        return dict(ref), {}
    return fake, calls


def test_pose_prompt_mapping_roundtrip():
    orig, orig_cfg = m._pose_llm_json, m.llm_configured
    try:
        fake, calls = _stub_pose_llm(REF2)
        m._pose_llm_json = fake
        m.llm_configured = lambda: True
        # 不传 mapping：默认左→左、右→右
        out = m.pose_prompt({"source": SRC2, "image": "data:image/png;base64,AA"})
        assert out["mapping"] == [0, 1], out["mapping"]
        assert len(out["pose"]["people"]) == 2
        # 反转配对：mapping 原样生效并回显
        out = m.pose_prompt({"source": SRC2, "image": "data:image/png;base64,AA", "mapping": [1, 0]})
        assert out["mapping"] == [1, 0], out["mapping"]
        line = next(x for x in out["prompt"].split("\n") if x.startswith("【人物配对】"))
        assert "人物A（P1，图1画面左侧） ← 图2 P2" in line
        # -1 = 保持原姿势：原样生效并回显，不被默认配对补位
        out = m.pose_prompt({"source": SRC2, "image": "data:image/png;base64,AA", "mapping": [0, -1]})
        assert out["mapping"] == [0, -1], out["mapping"]
        # 全 -1 无效：退回默认
        out = m.pose_prompt({"source": SRC2, "image": "data:image/png;base64,AA", "mapping": [-1, -1]})
        assert out["mapping"] == [0, 1], out["mapping"]
        # 单人原图：mapping 为 None
        src1 = {"has_person": True, "people": [dict(SRC2["people"][1], primary=True)],
                "outfit": SRC2["people"][1]["outfit"], "accessories": [], "gender_age": "女"}
        out = m.pose_prompt({"source": src1, "image": "data:image/png;base64,AA"})
        assert out["mapping"] is None
        assert calls and not any(calls)
    finally:
        m._pose_llm_json, m.llm_configured = orig, orig_cfg


if __name__ == "__main__":
    test_two_person_prompt()
    test_reverse_mapping()
    test_extra_ref_people_ignored()
    test_single_person_unchanged()
    test_explicit_keep_original_pose_not_overridden()
    test_mapping_falls_back_to_default_only_when_absent_or_invalid()
    test_keep_original_pose_prompt()
    test_pose_prompt_mapping_roundtrip()
    print("multi-person tests passed")
