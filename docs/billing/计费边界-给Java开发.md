# 计费边界约定（Go 服务 ↔ Java 服务）

> 2026-09-27 · 给 fusion-aigc 的开发看
> 背景：本地生活 App 的后端正在用 Go（aigc-life）重写，和 fusion-aigc 并行跑、**共用同一个库**。
> 点数账户是全局的（一个用户只有一份余额），所以计费表没法拆开，只能约定边界。

## 一、一句话边界

**按 `biz_type` 划分预留单的所有权：谁写的单谁负责推到终态，对方的单只读不写。**

- Java 拥有：`IMAGE_GEN` `VIDEO_GEN` `CREATION` `AI_TOOL` `COMPAT_SYNC` `EXTERNAL_EXECUTION`
- Go 拥有：**`LOCALLIFE`**（新增，只有本地生活 App 的 10 个入口码用）

共享的账户表和批次表两边都写，但**只做加减、不做重置**，且严格按同一套语义。

## 二、所有权矩阵

| 表 | Java | Go | 说明 |
|---|---|---|---|
| `afv_billing_reservation` | 写自己 biz_type 的 | 写 `LOCALLIFE` 的 | **互不 UPDATE 对方的行** |
| `afv_billing_reservation_line` | 同上 | 同上 | 同上 |
| `afv_points_log` / `_line` | 两边都写 | 两边都写 | 只追加，不改不删。靠 `biz_type` 分辨来源 |
| `afv_points_account` | 两边都改 | 两边都改 | 只 `+=` / `-=`，不赋绝对值 |
| `afv_points_batch` | 两边都改 | 两边都改 | 同上，且 FIFO 顺序必须一致 |
| `afv_billing_offer` / `_variant` / `_charge_component` / `afv_model_pricing` | 你们维护 | **只读** | 配置在你们后台改，我们只读 |
| `afv_billing_charge_rule` | 你们维护 | **只读**（见第五节） | |
| `afv_billing_quote` / `_quote_line` | 你们写 | **完全不碰** | 那是你们报价接口的 |
| `afv_external_execution` | 你们写 | **完全不碰** | |
| `afv_app_user_feature` / `afv_feature` | 你们写 | **只读** | 我们只查权益，不发不撤 |

## 三、我们保证的（Go 侧）

1. **只写 `biz_type = 'LOCALLIFE'` 的预留单和明细行**，绝不 UPDATE / DELETE 其它 `biz_type` 的任何一行。
2. **账本语义和你们一字不差**：
   `FREEZE` = `-P`，balance 减、frozen 加；
   `CONSUME` = `-实际`，**balance 不动**、只清 frozen；
   `REFUND` = `+P` 原路退；
   预留过期走 `UNFREEZE` + `expired:{no}` → 状态 `EXPIRED`；
   部分结算两条流水**各记自己那一步之后的账面**（`CONSUME` 写 `balanceBefore`）。
3. **编号与幂等键同格式**：`BR`+32hex、`PL`+32hex、`biz_id = {userId}_{requestId}`、
   `confirm:{no}` / `refunded:{no}` / `partial-consume:{no}` / `partial-refund:{no}`。
   冻结那把键用 **`golife:` 前缀**，保证不和你们的 `doubao:` / `user:N:creation:` / `ecommerce-group:` 撞。
4. **自己回收自己的过期单**：预留 90 分钟（视频）/ 15 分钟（图片文案），
   我们的回收器每分钟扫一次，且只收 `golife:` 前缀的。
   90 < 你们扫描窗口的 120 分钟（`expire_time + 30min`），中间留了 30 分钟余量。
5. **算不准就不算**：命中你们已发布的计价规则时直接报错拒绝计费（见第五节），不猜、不估。
6. **不写你们的报价表**，不碰 `afv_external_execution`。
7. **加锁顺序和你们完全一致**（见第四节第 1 条）。

## 四、双方都必须守的三条硬约束

这三条任意一条破了，都会出真问题，而且是概率性的、难查的那种。

### 1. 加锁顺序：幂等键行 → 账户 → 批次

两个服务对同一批行以不同顺序加锁，**死锁不是概率问题，是迟早的事**。
我们已经照 `doReserve` 的顺序重写了冻结流程：

```
SELECT ... FOR UPDATE by idempotency_key   （命中即返回原单）
SELECT ... FOR UPDATE 账户
再查一次幂等键
余额校验 → 改账户 → 写 FREEZE 流水 → 扣批次（FOR UPDATE）→ 最后插预留单 → 插明细行
```

**如果你们以后要改这个顺序，请提前说。**

### 2. 批次 FIFO 排序键必须一致

```sql
ORDER BY expire_time IS NULL, expire_time ASC, create_time ASC, id ASC
```

**可消费条件只看 `status='ACTIVE'`，不要加 `expire_time > NOW()`。**
（我们一开始加了，结果出现「账户余额够、批次凑不齐」的整笔失败——
因为批次过期靠那个 5 分钟的清理任务，中间有窗口期。）

### 3. 账本字段口径不能各写各的

尤其是部分结算那两条流水的 `balance_after` / `frozen_after`。
你们代码里的注释写得很清楚：按流水顺序重放对账时 `balance_after` 与 `amount` 累加要对得上。
我们照抄了这个口径。

## 五、请你们做的四件事（都不大）

### 1. 回收器给 `LOCALLIFE` 加一个显式跳过分支

`BillingHeldReservationRecoveryServiceImpl` 现在对未知 `biz_type` 走
`UNKNOWN_BIZ_TYPE` 只打 WARN——**行为已经是对的**，我们不需要你们处理这些单。

但它按 `expire_time` 升序取前 100 条，我们的在途单会排在前面反复刷日志、占名额。
希望像 `EXTERNAL_EXECUTION` 那样加一行：

```java
if ("LOCALLIFE".equals(bizType)) {
    log.debug("billing_recovery_event event=LOCALLIFE_EXTERNAL_OWNER reservationNo={}", ...);
    return false;   // 由 Go 服务自己回收
}
```

**优先级：低。** 不加也不会出错，只是日志噪音。

### 2. 修一个 bug：部分结算会报 1406

`afv_billing_reservation_line.status` 是 `varchar(16)`，而 `doPartialConfirm` 的
`updateReservationLinesStatus` 往里写 **`PARTIALLY_REFUNDED`（19 字符）**。
严格模式下整个事务回滚——**只要预留单有明细行、又走到部分结算，就一定失败**。
整单路径写的是 `CONFIRMED`(9) / `REFUNDED`(8)，所以一直没暴露。

建议把该列加宽到 `varchar(32)`。我们这边暂时往明细行写 `CONFIRMED` 绕开了。

**优先级：高**（这是你们自己的功能坏了，和我们无关）。
> 2026-10-09 补记：你们的 V1.0.166 已把该列加宽到 varchar(32)，测试库已确认。我们明细行仍写 `CONFIRMED`，两种宽度都安全。

### 3. 计价规则的作用域要能排除本地生活

你们算完基础价还会过 `PricingRuleEngine`（`afv_billing_charge_rule`，6 个因子，
其中 `userLevel` 要读充值记录加权打分、匹配等级档、还能被人工指定覆盖）。
**这套我们没有复刻**，也不打算复刻。

现在库里两条规则的 `scope_code` 是 `*`（通配），还停在 `DRAFT`。
一旦发布，我们算出来的价就会和你们不一致。

我们的处理：**算价前先查有没有已发布规则会命中我们的 offer，有就直接报错拒绝计费。**
宁可当场炸出来，也不要静默算错——那种错是用户看到的价和实际扣的对不上。

**请约定：本地生活的 9 个入口码不要进规则作用域。**
如果做不到，请提供一个作用域排除名单，或者告诉我们你们准备发布哪些规则，我们评估要不要实现。

**优先级：中**（现在没发布就没事，但要有个约定）。

### 4. 运营工具对 `LOCALLIFE` 的单只读

`/api/v1/admin/billing/compensations/execute`（retry / ignore / refund）和
过期占用扣费单的手动恢复，如果运营对我们的单点了一下，就会绕过所有约定直接改账。

希望这些运维动作对 `biz_type='LOCALLIFE'` 的单**只展示、不执行**，
或者至少加一个二次确认提示「这张单属于本地生活服务，请联系对应负责人」。

**优先级：中**（概率低但后果不可逆）。

## 六、为什么不是「你们开三个接口我们调」

我们认真考虑过让你们开 reserve / confirm / refund 三个内部接口，我们调。没选这条，原因：

1. **战略方向是 Go 逐步替换 Java**，把账务调用权留在 Java 侧，等于把要迁走的东西又加固了一层。
2. 你们现在**没有任何对外的计费接口**（`/api/v1/billing` 只有 quote 三个；
   `union` / `open` 只能充值发权益，碰不到预留表），要开就是从零做，还要做鉴权、幂等、限流。
3. 每次提交多一次跨服务 RPC，失败语义更复杂（RPC 超时了到底扣没扣？）。

**但这个决定是可逆的**：如果哪天你们觉得账务必须单一 owner，我们把
`pkg/billing` 里那三个方法的实现换成 HTTP 调用即可，业务层代码一行不用改——
我们就是按这个前提设计的接口形状（`Freeze` / `Confirm(actual)` / `Refund(reason)`）。

## 七、对账与报表

- **对账**：两边的流水都在 `afv_points_log`，`change_type` 和字段口径统一，
  所以你们现有的对账脚本能同时覆盖两边，不用改。要分开看就按 `biz_type` 过滤。
- **报表**：本地生活的视频挂 `ecom_video_gen`、图片挂 `ecom_image_gen`（产品定的共享），
  所以**用量报表里我们的消耗会并进"电商"**。要拆开只能按 `biz_type='LOCALLIFE'` 过滤——
  如果你们的报表要单列本地生活，这是唯一的切口。

## 八、另一条要约定的：动我们依赖的表要提前说

2026-10-09 出过一次事：你们的 V1.0.181 把 `afv_asset` 的 `owner_type`/`owner_id` 删了、加了必填的 `workspace_id`，
我们的素材库写入从 9/29 起全部失败，前端反馈才发现。我们已经跟上（写个人空间 id、建档时建个人空间），
装机脚本也加了列级检查。**以后动这些表请提前说一声**：
`afv_asset` / `afv_asset_item` / `afv_app_user` / `afv_team` / `afv_team_member` / `afv_workspace*` /
`afv_points_*` / `afv_billing_reservation*` / `afv_billing_offer*` / `afv_app_user_feature` / `afv_public_template_*`。

## 九、附：我们用到的配置（2026-10-10 已对照测试库核实）

> Java 对照**仓库**回复说多数查不到，原因是这些是后台手工配在库里的，仓库迁移里没有。
> 以下是**测试库 fusion_aigc 实查**的结果（`afv_billing_offer` / `_variant` / `afv_ai_model`）。线上库请对方另行核对。

| 入口码 `entry_code` | offer_code | 能力码 | 档位 | 默认模型 |
|---|---|---|---|---|
| `exploreShop` | `explore_shop` | `ecom_video_gen` | `_cloud` / `_lite` | `doubao-seedance-2-5` / `xiaoqi-video-1.0-lite-b6` |
| `shortDrama` | `short_drama` | `ecom_video_gen` | `_cloud` / `_lite` | 同上 |
| `voiceover` | `voiceover` | `ecom_video_gen` | `_cloud` / `_lite` | 同上 |
| `digitalHuman` | `digital_human` | `ecom_video_gen` | `_cloud` / `_lite` | 同上 |
| `videoRemake` | `video_remake` | `ecom_video_gen` | `_cloud` / `_lite` | `doubao-seedance-2-0-b12` / `xiaoqi-video-1.0-lite-b6` |
| `actionTransfer` | `action_transfer` | `video_to_video` | `_cloud` / **`_standard`**（应为 `_lite`） | `doubao-seedance-2-0-b8` / `xiaoqi-video-1.0-lite-b6` |
| `imageTextNote` / `poster` / `longImage` | — | 期望 `ecom_image_gen`，`PER_IMAGE` | **测试库里没有，待配** | 产品 10/11 定：GPT Image 2（`creaft-image-2`，id 181），详见《回复-计费边界-给Java开发-261010》第 9 节 |
| `aiCopywriting` | — | — | **没有** | 产品 10/11 定：不计费 |

- 以上六个视频入口的 `quality_tier` 都是 `standard`，**不能靠它区分档次**，所以我们按档位码 `<offer_code>_<cloud|lite>` 选。
- 模型码里的 `-b6` / `-b8` / `-b12` 把单价编进了码里，调价就会换码，所以不用模型码当对接键。
- `actionTransfer` 的 lite 档叫 `action_transfer_standard`，和别的模块不一致，我们按 `_lite` 找不到，要改名（见《计费入口码-后台填写指引》五点八）。
- 能力码 `ai_chat` 是 `TEXT_CHAT` 对话，不是「帮写文案」，所以 `aiCopywriting` 没有合适的能力码。
