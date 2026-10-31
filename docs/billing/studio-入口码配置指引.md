# studio 入口码怎么配（给运营）

> 2026-10-30 · 页面：Fusion AIGC 后台 → **计费管理 / 计费场景**（表 `afv_billing_offer`）
> 对应接口文档 `docs/api/api-studio.md`「计费口径」；填写方式与 `计费入口码-后台填写指引.md` 完全一致，这里只列 studio 要填的内容。
> 嫌手填麻烦，可以让开发在 Java 库执行 `deploy/sql/ops/studio-offers-2026-10-30.sql`，跑完回到本文第四节验证。

## 一、为什么必须配

商拍换装 / 姿态复刻 / 图片生成三个功能提交时，每张图先冻结一笔点数。入口码在后台没配好，提交会直接被拒（"入口码没有配置计费方案"），不会免费放行。
部署脚本 `install.sh` 里的「计费配置检查」也会把这三个入口算一遍价，缺配置会报 ERROR：`BILLING_MODE=on` 时中止安装，部署准备步骤（6/6）一律中止。所以**上线前必须配好**。

## 二、要建 3 个场景

| 场景编码 `offer_code` | 场景名称 | 入口编码 `entry_code` | 能力编码 | 分类 | 计费场景 | 状态 |
|---|---|---|---|---|---|---|
| `studio_tryon` | 商拍换装·网页版 | `studio_tryon` | `ecom_image_gen` | IMAGE | IMAGE_GEN | ENABLED |
| `studio_pose` | 姿态复刻·网页版 | `studio_pose` | `ecom_image_gen` | IMAGE | IMAGE_GEN | ENABLED |
| `studio_imagegen` | 图片生成·网页版 | `studio_imagegen` | `ecom_image_gen` | IMAGE | IMAGE_GEN | ENABLED |

⚠️ 这三个入口编码是**下划线**写法（`studio_tryon`），不是驼峰，和图文笔记（`imageTextNote`）不一样。代码里就是这么写的，一个字符都不能错。
⚠️ 入口编码必须和素材库分类 `afv_asset_category.category_code` 同名（`Go_V1.0.21__studio.sql` 已建），成品图才会归到对应分类。
⚠️ 能力编码选 `ecom_image_gen`（和图文笔记 / 海报 / 长图相同）。它要有人被发放过权益，否则开权益后所有人被挡，见第四节 ③。

## 三、每个场景下展开 `+` 配档位和步骤

**档位**（`afv_billing_offer_variant`）：每个场景 1 档。

| 字段 | 填什么 |
|---|---|
| 档位编码 `variant_code` | `<场景编码>_standard`，如 `studio_tryon_standard` |
| 档位名称 | 标准版 |
| 质量档 `quality_tier` | `standard` |
| 默认模型 | `creaft-image-2`（GPT Image 2；各库 id 不同，测试库 14、线上 181，按 code 找） |
| 默认分辨率 | 留空 |
| 包含张数 `included_outputs` | 1 |
| 结算方式 `settlement_type` | `PARTIAL` |
| 状态 | ENABLED |

**计费步骤**（`afv_billing_charge_component`）：每个档位 1 个，**只能有 1 个收费步骤**（多个会叠加扣费）。

| 字段 | 填什么 |
|---|---|
| 步骤编码 `component_code` | `gen_<场景编码>`，如 `gen_studio_tryon` |
| 所属档位 | 上面那档 |
| 步骤 `step_code` | 场景编码 |
| 类型 `component_type` | `OUTPUT`（按张） |
| 计费类型 `cost_type` | `BILLED` |
| 动作 `action_type` | **`CREAFT_V1`**（不是 IMAGE_GEN；creaft-image-2 现成的单价行是这个动作） |
| 模型 | `creaft-image-2` |
| 倍率 `price_multiplier` | `1.0` |
| `mode` | 0（页面必填的 AIGC 模式码；studio 走 ai-gateway 没有对应码，算价不用它） |
| 基础点数 `base_points` | 0 |

## 四、价格怎么来、什么时候要改

**现在是占位价，待产品定价。** 步骤指向 `creaft-image-2` 的 `CREAFT_V1` 单价行，按分辨率取价（提交时的 `resolution`，小写 1k / 2k / 4k）：

| 分辨率 | 单价（2026-10-20 调价后） |
|---|---|
| 1k | 2 点 / 张 |
| 2k | 3 点 / 张 |
| 4k | 4 点 / 张 |

提交 N 张就冻结 N 张的钱：商拍 = 模特数 × 每位张数，姿态 = 姿势数，图片生成 = `count`。

⚠️ 这几行单价被 Java 17 条图片路由和图文笔记 / 海报 / 长图共用，**不要直接改**，会连带改别人的价。产品要给 studio 单独定价，有两种做法：

1. **studio 一个价**：在「模型管理」另建一个只作价格载体的模型，在「计价规则」给它写 `CREAFT_V1` 的 1k / 2k / 4k 三行（`resolution` 小写，`price_unit` = `PER_IMAGE`），再把三个档位的默认模型和步骤的模型都改成它。
2. **四档模型各一个价**（通用 / GPT / Banana / Seedream）：每个入口按模型档建多个档位，并需要开发在 `pkg/billing/audit.go` 的 `Targets` 给这三个入口补 `Tiers`，提交时才能按档选。要走这条先告诉开发。

只是同一个模型按 1k / 2k / 4k 分价，**不需要多个档位**，写成同一个模型下的三条分辨率单价行即可。

## 五、怎么验证

1. 在 Java 库跑只读检查，`deploy/sql/ops/` 下的 `check-billing-*.sql`。它们的入口清单里**还没有 studio 三个入口**，直接跑看不到 studio，所以用 `studio-offers-2026-10-30.sql` 末尾的第 ② ③ ④ 三段复核查询（只读，可单独执行）：
   - ② 每个入口应有场景、档位、步骤，且 1k / 2k / 4k 各有一行单价，没有 `✗` 字样；
   - ③ 能力码 `ecom_image_gen` 的有效用户数不为 0；
   - ④ 不应返回任何行（有行 = 收费步骤数不是 1）。
2. 在部署机跑 `bash install.sh check-billing`（等价于 `billing-check -dsn ...`）：三个 studio 入口都应显示通过、没有 ERROR。它读配置真的算一遍价，算不出来会报"入口码没有配置计费方案"。
3. 联调：用测试号各提交一条（`Idempotency-Key` 随便给个 UUID），看返回的 `estimated_points` 是否等于「单价 × 任务数」，并在点数流水里看到对应冻结记录；失败的张数应退回。

## 六、卡住了怎么办

| 现象 | 原因 |
|---|---|
| 提交返回"入口码没有配置计费方案" | 场景没建 / 状态不是 ENABLED / 入口编码拼错（下划线 vs 驼峰） |
| 提示"档位没有计费步骤" | 档位建了没配步骤 |
| 提示"算出 0 点" | 步骤没指向有单价的模型，或单价为 0；不会被当成免费放过 |
| 提示"模型 X 的 CREAFT_V1 没有单价" | 步骤的动作不是 `CREAFT_V1`，或单价行被停用 / 已过期 |
| 点数比预期多一倍 | 一个档位挂了两个收费步骤，见第五节 ④ |
