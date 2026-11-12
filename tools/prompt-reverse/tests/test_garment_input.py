# -*- coding: utf-8 -*-
# 来源：姿态包 2026-11-10.13 tests/test_garment_input.py（原样，加载同级 serve_reverse.py）。
"""服装参考图：离线归一化与提示词测试（不调用外部模型）。"""
import importlib.util
import os

HERE = os.path.dirname(os.path.abspath(__file__))
BASE = os.path.dirname(HERE)
SERVER = os.path.join(BASE, "server", "serve_reverse.py")
if not os.path.exists(SERVER):
    SERVER = os.path.join(BASE, "serve_reverse.py")
spec = importlib.util.spec_from_file_location("serve_reverse", SERVER)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)

REF = [
    {"slot": "上衣", "name": "白色长袖衬衫", "color": "白色", "collar": "尖角翻领",
     "closure": "单排白色小圆扣", "cuff": "单枚纽扣收口", "details": "左胸贴袋", "back": "后育克"},
    {"slot": "下装", "name": "白色高腰百褶短裙", "color": "白色", "waist": "高腰抽褶",
     "hem": "A字百褶", "side": "侧缝"},
    {"slot": "鞋", "name": "米白及膝长靴", "color": "米白", "closure": "外侧搭带与金属扣",
     "side": "拉链与靴筒褶皱"},
]


def single_source(person=0):
    return {
        "has_person": True, "person_count": 1, "outfit": ["白衬衫", "白短裙"],
        "accessories": ["白靴"], "gender_age": "女",
        "people": [{"label": "P1", "position": "画面中间", "primary": True,
                    "person": {"gender_age": "女"}, "outfit": ["白衬衫", "白短裙"],
                    "accessories": ["白靴"], "pose": "站立"}],
        "garment_refs": REF, "garment_person": person,
    }


def single_pose():
    return {"has_person": True, "person_count": 1,
            "people": [{"label": "P1", "position": "画面中间", "primary": True,
                        "orientation_deg": "正侧90°", "stance": "站立", "expression": "平静"}]}


def test_norm_garments():
    refs = m._norm_garments({"garments": REF, "missing_views": "缺少背面"})
    assert len(refs) == 3
    assert refs[0]["slot"] == "上衣" and refs[2]["slot"] == "鞋"


def test_single_prompt_has_garment_section():
    prompt = m.build_pose_prompt(single_source(), single_pose())
    assert "【服装参考图】" in prompt
    assert "图3–图5" in prompt
    assert "白色长袖衬衫" in prompt and "米白及膝长靴" in prompt
    assert "如果没有提供" in prompt
    assert "【人物配对】" not in prompt


def test_short_prompt_mentions_garments():
    prompt = m.build_pose_prompt_short(single_source(), single_pose())
    assert "服装参考图" in prompt and "白色长袖衬衫" in prompt


def test_multi_person_garment_owner():
    src = {
        "has_person": True, "person_count": 2,
        "people": [
            {"label": "P1", "position": "画面左侧", "primary": False,
             "person": {"gender_age": "女"}, "outfit": ["灰西装"], "pose": "站"},
            {"label": "P2", "position": "画面右侧", "primary": True,
             "person": {"gender_age": "女"}, "outfit": ["米白西装"], "pose": "站"},
        ],
        "garment_refs": REF, "garment_person": 1,
    }
    pose = {"has_person": True, "person_count": 2, "people": [
        {"label": "P1", "position": "画面左侧", "primary": False, "orientation_deg": "正面0°"},
        {"label": "P2", "position": "画面右侧", "primary": True, "orientation_deg": "正侧90°"},
    ]}
    prompt = m.build_pose_prompt(src, pose)
    section = next(x for x in prompt.split("\n\n") if x.startswith("【服装参考图】"))
    assert "图1的B" in section
    assert "只约束该人物的服装" in section


if __name__ == "__main__":
    test_norm_garments()
    test_single_prompt_has_garment_section()
    test_short_prompt_mentions_garments()
    test_multi_person_garment_owner()
    print("garment-input tests passed")
