#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""select_images 纯函数单测（不启动服务、不联网、不读 Key）。

    python3 -I tests/select_images_test.py -v      # 在 tools/prompt-reverse/ 下

对应设计验收：D1 总数 <=9；D2 主图 -> 细节 -> 模特，模特恰 1 张且在最后；
D3 每件单品至少 1 张；D4 sha1 去重；细节名额 max(1, 9-主图-1) 按单品轮询。

约定：断言写的是 sidecar「当前实际行为」。与验收冲突的地方用 KNOWN DEVIATION 标注：
  - 冲突点另写一条 expectedFailure 用例表达「验收期望」；sidecar 修好后它会变成 unexpected success 提醒更新。
"""
import hashlib
import importlib.util
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
SERVER = os.path.join(os.path.dirname(HERE), "serve_reverse.py")
_spec = importlib.util.spec_from_file_location("serve_reverse_under_test", SERVER)
sr = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(sr)
select_images = sr.select_images

_n = [0]


def img(role, label="", tag=None):
    """生成唯一 data URL（内容不必是合法图片，select_images 只看前缀与 sha1）。"""
    _n[0] += 1
    return {"role": role, "label": label, "data": "data:image/png;base64,%s" % (tag or "IMG%04d" % _n[0])}


def mains(role, n):
    return [img(role, "%s-%d" % (role, i)) for i in range(n)]


def crops(item, n):
    return [img("局部细节", "%s·细节%d" % (item, i)) for i in range(n)]


def run(images, category="compose"):
    return select_images({"category": category, "images": images})


def roles(out):
    return [i["role"] for i in out]


def kind(i):
    return {"局部细节": "crop", "人物模特": "model"}.get(i["role"], "main")


class SelectImagesTest(unittest.TestCase):
    def test_constants(self):
        self.assertEqual(sr.MAX_IMAGES, 9)

    # ---- D1
    def test_d1_total_never_exceeds_9(self):
        scenes = [
            mains("上衣", 3) + mains("下装", 3) + mains("鞋子", 3) + crops("上衣", 6) + crops("下装", 6) + mains("x", 0)
            + [img("人物模特", "m%d" % i) for i in range(5)],
            mains("上衣", 20),
            crops("上衣", 30),
            [img("人物模特") for _ in range(12)],
        ]
        for cat in ("compose", "look", "top", "set"):
            for s in scenes:
                with self.subTest(cat=cat, n=len(s)):
                    self.assertLessEqual(len(run(s, cat)), 9)

    def test_non_data_url_dropped_and_empty(self):
        self.assertEqual(select_images({"images": [{"role": "上衣", "data": "https://x/y.png"}, "str", None]}), [])
        self.assertEqual(select_images({}), [])

    # ---- D2
    def test_d2_order_main_then_crop_then_one_model_last(self):
        s = [img("人物模特", "m0")] + crops("上衣", 2) + mains("上衣", 2) + [img("人物模特", "m1"), img("人物模特", "m2")]
        out = run(s)  # 故意打乱输入顺序
        ks = [kind(i) for i in out]
        self.assertEqual(ks, sorted(ks, key={"main": 0, "crop": 1, "model": 2}.get))
        self.assertEqual(ks.count("model"), 1)
        self.assertEqual(ks[-1], "model")
        self.assertEqual(out[-1]["label"], "m0")  # 取输入顺序里的第一张模特

    def test_d2_no_model_means_no_model_slot(self):
        out = run(mains("上衣", 2) + crops("上衣", 2))
        self.assertNotIn("人物模特", roles(out))

    # ---- 主图上限
    def test_main_cap_compose_and_look_is_6(self):
        for cat in ("compose", "look"):
            with self.subTest(cat=cat):
                out = run(mains("上衣", 9), cat)
                self.assertEqual(len(out), 6)

    def test_main_cap_single_item_is_4_or_1(self):
        self.assertEqual(len(run(mains("上衣", 9), "top")), 4)
        self.assertEqual(len(run(mains("上衣", 1), "top")), 1)

    # ---- D3
    def test_d3_one_main_per_item_all_kept(self):
        out = run(mains("上衣", 1) + mains("下装", 1) + mains("鞋子", 1))
        self.assertEqual(sorted(roles(out)), sorted(["上衣", "下装", "鞋子"]))

    def _three_items_nine_mains(self):
        # 前端的收单顺序：单品按 items 顺序，每件 front/back/side
        return mains("上衣", 3) + mains("下装", 3) + mains("鞋子", 3)

    def test_d3_current_behavior_three_items_nine_mains(self):
        """KNOWN DEVIATION（当前行为）：3 件 x 3 张 = 9 张主图时，主图按上传顺序直接截到 6 张，
        第 3 件单品（鞋子）一张都进不了 -> 违反 D3「每件至少 1 张」。"""
        out = run(self._three_items_nine_mains())
        self.assertEqual(len(out), 6)
        self.assertEqual(roles(out), ["上衣"] * 3 + ["下装"] * 3)
        self.assertNotIn("鞋子", roles(out))

    @unittest.expectedFailure
    def test_d3_expected_each_item_at_least_one(self):
        """验收期望（D3）。目前失败，已标 expectedFailure；sidecar 改成按单品轮询后会变 unexpected success，届时删标记。"""
        out = run(self._three_items_nine_mains())
        for role in ("上衣", "下装", "鞋子"):
            self.assertIn(role, roles(out))

    def test_d3_current_behavior_details_do_not_rescue_dropped_item(self):
        """KNOWN DEVIATION：被截掉的整件单品，即使有细节图，名额也已被占满：6 主图 + 模特 -> 细节名额 2，轮询只覆盖有细节的单品。"""
        s = self._three_items_nine_mains() + crops("上衣", 2) + crops("下装", 2) + crops("鞋子", 2) + [img("人物模特")]
        out = run(s)
        self.assertLessEqual(len(out), 9)
        self.assertNotIn("鞋子", [i["role"] for i in out if i["role"] != "局部细节"])
        self.assertEqual(sum(1 for i in out if kind(i) == "crop"), 2)  # 9-6-1

    # ---- 细节名额
    def test_crop_quota_formula_with_model(self):
        for n_main in (1, 2, 3, 4, 5, 6):
            with self.subTest(mains=n_main):
                s = mains("上衣", n_main) + crops("上衣", 9) + [img("人物模特")]
                out = run(s)
                want = max(1, 9 - n_main - 1)
                self.assertEqual(sum(1 for i in out if kind(i) == "crop"), want)
                self.assertLessEqual(len(out), 9)

    def test_crop_quota_minimum_is_1(self):
        # 6 主图 + 模特 -> 9-6-1=2；用 look 6 张 + 3 张细节 + 模特：仍 2，不会 <1
        out = run(mains("上衣", 6) + crops("上衣", 3) + [img("人物模特")], "look")
        self.assertGreaterEqual(sum(1 for i in out if kind(i) == "crop"), 1)

    def test_crop_quota_without_model_gets_extra_slot(self):
        """KNOWN DEVIATION（相对设计文档公式 max(1, 9-主图-1)）：没有模特图时 model_slots=0，
        细节名额为 9-主图（多 1 张），而不是预留模特位。无模特时 analyze 本身不会成立，影响小，但与文档公式字面不一致。"""
        out = run(mains("上衣", 4) + crops("上衣", 9), "top")
        self.assertEqual(sum(1 for i in out if kind(i) == "crop"), 5)  # 9-4，文档公式会给 4

    def test_crop_round_robin_by_item_prefix(self):
        s = mains("上衣", 1) + crops("上衣", 4) + crops("下装", 4) + crops("鞋子", 4) + [img("人物模特")]
        out = run(s)
        labels = [i["label"].split("·")[0] for i in out if kind(i) == "crop"]
        self.assertEqual(len(labels), 7)  # 9-1-1
        self.assertEqual(labels[:3], ["上衣", "下装", "鞋子"])  # 第一轮每件各 1
        self.assertEqual(labels[3:6], ["上衣", "下装", "鞋子"])  # 第二轮
        self.assertEqual(labels[6], "上衣")
        self.assertEqual({l: labels.count(l) for l in set(labels)}, {"上衣": 3, "下装": 2, "鞋子": 2})

    def test_crop_round_robin_skips_empty_group(self):
        s = mains("上衣", 1) + crops("上衣", 1) + crops("鞋子", 5) + [img("人物模特")]
        labels = [i["label"].split("·")[0] for i in run(s) if kind(i) == "crop"]
        self.assertEqual(labels, ["上衣", "鞋子", "鞋子", "鞋子", "鞋子", "鞋子"][:len(labels)])
        self.assertEqual(labels.count("上衣"), 1)

    # ---- D4
    def test_d4_sha1_dedupe(self):
        a = img("上衣", "a", tag="SAME")
        b = img("上衣", "b", tag="SAME")  # 同内容不同 label
        self.assertEqual(hashlib.sha1(a["data"].encode()).hexdigest(), hashlib.sha1(b["data"].encode()).hexdigest())
        out = run([a, b, img("下装")])
        self.assertEqual(len(out), 2)
        self.assertEqual(out[0]["label"], "a")  # 保留先到者

    def test_d4_dedupe_across_roles(self):
        out = run([img("上衣", tag="S"), img("局部细节", "上衣·x", tag="S"), img("人物模特", tag="S")])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["role"], "上衣")

    def test_d4_current_behavior_dedupe_happens_after_slicing(self):
        """KNOWN DEVIATION（轻微）：去重在截断之后，重复图会白占主图名额，不会由后面的图递补。
        4 张主图里 2 张重复 -> 只剩 3 张，即使还有第 5 张可用主图也不补（top 品类上限 4）。"""
        s = [img("上衣", tag="D"), img("上衣", tag="D"), img("上衣"), img("上衣"), img("上衣")]
        out = run(s, "top")
        self.assertEqual(len(out), 3)


if __name__ == "__main__":
    unittest.main()
