# 视频做同款（video-replicate）接口契约 · 第一切片

状态：**内核（`app/replicate/core`）+ gateway-api 接线（`app/replicate/httpapi`）已完成，默认关**。路由 `/api/v1/replicate/**` 全部挂 `Auth`（用户 id 取登录态），
响应体就是 `core` 里对应 View 结构的 JSON（字段名以代码 `json` tag 为准，本页与代码不一致时以代码为准并回改本页）。迁移 `Go_V1.0.37__replicate.sql`。

开关（数据库 > 文件配置）：总开关 `afv_system_config.replicate.enabled` 存在且合法时以库为准（60 秒内生效，不用重启）；库里没有 / 值非法 / 读库失败时用 yaml `Replicate.Enabled`（默认 false；部署变量 `REPLICATE_ENABLED`）。
配了 DataSource 就装配、启动轮询器；总开关关着时写接口回 7003，读接口（limits / 分析 / job / 历史 / 我的方案）与轮询器照常，在途任务能收尾、退点。`GET /limits` 带 `enabled` 告诉前端当前是否接新提交。没配 DataSource 时不装配，所有接口回 `7003 FEATURE_NOT_READY`。
轮询器在 gateway-api 进程内（每 `PollIntervalMs` 一轮，每轮 ≤`BatchSize` 条），每条分析 / job 推进前用 `pkg/lease` 抢行租约（`afv_replicate_analysis` / `afv_replicate_job`，TTL `LeaseSeconds`，每 TTL/3 续），多实例同时跑安全。
取数按 `next_run_at` 升序（NULL 最先）：每推进一行写回 `next_run_at`（在途 3 秒；终态补退点 / 补合成按「结束多久了」退避 30 秒 ~ 30 分钟；出错 30 秒），不动 `updated_at`，`BatchSize` 再小也不会饿死后面的行。
job 的取数口径（四条各走索引的查询，内存合并按 `next_run_at` 排序）：在途（排队 / 执行中 / 提交中 / 继续中，`idx_status_next_run`）；待继续只在 `continue_deadline` 已到时（`idx_status_deadline`）；
**任何状态**下还有未结镜像（`afv_replicate_charge.status IN (0,1,6)`，走 charge 的 `idx_status` 再按主键回 job；终态后 / 待继续期间的退点 / 扣除失败靠这条补扫，间隔 15 秒 ~ 2 分钟）；
成功 / 部分成功里成片未合成（1 天内结束）或合成待重试（7 天内结束，`idx_status_finished`）。
设计依据：`fusion-aigc/.scratch/video-replicate-agent/DESIGN.md` §5.3–§5.17、§8.2；文案：同目录 `EXPERIENCE.md` §4。

信封与其它 Go 接口一致：`{"code":0,"msg":"ok","data":{…}}`；业务错误 `code≠0`，`reason` 见 xerr 细分（409 一律 `code=4002 RESOURCE_CONFLICT` + 下表 reason）。
所有写接口必须带 `Idempotency-Key`（≤64 字符，同用户唯一）；数字与「已退回」等字样全部由后端字段驱动，前端不得写死。

## 1 接口

| 方法 路径 | core 方法 | 请求要点 | 响应 |
|---|---|---|---|
| `GET /limits` | `Service.Limits` | — | `LimitsView` |
| `POST /analyses` | `CreateAnalysis` | `{sourceUrl}`（≤1024 字符；本人在本站上传的 http(s) 地址，或 `mr://rid`，见 §1.1） | `AnalysisView` |
| `GET /analyses/{no}` | `GetAnalysis` | — | `AnalysisView` |
| `POST /plans` | `CreatePlan` | `{analysisNo, annotations, refImages[], modelGroup, resolution?, aspectRatio?}` | `PlanView`；缺项时 `code=1003` + `data.gaps[]` |
| `POST /plans/{no}/quote` | `QuotePlan` | — | `PlanView`（重算价格，`cost.validUntil` = 现在 +10 分钟） |
| `POST /plans/{no}/confirm` | `Confirm` | `{planHash, stage: first\|all\|"", expectedPoints}`（**必填**） | `JobView` |
| `GET /jobs?page=&size=` | `ListJobs` | page 从 1 起；size 默认 20、最大 50 | `JobList`（§5.2） |
| `GET /jobs/{id}` | `JobView` | — | `JobView` |
| `POST /jobs/{id}/continue` | `Continue` | `{planHash, expectedPoints}`（**必填**） | `JobView` |
| `POST /jobs/{id}/stop` | `Stop` | — | `JobView`（只交付第 1 段） |
| `POST /jobs/{id}/segments/{k}/redo` | `Redo` | `{expectedPoints}`（**必填**） | `JobView` |
| `PUT /pref` | `SetAutoContinue` | `{autoContinue: bool}` | — |
| `POST /jobs/{id}/segments/{k}/save` | `SaveSegment` | — | `{segIndex, library}`（已扣费的段收尾时已自动入库，这里返回已有状态；没入的补入，幂等；入库异步，见 §8.2） |

`expectedPoints`：页面上展示的本次预留点数（确认 = 所选阶段的报价，继续 = 决策卡「继续」选项的 `points`，重做 = 该段的 `points`），确认 / 继续 / 重做**都必填**：缺失或 ≤0 → `code=1002 PARAM_MISSING`（HTTP 200，与其它参数错误一致），在查重放、限频、动钱之前拦下；与后端重算不一致 → 409 `QUOTE_CHANGED`，不动钱。
**发送前二次确认（§13.3）**：`afv_system_config.replicate.confirm_threshold_points` > 0 且方案总价（全部段合计）超过它时，确认的 `expectedPoints` 必须等于本次现算的预留点数，否则同样 409 `QUOTE_CHANGED`（用户端价格不一致只有这一个码，`msg` 区分缺失 / 不一致），不建 job、不动钱。
幂等键取请求头 `Idempotency-Key`（兼容旧头 `X-Request-Id`）；confirm / continue / redo 缺键 → `1002`「缺少请求标识」。`PUT /pref` 返回 `{autoContinue}`。

### 1.1 输入来源与归属（不合规 → `1003` + 可读原因；方案里的参考图是 `data.gaps[]`）

| 输入 | 接受 | 拒收 |
|---|---|---|
| `sourceUrl` = `mr://{rid}` | rid 合法且是**用户上传类别**（`{实例}-user-{26 位}`） | 其它类别 / 非法 rid：「这条视频不是从本站上传的，请先上传」 |
| `sourceUrl` = http(s) | 本站存储签发（域名由存储配置推导：`storage.Store.Owns`，或 `Replicate.InputURLPrefixes` 自有前缀），且路径**必须**以上传布局 `…/<image\|video>/<userId>/<yyyymmdd>/<文件名>` 结尾、userId = 当前用户；分析的 ingest 阶段经媒体服务拉入私有桶得到 rid | 第三方域名 / 别人的上传目录 / 不符合上传布局 / 路径含空段、`.`、`..`、编码的 `/` `\` / 没装配存储：「这条视频不是你在本站上传的文件…」 |
| `refImages[]` = `mr://{rid}` 或裸 rid | 同上：用户上传类别 | gap `REF_NOT_OWNED` |
| `refImages[]` = http(s) | 同 sourceUrl 的归属判据；通过后**提交方案时**经媒体服务 `IngestURL(purpose=user_input)` 拉入成 rid（幂等键 = 前缀 + `rpl-ref:` + sha256(用户, 地址)） | `REF_NOT_OWNED`（不是本人 / 本站）、`REF_UNREADABLE`（媒体服务读不了）、`REF_UNAVAILABLE`（没配媒体服务）；媒体服务暂时性错误按服务错误返回，稍后重试 |

限制说明：媒体服务没有「rid → 用户」的归属记录，`mr://` 只能限制为「本站用户上传来源」，不能证明是当前用户上传的；http(s) 上传目录能证明到用户。
不符合上传布局的本站地址（证明不了属于谁）一律拒收。不设第三方域名白名单。

## 2 409 reason

| reason | 何时 | 文案（`msg`） |
|---|---|---|
| `PLAN_STALE` | `planHash` 与方案 / job 不一致；并发下方案已被别的 job 占用；方案已被改标注取代（§10） | 方案刚更新了，请再看一眼价格 |
| `QUOTE_CHANGED` | `expectedPoints` 与重算价不同（确认 / 继续 / 重做）；确认时方案总价超过 `replicate.confirm_threshold_points`（>0 才启用）且 `expectedPoints` 缺失或与本次现算的预留点数不一致（§13.3）也用这个码。用户端价格不一致只有这一个码（`POINTS_CHANGED` 只用于后台退点，§9） | 价格有变化：340 → 380 点。确认后按新价格生成；超阈值时：点数有变化：130 → 120 点，请再确认一次 / 这次要预留 120 点，请确认点数后再提交 |
| `PLAN_EXPIRED` | 方案过期（7 天）或不是 READY | 这个方案已过期。点「重新生成方案」，不用重新分析 |
| `IN_PROGRESS` | 同一 job 的继续 / 重做正在提交 | 正在提交… |
| `NOT_WAITING` | 继续 / 不继续时 job 不在待继续 | 第 1 段还没做好… / 这一步已经处理过了 |
| `SEGMENT_OK` / `SEGMENT_BUSY` | 重做成功段 / 进行中的段 | 这一段已经做好了 / 这一段还在生成 |
| `REDO_LIMIT` | 每段 3 次、每 job 10 次 | 这一段已经重做了 3 次，换个模型或调整方案再试 |
| `NOT_REDOABLE` | job 状态不允许重做（提交中 / 待继续 / 已取消…）；该段没开始做（提交失败取消） | 这个任务现在不能重做 / 这一段没有开始做，不能重做 |
| `SUBMIT_TIMEOUT` | 确认 / 继续 / 重做在逐段预留期间超时、被回收器接管 | 提交超时，已冻结的点数会自动退回。请重试 |
| `MODERATION_PENDING` | 确认时方案的内容检查（后台预审）还没出结论，等了 `replicate.moderation.confirm_wait_ms`（默认 3 秒）仍没有（§8.3） | 内容检查还没完成，几秒后再点确认。（没有扣点数） |
| `SEGMENT_NOT_PAID` | `POST /jobs/{id}/segments/{k}/save`：这一段出了片但没有确认扣费（不计费 / 已退点），不进素材库（§8.2） | 这一段没有收费，不保存到素材库，你仍可下载 |

其它：余额不足 `code=3003`「点数不足，还差 N 点」；入口未配价 `code=5001`「该功能暂未配置价格，请联系客服」；多段视频在区间切段上线前 `code=7003`；
预留结果未知（冻结已返回但读不出状态）等非业务错误 `code=5001`「没能提交，请稍后重试；如有冻结的点数会自动退回」（不说「没有扣点数」）。

## 3 状态枚举

| 对象 | 字段 | 取值 |
|---|---|---|
| 分析 | `status` | `queued` `running` `succeeded` `failed` |
| 分析阶段 | `stages[].key` / `status` | `read` `frames` `lines` `understand` / `done` `active` `pending` `skipped`（无声跳过台词） |
| 方案 | `status` | `READY` `CONFIRMED` `EXPIRED` `STALE`（`STALE` = 被改标注取代，见 §10；库里只存 READY / CONFIRMED / STALE；`EXPIRED` 是视图按 `expiresAt` 派生的。过期后同输入再 `POST /plans`：复用同一张方案并刷新 `expiresAt` 与报价） |
| job | `status` | `submitting`（逐段预留） `queued` `running` `waiting`（第 1 段好了，等继续） `continuing`（预留其余段中） `succeeded` `partial` `failed` `canceled` |
| 段 | `segments[].state` | `not_started`（等继续） `queued` `retrying`（自动重试中，不多收） `running` `succeeded` `failed` `canceled` |
| 决策卡 | `decision.kind` / `options[].action` | `continue`：`continue` `adjust` `stop`；`redo`：`redo`（带 `segIndex`、`points`） |
| 切点 | `cutReason` | `cut` 镜头切换处 / `pause` 说话停顿处 / `even` 按时长拆分处 / `end` 结尾 |
| 确认方式 | `cost.defaultStage` | `first`（多段默认）/ `all`（单段，或用户勾了「以后直接全部做」） |

任务中心映射（接线时）：`submitting/queued/running/waiting/continuing` → 处理中，`succeeded`→2，`failed`→3，`canceled`→4，`partial`→5。

## 4 PlanView（`POST /plans`、`POST /plans/{no}/quote`）

```json
{
  "planNo": "VRP…", "planHash": "64hex", "status": "READY",
  "title": "这条方案会改什么",
  "overview": "换商品 · 重新配乐 · 共 32 秒 · 3 段",
  "changes": [{"key":"product","label":"商品","mode":"replace","modeLabel":"换成我的图","current":"白色运动鞋","refs":[1],"risk":"mid"}],
  "kept":    [{"key":"model","label":"人物","mode":"keep","modeLabel":"不改","current":"长发女生","risk":"high"}],
  "risks":   ["画面文字将在生成后由系统叠加，不会交给 AI 画"],
  "timeline": {"title":"这条视频会拆成 3 段分别生成，再拼成一条","explain":"…","single":false,
               "seamTip":"这里是两段的拼接处，会多留 0.4 秒让画面接得更顺",
               "segments":[{"index":1,"t0":0,"t1":12,"genDuration":12,"cutReason":"cut","cutLabel":"镜头切换处","points":120}]},
  "modelGroup": "vid.seedance-2.5",
  "cost": {
    "lines":[{"index":1,"seconds":12,"points":120},{"index":2,"seconds":14,"points":140},{"index":3,"seconds":8,"points":80}],
    "first":120,"rest":220,"total":340,"restSegments":2,"totalSeconds":34,
    "validUntil":"2026-11-14T10:10:00+08:00","defaultStage":"first",
    "firstText":"现在先做第 1 段：120 点","restText":"满意之后再做剩下 2 段：约 220 点（现在不收，你点「继续」才预留）",
    "totalText":"全部做完共约 340 点","explain":"按每段生成的秒数计点。哪一段没生成成功，就退哪一段的点数",
    "overlapText":"段与段之间多留的 0.4 秒也按秒计点","holdText":"确认后先预留点数…"
  },
  "stageQuotes": {"first":120,"rest":220,"total":340},
  "eta": {"first":{"lowSec":240,"highSec":360,"text":"预计 4–6 分钟"},
          "rest":{"lowSec":240,"highSec":360,"text":"预计 4–6 分钟"},
          "total":{"lowSec":240,"highSec":360,"text":"预计 4–6 分钟"},"prior":true},
  "policy": {"watermark":false,"text":"成片：不带水印"},
  "confirmButton": "先做第 1 段（120 点）",
  "copyright": "默认会改写原视频的台词和画面文字，只沿用结构和节奏",
  "prompts": ["参考 @视频1 …"],
  "expiresAt": "2026-11-21T10:00:00+08:00",
  "jobId": 0,
  "analysis": {"summary":"看懂了：3 个镜头，…","durationS":32}
}
```

- `stageQuotes` 与 `cost.first/rest/total` 同值（后者保留给费用块文案）；单段时 `rest=0`、`firstText/restText` 为空、`eta.first/rest` 省略。
- `eta`：区间秒数，`R = ceil(N/最多同时在途段数)` 轮 × 每轮 [P50, P90]；每轮耗时优先取**历史统计**（§13.1，`prior=false`），该桶样本不足时回落先验（默认 4/6 分钟，**目标假设**，`prior=true`）。
  字段形状不变；文案按分钟向上取整，上下限相同时只写一个数（「预计 4 分钟」）。
- `policy`：成片规则，付费前必须展示；规则未定时第一期一律不带品牌水印；「AI 生成」角标待法务确认，后端未承诺前不返回该文案。
- 缺项（`code=1003`）：`data.gaps = [{"code":"REF_MISSING","field":"dims.product","message":"要把商品换掉，需要一张你的商品图。放一张正面清晰的就行"}]`，
  code 取值 `MODEL_UNKNOWN REF_TOO_MANY REF_EMPTY REF_MISSING REF_UNKNOWN REF_NOT_OWNED REF_UNREADABLE REF_UNAVAILABLE DIM_UNKNOWN DIM_ABSENT DIM_MODE NOTE_MISSING NOTE_TOO_LONG AUDIO_MODE NOTHING_CHANGED RESOLUTION_UNSUPPORTED ASPECT_UNSUPPORTED`。
- `resolution` 白名单 `480p` `720p`（默认）`1080p`；`aspectRatio` 白名单 空（跟随原片）`16:9` `9:16` `1:1` `4:3` `3:4` `21:9`。

`annotations`：`{"dims": {"<key>": {"mode":"keep|replace|describe","note":"…","refs":[1]}}, "audio":"regenerate|keep|mute"}`，
key 为十一项：`model garment accessory product motion expression line camera light scene style`；`audio` 默认 `regenerate`（重新配乐）。

## 5 JobView（confirm / continue / stop / redo / GET）

```json
{
  "jobId": 42, "taskId": "AiCreation_LF_…", "planNo": "VRP…", "planHash": "64hex",
  "status": "waiting", "statusLabel": "第 1 段做好了", "staged": true,
  "segTotal": 3, "segDone": 1, "segFailed": 0, "overview": "已完成 1/3 段",
  "concurrencyText": "最多同时做 3 段，其余排队",
  "segments": [
    {"index":1,"state":"succeeded","stateLabel":"已完成","points":120,"videoRid":"…","videoUrl":"https://…(签名)","coverRid":"…","coverUrl":"https://…","redoable":false,"redos":0},
    {"index":2,"state":"not_started","stateLabel":"满意第 1 段后再做","points":0,"redoable":false,"redos":0},
    {"index":3,"state":"failed","stateLabel":"没成功","text":"这一段这次没生成出来","moneyText":"已退回 80 点",
     "points":80,"redoable":true,"redoLabel":"重做这一段（80 点）","redos":0}
  ],
  "money": {"held":0,"confirmed":120,"refunded":80,"refundPending":0,"chargePending":0,"text":"共花费 …"},
  "result": {"status":"ready","kind":"full","videoRid":"…","videoUrl":"https://…","coverRid":"…","coverUrl":"https://…","text":"整条成片已拼好"},
  "urlExpiresAt": "2026-11-14T11:05:00+08:00",
  "decision": {"kind":"continue","title":"第 1 段做好了，和原片对比看看。",
    "options":[{"action":"continue","label":"满意，继续做剩下 2 段（预计 220 点）","points":220},
               {"action":"adjust","label":"调整后重做第 1 段"},{"action":"stop","label":"不继续了"}],
    "autoContinueLabel":"以后直接全部做（不再先看第 1 段）",
    "expiresAt":"2026-11-15T10:05:00+08:00","remainingText":"还剩 24 小时"},
  "message": "", "error": ""
}
```

- `moneyText` 只在退回真正成功后才是「已退回 N 点」；退回调用失败、等补扫重做时是「退点正在处理中，一般 5 分钟内到账」，同时 `money.refundPending>0`。
- 扣费失败与退点失败分开：段已出片、确认扣除调用失败（等重试，确认前不交付、不会重复扣）计入 `money.chargePending`，文案「有段已经做好，正在确认扣点数…」，不说「退点」。
- `*Url`：媒体服务现签的播放 / 下载地址（`mode=signed`），有效期 `Replicate.URLTTLSeconds`（默认 1 小时，上限 24 小时），到期时间 `urlExpiresAt`；地址不入库，过期后重新 `GET /jobs/{id}` 即可。
  没配媒体服务或现签失败时只给 rid（不报错）。`coverRid/coverUrl` 是首帧（网关结果里有才有；成片封面取第一个成功段的首帧）。
- 预留失败的重做（`HOLD_FAILED` / `HOLD_ABANDONED`）不算该段结果：段状态、重做次数、汇总、成片都跳过它，不挡后续重做（请求号 / 步骤键编号照常递增，不复用）。
- `decision.kind=redo` 出现在 `partial` / `failed`，每个可重做的失败段一个 `redo` 选项。`adjust` = 回到方案编辑生成新方案（前端导航，后端无接口）。
- 到期 / 不继续：`status=succeeded` 且 `message` 为「超过 24 小时没有继续，已为你结束，只保留了第 1 段，没有收费」/「好的，已结束。第 1 段留给你，可以下载，没有再收费」。

## 5.1 整条成片（`JobView.result`，`afv_replicate_job.result_document`）

job 到「成功 / 部分成功」后由轮询器合成（钱与合成无关：段级早已扣除 / 退回，合成失败不动账）：

- 多段：第 2 段起去掉衔接重叠（`/video/cut` 切 `[overlapIn, overlapIn+段长]`，取整多生成的尾巴一并去掉）→ `/video/concat` 按段序拼接；
  只有一段且没有叠字时直接用该段视频，不调媒体服务。
- 失败段占位预览版：失败段用占位画面（`placeholder{duration=段长, text, font_rid}`），文字取 `replicate.placeholder_text`（默认「这一段还在重做」）；
  没配中文字体 `replicate.font_rid` 时用英文「Segment N is being redone」并打 WARN（`result_document.warnings` 含 `PLACEHOLDER_NO_FONT`）。
  用户不继续 / 到期结束的段不占位（`kind=partial`）。段重做成功后合成依据变化，自动重新合成完整成片。
- 文字叠加（店名 / 价格 / 标题）：方案 `annotations.texts=[{text, kind?: shop_name|price|title|other, box?: {x,y,w,h}（0~1，缺省底部横条）, t0?, t1?, color?: #RRGGBB}]`，
  ≤4 处、每处 ≤30 字；缺项码 `TEXT_TOO_MANY TEXT_EMPTY TEXT_TOO_LONG TEXT_BOX TEXT_TIME TEXT_COLOR`（字段 `texts[n].…`）。只叠字也算「有改动」。
  拼好后走 `/video/overlay-text`；没配字体时不叠字（WARN `TEXT_NO_FONT`）。
- `result`：`{status: ready|retry|failed|unavailable, kind: full|preview|partial, videoRid, videoUrl, coverRid, coverUrl, missing[], text}`；合成失败自动重试 3 次后 `failed`（只交付逐段视频）；
  没配媒体服务为 `unavailable`。缺段（没成功，且不是用户不继续 / 到期）一律占位，不静默省略。

## 5.2 历史列表（`GET /jobs?page=&size=`）

```json
{"items":[{"jobId":42,"taskId":"…","planNo":"VRP…","status":"succeeded","statusLabel":"做好了！","title":"看懂了：3 个镜头…",
           "segTotal":3,"segDone":3,"points":340,"sourceThumbRid":"…","sourceThumbUrl":"https://…","coverRid":"…","coverUrl":"https://…",
           "createdAt":"2026-11-14T10:00:00+08:00"}],
 "total":12,"page":1,"size":20,"urlExpiresAt":"2026-11-14T11:00:00+08:00"}
```

只列本人（按 id 倒序）；`points` = 已扣除 + 预留中 + 扣费确认中（已退回不算）；`title` / `sourceThumbRid` 确认时从分析结果冻结（一句话概述 / 首帧），没有时 `title` 为「视频做同款」。

## 5.2 配置与运行参数

```yaml
Replicate:                  # gateway-api
  Enabled: false
  LongformEnabled: true     # 多段视频（装配 /video/cut 区间切段）；配了 Media 才生效；运行时 replicate.longform_enabled 可应急关
  PollIntervalMs: 2000
  BatchSize: 20
  LeaseSeconds: 120
  Billing: { Mode: "off" }  # off / shadow / on（YAML 里 off 要加引号）
  AiGateway: { BaseURL: "", KeyID: "", Secret: "", LongPollWait: 20, TimeoutSeconds: 30 }
  Media: { NacosServerAddr: "", BaseURL: "", Token: "" }
  ASRModelGroup: ""         # 空 = 跳过台词
  VisionModelGroup: ""      # 空 = vlm.qwen-3.8-flash
  KeyPrefix: ""             # 网关 / 媒体服务幂等键环境前缀（[A-Za-z0-9._-]，≤32；非法拒收按空）。确认时冻结进 job.input、分析开始时冻结进 analysis.state
  Storage: { Mode: oss, Prefix: aigc-local-life }  # 可选：校验 http(s) 输入归属；须与顶层 Storage 一致
  InputURLPrefixes: []      # 可选：另认的自有地址前缀（素材库 / 媒体签名域名），不放第三方
  URLTTLSeconds: 3600       # 视图里播放地址的签名有效期
```

装配：`httpapi.MustBuild(c.Replicate, conn, httpapi.WithStorage(svcCtx.Storage))`（推荐，与上传接口同一份存储配置）；不传 `WithStorage` 时用 `Replicate.Storage`；
两者与 `InputURLPrefixes` 都没配时 **http(s) 源视频 / 参考图一律拒收**（启动日志 ERROR），只接受 `mr://` 上传引用。

`afv_system_config`（`deleted=0`，60 秒缓存；值非法的键忽略并按「判据 / 键 / 原值」告警；查库失败沿用上一次）：
`replicate.enabled`（bool）、`replicate.longform_enabled`（bool）、`replicate.auto_retry_max`（0~3）、`replicate.redo_max_per_segment`（1~10）、
`replicate.redo_max_per_job`（1~50）、`replicate.continue_deadline_hours`（1~168）、`replicate.plan_ttl_hours`（1~720）、`replicate.max_in_flight`（1~6）、
`replicate.max_segments`（1~6）、`replicate.overlap_ms`（0~2000）、`replicate.max_cost_cny`（正数）、`replicate_vision`（模型组码）、
`replicate.font_rid`（中文字体 rid）、`replicate.placeholder_text`（≤40 字）、`replicate.confirm_threshold_points`（0~100000000，默认 0 = 不启用，§13.3）、
`replicate.analysis_rules`（JSON，分析纠偏规则配置，空 = 默认，§13.2）。

## 6 钱的规则（实现口径）

- 每段一张预留单（pkg/billing，`entry_code=video_replicate`，按模型组 + 秒数报价）；请求号**从不复用**：`replicate-<job>-<kk>`（确认）、`-c<n>`（第 n 次继续）、`-u<n>`（第 n 次重做）。
- 段成功：先确认扣除成功才置成功；预留已失效（`ErrChargeGone`：到期被 Reclaimer 回收 / 已退回）则不交付，段失败 `BILLING_GONE`（可重做，没有扣点），镜像按真实状态收敛为已退回，这段供应商成本平台承担（日志带 costCny）。
  段最终失败才退回；瞬时错误自动重试 `replicate.auto_retry_max` 次（默认 1；`#r1`、`#r2`…，共用原预留单，不另扣）；内容策略 / 输入拒绝 / 结果未知不自动重试。
- 预留到期：视频预留默认 200 分钟（`Billing.Hold.VideoMinutes`）到期并被 Reclaimer 回收。`billing.Renew` 把到期设成「现在 + 15 分钟」，所以只在冻结
  ≥ 预留时长 − 15 分钟（+1 分钟时钟余量，默认 186 分钟）后才逐轮续期——任何一次续期都不会缩短到期时间。起算点是 `afv_replicate_charge.held_at`
  （冻结成功、镜像写成已预留的时刻，应用时钟，写一次不再改；`Go_V1.0.41` 补列）——镜像 `updated_at` 会被扣费待重试等写路径改写，不能用。续哪些单：
  已预留且链上还有排队 / 在途步骤的；**扣费待重试**（6 + `confirm:`，段已出片、确认扣除失败等重试）的单不论 job 状态（在途 / 待继续 / 终态）都续，
  避免重试拖过到期被回收、变成出了片却扣不到。没有 `held_at` 的旧行：已预留回退 `updated_at`，扣费待重试推不出冻结时刻、不续（宁可不续也不缩短）。
  镜像修正（待预留 → 已预留）把 `held_at` 记成修正时刻（晚于真实冻结，续期只偏晚）。续期失败 ERROR 判据=RENEW_FAILED（带累计次数）。领段（PENDING → RUNNING）前再查一次真实状态，已不是 HELD 就不出片（`BILLING_GONE_BEFORE_START`）。
- 镜像修正：排队步骤的镜像卡在「待预留」超过 5 分钟（冻结后写镜像失败 / 崩溃）时按预留单真实状态修正（HELD → 已预留；已退回 / 回收 → 已退回；没有单 → 预留失败，绝不当不计费放行）；
  排队步骤的镜像已退回 / 预留失败时置失败 `BILLING_GONE`（可重做），job 照常收尾。冻结成功但写镜像失败按失败处理：退回本次已冻结的并中止。
- 并发确认：确认扣除报预留失效时先查真实状态，已扣除（并发的另一次确认）按成功交付；pkg/billing `SettleCost` 捞单落空时重读状态，已扣除返回 Already（不再误报 ErrGone）。
  要退的单已扣除（`ErrChargeConfirmed`）：镜像收敛为已扣除并 ERROR 判据=REFUND_ON_CONFIRMED，需人工核对，不再补扫。
- 冻结：请求路径整体时限 ReapAfter/2（默认 2.5 分钟），每冻结一段刷新 job 心跳（`updated_at`），回收器只回收超过 ReapAfter（5 分钟）没有心跳的「提交中 / 继续中」；
  心跳发现已被回收 → 自己退回刚冻结的单（`SUBMIT_TIMEOUT`）。冻结报错一律按「可能已冻结」按请求号幂等退回一次（没有单才记预留失败）。
- 退回 / 确认扣除都脱离请求上下文执行（客户端断开、停机也做完，单次 15 秒时限）；失败的镜像记 6-结算待重试（`reason` 前缀 `confirm:` 扣费 / `refund:` 退点），由 DueJobs 补扫重做。
- 重做次数：`redo_count = redo_count + 1` 与上限在同一条件里原子占名额，**同一条语句**把 job 置回执行中（终态 job 上的重做在预留期间不再被终态补扫误退）；
  预留没成功还回名额并把 job 还原为原终态；卡住的 HOLDING 由取消它的一方（轮询器 / 请求）还名额，终态 job 上也回收。汇总到终态时库里再核「没有未终态出片步骤」，与并发重做互斥。
  镜像收敛时排队 / 重做预留中的步骤一律算在途；提交回收留下的孤儿 PENDING 步骤由联表条件更新（job 仍是终态）先取消。
- 对账（sqlc 只读）：`db/audit/query/replicate_reconcile.sql` → `auditdb.ReplicateChargeBillingMismatch`、`ReplicateSegmentRetryAudit`。
  注意：`ReplicateSegmentRetryAudit` 目前按 `LIKE '%#r1'` 统计自动重试，`auto_retry_max>1` 时 `#r2…` 会被算进 user_redos，需要同步改该查询（本切片未改 db/audit）。


## 8 上线补齐（next-phase P0-3/4/5/6，迁移 `Go_V1.0.39__replicate_ops.sql`）

### 8.1 任务中心（P0-3）

视频做同款 job 是 `pkg/taskcenter` 的**新同步源**（`httpapi.TaskSource()`，`afv_replicate_job → afv_creation_task`），由 gateway-api 轮询器每 5 秒同步一轮、写接口成功后 `AsyncSync.Kick()` 即时触发；`Replicate.TaskCenterEnabled`（默认 true）可关。
选「新同步源」而不是「直接写」：`pkg/taskcenter` 只提供 Source + Syncer 这一种写法，SYNC_SPEC 要求任务中心行的全部列由同一条 upsert 子句派生（legacy / new / shadow 与对拍都建立在 Source 上），另开直写口等于共享表第二个写者。

| 任务中心列 | 值 |
|---|---|
| `task_id` / `origin_ref` | `taskcenter.Ref("rpl", job.id)`（= `JobView.taskId`）/ `rpl:<job id>`；`origin_service=LOCAL_LIFE` |
| `type` / `entry_code` | `video` / `video_replicate` |
| `status` | 成功 2、失败 3、已取消 4、部分成功 5；提交中 / 排队 / 执行中 / 待继续 / 继续中 → 1 处理中 |
| `result_url` / `cover_url` | 已入素材库的**公有桶**地址（§8.2），按 完整成片（`final`）> 预览版（`preview`）> 第 1 个已入库的段（段号最小）取一条——部分成功的任务也有可点开的成品；封面取同一条，没有回退该条地址；都没入库前为空（私有 rid 不进共享表） |
| `error_msg` | `job.error_reason` |
| `request_params` | `{entryCode, title, jobId, planNo, link:"/video/replicate?job=<id>", segTotal, segDone, segFailed, progress, points}`：`progress` = 已完成段 / 总段（0~100），`points` = 已扣除 + 预留中 + 扣费确认中（与历史列表同口径） |
| ↳「已完成 N/M 段」 | 读 `request_params.segDone`（N，已成功的段数）/ `request_params.segTotal`（M，总段数），两个都是整数、键名区分大小写（camelCase）；`segFailed` 为最终失败段数。展示文案由读方拼（Go 不写文案进共享表） |
| `billing_status` | 恒 `NONE`：一个 job 多张预留单（每段一张），`Source.BillingRequest` 只能关联一张，关联第 1 段会把单段价当整单价；点数走 `request_params.points` |

同步水位用 `afv_replicate_job.tc_updated_at`（**没有 ON UPDATE**：只在业务状态写路径显式 `SET tc_updated_at = NOW(3)`——状态迁移 / 汇总段数 / 成片 `result_document` / 继续 / 重做占名额 / 预留镜像变化 / 成片入库；租约抢占与续租、`next_run_at`、提交心跳都不碰它，避免任务中心同步抖动）。
**注意**：Java 任务中心的 `progress` 目前由 `CreationTaskProgressEstimator` 按时间外推，不读 `request_params.progress`；「已完成 N/M 段」由 Java 对 `origin_ref LIKE 'rpl:%'` 的行读 `segDone` / `segTotal`（Java 侧另行处理，Go 不改）。
工作空间：任务中心表没有工作空间列（按用户归属）；`afv_replicate_job.workspace_id` 在确认时写用户**个人空间**（`pkg/workspace.PersonalID`，同 studio / 画布；查不到记 0、不挡确认）。
Go 网关的登录态只解析出 `userId`（共用会话的 `currentWorkspaceId` 没有带进请求上下文），所以拿不到「当前选中的空间」；素材库归属由 `pkg/asset` 统一落个人空间。

### 8.2 素材库（P0-3）

- **收尾自动入库（花了钱的产物必须进素材库）**：job 到成功 / 部分成功（含后台强制收尾后的汇总，由轮询器补合成时触发）时入队 `afv_replicate_delivery`（唯一键 `job_id+kind+seg_index`，幂等：同一条只入一次）：
  - 每个**已确认扣费**（镜像 `ChargeConfirmed`）的成功段一条 `kind=segment`，素材名「<方案标题> · 第 N 段」；不再依赖用户手动保存。
  - 成片可用（`result.status=ready`）时：完整版 / 用户不继续或到期的部分成片一条 `kind=final`（素材名 = 方案标题）；预览版（失败段占位）一条 `kind=preview`，素材名「<方案标题> · 部分完成」。
  - 之后重做成功拼出完整成片：完整版另入一条 `final`（新资产），**预览版保留**；同 job 预览版只入一次（重做后仍是预览版不再另入）。
  - 按内容去重：同一个源 rid 只入一条，成片先入。单段且无叠字（或部分成片就是唯一做好的那一段）时成片与该段是同一个视频，只入 `final`，不再入 `segment`；手动保存这一段命中这条 `final`。
  - 只入已确认扣费的产物：已退点的失败段、不计费（`Billing.Mode` off / shadow、0 点）的段、扣费还没确认的段不入；job 里一段都没扣费时成片也不入。示例试看不建 job，不入。
  - 素材 `properties`：`{source:"replicate", jobId, planNo, kind: final|preview|segment, segIndex, rid, preview: bool}`（预览版 `preview=true`）。
  - 入队与写合成结果同一轮：入队失败本轮不写结果，下一轮重做（媒体幂等键，同依据同 rid）再入队；合成已有定论后每轮只补入已扣费段（人工退点 / 补扣后可能变化）。
- 轮询器认领（CAS 令牌）→ 媒体服务 `CopyToLibrary(purpose=asset_library)`（素材库 → 公有桶）→ `pkg/asset.InsertOnceByURL`（`source_task_id` = 任务中心编号，`type=video`，`entry_code=Replicate.Asset.EntryCode`）→ 标记完成并刷新任务中心水位。崩溃重放：复制幂等键固定、写库按地址查重，不会写第二条。认领有效期 20 分钟，大于一次入库的总时限 12 分钟（媒体调用都在这个时限内），写素材库前再续一次认领、确认还是自己的才写，避免认领过期后被另一实例接手、两边各写一条。
- 失败不阻塞 job：暂时错误指数退避（30 秒起、最长 30 分钟），媒体服务 4xx 或超过 8 次放弃（`判据=DELIVERY_GAVE_UP`，后台详情可见）。
- `JobView.library`（当前成片：预览版时报 `preview` 那条，否则报 `final`）/ `segments[].library`：`{status: saved|saving|failed, text}`，文案「已保存到素材库 · 去看」「正在保存到素材库…」「保存到素材库失败，已在重试。你仍可下载」「保存到素材库没有成功，你仍可下载」。
- `POST /jobs/{id}/segments/{k}/save`（保留）：已入队 / 已入库（收尾时自动入的）直接返回已有状态；段没做好 → 409 `SEGMENT_NOT_READY`；出了片但没确认扣费 → 409 `SEGMENT_NOT_PAID`；没开启入库 → 7003。单段任务的成片就是这一段，同地址只入库一条。
- 每条入库完成都刷新任务中心水位（`result_url` 可能因此改变，见 §8.1）。
- 配置：`Replicate.Asset.Enabled`（默认 true，需配 `Replicate.Media`）、`Replicate.Asset.EntryCode`（默认 `video_replicate`，需在 `afv_asset_category_entry` 登记分类，否则落「其他」）。

### 8.3 内容审核（P0-4）

方案确认前审：源视频关键帧（复用分析阶段已抽的帧，`biz_type=VIDEO`）、参考图（`IMAGE`）、叠字文本 + 改法说明（`TEXT`：先过 `pkg/moderation` 关键词库 `afv_copywriting_option.BLOCKWORD`，再过网关）。经 AI 网关 `moderation.image` / `moderation.text`（每类一次批量调用，幂等键 `mod:<方案号>:<类>:<输入 sha256>`）；策略同 Java `InputModerationService`：
`block` 恒违规；`review` 默认按违规（`replicate.moderation.review_as_block=false` 放行）；审核服务故障默认 **fail-close**（`replicate.moderation.fail_open=true` 放行并记 `ERROR_OPEN`）。

- **异步预审**：生成 / 重新报价 / 改标注出方案后立即在后台发起审核（脱离请求上下文；同进程同方案同口径单飞，多实例各自发起时网关幂等键相同、共用一次网关调用），
  方案不等结论（`replicate.moderation.plan_wait_ms` 默认 0；调大则最多等这么久，结论出了就直接带在方案卡上），没结论时 `PlanView.moderation.status=pending`「正在做内容检查，一般几秒内完成」。
- 确认时：已有有效结论（见下条的作废规则）直接用；没有就（复用进行中的或新发起）后台审核并**最多等 `replicate.moderation.confirm_wait_ms`（默认 3 秒）**，
  仍没结论 → 409 `code=4002 reason=MODERATION_PENDING`「内容检查还没完成，几秒后再点确认。（没有扣点数）」：不建 job、不预留点数、退回本次确认限频额度；审核在后台继续，结论落库后再点确认直接用。
- 结论按 `plan_id + plan_hash` 落 `afv_replicate_moderation`（方案变了重审）。已有结论在这些情况下作废重审（成功则覆盖）：策略签名变了（`fail_open` / `review_as_block` / 两个模型组）；确认时遇到 `ERROR_OPEN`（上次是故障放行，确认这一刻必须真审一次）；`REJECTED` 超过 `replicate.moderation.reject_ttl_hours`（默认 24 小时）。网关幂等键 = `mod:<方案号>:<类>:sha256(策略签名|重审轮次|输入)`，结论行记重审轮次 `attempt`，每次作废重审 +1，所以重审一定是新调用。`PlanView.moderation = {status: passed|rejected|pending|skipped, text}`，`rejected` 时前端禁用确认。
- 确认时等到的结论（或已有结论）不通过 → `code=1003 reason=MODERATION_REJECTED`，`msg` 为可读原因（「有参考图没有通过内容检查，请换一张再试。（没有扣点数）」/「原视频的画面没有通过内容检查…」/「填写的文字没有通过内容检查…」，**不含命中词**）；审核服务故障（fail-close）→ `code=6001 reason=MODERATION_UNAVAILABLE`「内容检查暂时不可用，请稍后再试。（没有扣点数）」。两种情况都还没建 job、没预留点数。
- 留痕：每次实际审核（通过 / 未通过 / 故障）写一条 `afv_moderation_log`（provider `gateway` / `keyword`，`request_id = replicate:<方案号>`，`out_task_id` = 网关调用号，`rejected_target` 只写「参考图 #n / 第 n 帧 / 文本摘要」，不落签名地址）；写日志失败只告警。
- 前提：ai-gateway 的 `fusion-go` 调用方需要 `moderation.*` 操作授权与模型组 `moderation.image.default` / `moderation.text.default`；没有时在 fail-close 下所有确认都会失败（上线检查项）。

### 8.4 限频（P0-4）

每用户 分析 / 方案 / 确认 的每分钟与每日上限（`pkg/userquota`，固定窗口，北京时间自然日，计数在会话 Redis；没配 Redis 退回进程内计数并 WARN `判据=RATE_NO_REDIS`）+ 确认时全局在途 job 上限（库里实时数：排队 / 执行中 / 提交中 / 继续中）。
命中：**HTTP 429**，`{"code":4002,"reason":"RATE_LIMITED","msg":"…","data":{"retryAfter":秒}}` + `Retry-After` 头。文案：分钟「操作太快了，稍等一下再试」；日「今天的免费分析次数用完了，明天 0 点恢复。你仍可使用已分析过的视频」/「今天的方案次数用完了，明天 0 点恢复。已有方案仍然可以确认生成」/「今天提交生成的次数用完了，明天 0 点恢复」；全局「现在做同款的人比较多，请稍等几分钟再提交。没有扣点数」（retryAfter 60）。
计次口径：失败（缺项 / 审核没过 / 价变 / 余额不足…）退回本次额度；确认接口先按幂等键 / 已确认方案查已有 job，命中直接返回（不走限频、不计次）；分析命中同一条视频（`cached=true`）不计次；`POST /plans/{no}/quote`、继续、重做不限频。

### 8.5 站内通知（P0-5）

写 Java 通知中心表 `afv_notification`（经 `pkg/notify`，定向 `target_scope=USER`、`notification_type=SYSTEM`），web 现有铃铛直接展示，Go 不注册通知路由（见 `docs/api/api-notify.md`）。三处、同 job 同类只 1 次（去重键 `replicate:<job>:<类>`）：

| 类 | 何时 | 标题 / 正文 |
|---|---|---|
| `replicate.first_ready` | 多段 job 到「待继续」（开了自动继续则继续后不发） | 第 1 段做好了 / 视频做同款：第 1 段做好了，来看看再决定要不要继续 |
| `replicate.done` | 成功 / 部分成功（用户自己点「不继续」不发；到期结束发「已结束」；后台强制收尾发「做好了 X 段，Y 段没做成，没有收费」） | 视频做同款已完成 / N 段全部成功，点击查看；部分：做好了 2 段，1 段没成功，已退回 150 点。点击查看 |
| `replicate.seg_failed` | 某段最终失败（或被后台收尾取消）且**退点已成功**（退点还在处理中不发，补扫退成功后的那一轮再发；退回点数按计费请求号去重，自动重试共用的预留单只算一次） | 第 k 段没生成出来 / 视频做同款：第 k 段这次没生成出来，已退回 N 点。可以重做这一段；全部失败：这次没有生成成功，已全部退回 N 点 |

`link=/video/replicate?job=<id>`、`biz_type=replicate_job`、`biz_id=<job id>`。由轮询器每次推进后按状态推导补发（幂等），通知失败只打日志。用户按类关闭本期不做。

### 8.6 运行参数（`afv_system_config`，60 秒生效；非法值告警并用默认）

| 键 | 默认 | 说明 |
|---|---|---|
| `replicate.moderation.enabled` | true | 方案审核总开关 |
| `replicate.moderation.fail_open` | false | 审核服务故障是否放行 |
| `replicate.moderation.review_as_block` | true | 嫌疑是否按违规 |
| `replicate.moderation.text_model_group` / `image_model_group` | `moderation.text.default` / `moderation.image.default` | 网关审核模型组 |
| `replicate.moderation.reject_ttl_hours` | 24 | 不通过的结论多久后允许重审（1~720） |
| `replicate.moderation.confirm_wait_ms` | 3000 | 确认时等后台预审结论的上限（500~10000 毫秒），等不到回 `MODERATION_PENDING` |
| `replicate.moderation.plan_wait_ms` | 0 | 生成方案时等预审结论（0~5000 毫秒；0 = 不等，立即返回 pending） |
| `replicate.rate.analysis_per_minute` / `_per_day` | 5 / 20 | 每用户分析（0 = 不限） |
| `replicate.rate.plan_per_minute` / `_per_day` | 10 / 30 | 每用户生成方案 |
| `replicate.rate.confirm_per_minute` / `_per_day` | 5 / 20 | 每用户确认 |
| `replicate.rate.max_inflight_jobs` | 200 | 全局在途 job 上限 |
| `replicate.rate.annotate_per_minute` / `_per_day` | 20 / 300 | 每用户改标注 + 再做一条（独立计数，不占方案额度；§10） |

## 9 后台（admin-api `/api/v1/replicate/admin/**`，P0-6）

权限（`Go_V1.0.39`，super_admin 全有、operator 只有 view）：`GET` → `replicate:view`；`POST /jobs/{id}/refund` → `replicate:refund`；`POST /jobs/{id}/force-finish` → `replicate:force`（`adminauth_middleware.replicateRoutePerm`；其它写接口从严按 force）。
配置：admin-api `ReplicateAdmin.Billing` **必须与 gateway-api `Replicate.Billing` 一致（on）**，否则退点 / 收尾回 7003 `BILLING_OFF`（避免把真实冻结的单误判为不计费）。强制收尾用的内核与 gateway 同源读 `afv_system_config` 运行参数（一次都没读到时拒绝收尾：7003「运行参数暂不可用」），装通知器；`ReplicateAdmin.TaskCenterEnabled`（默认 true，须与 gateway 的 `Replicate.TaskCenterEnabled` 一致）时收尾后触发任务中心同步。
幂等键：**只认**请求头 `Idempotency-Key`（后台 CORS 已放行；不回落 `X-Request-Id`）。没带时派生：退点 = 段 + 原因 + 当前预留单号集合；收尾 = 原因 + job 状态 + 当前未结镜像集合。

| 方法 路径 | 请求 | 响应 |
|---|---|---|
| `GET /jobs?userId&status&from&to&jobId&taskId&planNo&page&size` | `status` 为对外码（`succeeded,partial` 可多选）或数字；`from/to` 为 RFC3339 / `2006-01-02 15:04:05` / `2006-01-02`（北京时间，日期形式的 `to` 含当天）；size 默认 20 最大 100 | `{items:[{jobId,taskId,planNo,userId,status,statusLabel,title,modelGroup,segTotal,segDone,segFailed,redoCount,error,stopReason,createdAt,finishedAt}],total,page,size}` |
| `GET /jobs/{id}` | — | `{job, input, result, refundLines（人工退点逐单明细）, segments:[{index,state,gatewayCallIds,failReason,steps,charges,refundablePoints,canRefund}], steps:[{id,stepKey,kind,segIndex,status,attempt,billingRequestId,gatewayCallId,gatewayStatus,errorClass,errorCode,errorMsg,retryOf,createdAt,startedAt,finishedAt}], charges:[{id,segIndex,stage,requestId,seconds,points,mirrorStatus,mirrorLabel,reason,billingStatus,reservationNo,mismatch,refundablePoints,blockedReason,settledAt}], moderations, moderationLogs, deliveries, actions, billingOn, canRefund, canForce, forceBlocker}` |
| `POST /jobs/{id}/refund` | `{segIndexes:[1,2], reason（必填 ≤200 字）, expectedPoints（必填，页面展示的「现在还能退」合计，只算尚未退成的单）}` | `{actionId, points（本动作累计）, thisPoints（这一次请求）, lines:[{id,segIndex,requestId,reservationNo,before,expectedPoints,points,status:PENDING\|DONE\|FAILED,error}], replay, status}` |
| `POST /jobs/{id}/force-finish` | `{reason（必填）}` | `{actionId, replay, jobId, fromStatus, status, canceledSteps, openCharges, message}` |

- 可退点数（每张单）：真实状态 `HELD` 且没挂在未终态步骤上、job 不在提交 / 继续中 → 冻结点数；`CONFIRMED` / `PARTIALLY_REFUNDED` → 实际消费 − 已补退；其余 0。段的 `refundablePoints` 为该段各单之和。
- 退点：`HELD` → `billing.Refund`；已扣除 → `billing.RefundConfirmed`（都幂等）；镜像随后收敛为已退回。
- 逐单审计 `afv_replicate_admin_refund_line`：每张单**动钱前**先落 `PENDING`，结果确认后改 `DONE`（`points` 以计费流水为准）/ `FAILED`；动作行 `afv_replicate_admin_action` 的 `points` = 全部 DONE 明细之和。
- 「本次实际退了多少」：退回原因以本动作前缀 `后台人工退点[a<动作 id>]：` 开头（管理员原因里的 `[a` 片段会被剔除），退后读流水（键前缀 `billing.RefundKeyPrefix` / `billing.PostRefundKeyPrefix` + 单号），描述**以本动作前缀开头**（已确认单为 `人工处理退回：` + 前缀）才计入——两个管理员同时退同一张冻结单，billing 两边都返回成功，但只有真正写入流水的那个动作记点数。冻结单退回成功却找不到退回流水 → ERROR `判据=ADMIN_REFUND_LEDGER_MISSING`，明细记 FAILED（人工核对）。
- 读后状态变化：读到 HELD 后被轮询器确认扣除（`Load` 拿不到 / `Refund` 报已失效）→ 重读真实状态，CONFIRMED / 部分退 改走 `RefundConfirmed`；被回收 / 已退回 → 不动钱、按流水认领。**镜像只在计费真实状态是 REFUNDED / EXPIRED / CANCELLED 时才改已退回**，绝不会出现「镜像已退回、计费已扣除没退」。
- 409 reason：`POINTS_CHANGED`（`expectedPoints` 与现算的「尚未退成的单」合计不一致，不动钱）、`NOTHING_TO_REFUND`、`IN_PROGRESS`（同键正在处理且认领未过期）；同键重放返回第一次的结果（`replay=true`，含全部明细）。
- 部分单退失败 → `code=5001`「有 N 张预留单没退成（这次已退回 X 点，累计 Y 点）。刷新页面后用同一请求重试，只会补退剩下的」，**`data` 同时带逐单明细**（与成功时同形）+ 审计 FAILED；同键重试接管同一个动作：DONE 明细保留，FAILED 明细重置为 PENDING 并按当前状态刷新可退点数 / 退前状态，只退剩下的。
- 处理中崩溃：动作带 `lease_until`（2 分钟，强制大于脱离上下文时限 60 秒 + 30 秒）与认领令牌 `lease_token`，过期后同键可接管（换新令牌）；明细与审计的结果写回都带令牌条件，被接管的旧执行者写入落空。接管时遗留的非 DONE 明细对账：只有真实状态已是退回类终态或流水归属本动作才记完成，否则保持 FAILED 并写明原因（如「这一段还在排队 / 生成中」）。
- 强制收尾：只收非在途（提交中 / 继续中、有段 RUNNING / DELIVERING / HOLDING → 409 `IN_FLIGHT` + 原因）；待继续 → 只交付第 1 段；排队 / 执行中 → 取消没开始的段 → 按步骤状态结清镜像（成功的确认扣除、其余退回）→ 汇总终态；终态但有未结镜像 → 只结清。结清不了的（扣费 / 退点调用失败）在 `openCharges` 如实返回，轮询器继续补扫。收尾后按新状态补发站内通知：被取消的段计入「没做成」（排队被收尾 → 「这次没有生成成功，已全部退回 N 点」）。

## 10 示例 / 我的方案 / 改标注（迁移 `Go_V1.0.41__replicate_samples.sql`）

设计依据：DESIGN §5.18（示例共享只读分析）、§8.2（`PATCH /plans/{no}/annotations`）；EXPERIENCE §2.2（示例 0 点走到看懂 + 预计算示例成片）、§3.4.1（每次编辑 → 新方案、旧方案失效）、§3.9.2（我的方案）。
全部挂 `Auth`，按登录用户隔离（别人的方案 / 保存的方案一律「找不到」，不暴露存在性）。

### 10.1 接口

| 方法 路径 | 作用 | 请求要点 | 响应 | 限频 |
|---|---|---|---|---|
| `GET /samples?industry=` | 示例列表（已启用且已有分析） | `industry` 为行业 code（`afv_template_industry.code`），空 = 全部；非空时返回该行业 + 通用（行业为空）的示例 | `SampleList` | 不计 |
| `POST /samples/{id}/try` | 用示例体验 | — | `SampleTry`：共享分析结果 + 示例原片 + 预计算示例成片（签名地址） | **不计**；不建 job、不预留点数、不写任何用户表 |
| `POST /plans` | 生成方案 | 新增 `sampleId`（与 `analysisNo` 二选一）：用示例的共享分析出**你自己的**方案（参考图是你的素材），正常计费 | `PlanView` | 计「方案」 |
| `GET /plans/{no}` | 取方案 | — | `PlanView`（报价为方案里的快照，要最新价调 `POST /plans/{no}/quote`；不触发新的内容检查） | 不计 |
| `PATCH /plans/{no}/annotations` | 改标注 | `{annotations, refImages?, modelGroup?, resolution?, aspectRatio?}`；`refImages` 不传 = 沿用原方案的图（传了就整体替换），其余不传 = 沿用 | 新的 `PlanView`（新 `planNo` / `planHash` / 报价 / 内容检查） | 计「改标注」（独立计数，不占方案额度；失败 / 重放退回） |
| `POST /plans/{no}/save` | 保存为我的方案 | `{name?}`（≤40 字；不传按「原片概述·改法」生成） | `SavedPlanView`（`created=false` 表示这张方案已经保存过，只更新名字） | 不计 |
| `GET /saved-plans?page=&size=` | 我的方案 | page 从 1 起；size 默认 20、最大 50 | `SavedPlanList` | 不计 |
| `DELETE /saved-plans/{id}` | 删除 | — | `{id}` | 不计 |
| `POST /saved-plans/{id}/reuse` | 用它再做一条 | `{analysisNo?, sampleId?, refImages?, modelGroup?, resolution?, aspectRatio?}`：不传源视频 = 用保存时的原片；`refImages` 不传 = 沿用保存时的图 | 新的 `PlanView`（新 `planNo` / `planHash`，**不复用**旧方案的指纹，同一 `Idempotency-Key` 重放得到同一张；不带键时按「保存 id + 这次的素材 / 源视频 / 模型参数」派生，重复点只出一张） | 计「改标注」（同上） |

「改标注」计数：`afv_system_config` 的 `replicate.rate.annotate_per_minute`（默认 20）/ `replicate.rate.annotate_per_day`（默认 300），0 = 不限；
与「方案」（`plan_*`）分开计，命中同样回 HTTP 429 `RATE_LIMITED`（日文案「今天修改方案的次数用完了，明天 0 点恢复。已有方案仍然可以确认生成」）。

其它 409 reason：`SAVED_PLAN_LIMIT`（我的方案满 50）、`PLAN_CONFIRMING`（在已确认 / 占用中的方案上改标注）；404 reason `SOURCE_GONE`（原片 / 分析不在了）。改标注 / 换图时，原方案里已经解析过的图原样传回（`mr://rid`）直接认，不再要求是上传类别。

改标注的规则：

- 在**同一份分析**上用新标注重新出方案（不调任何模型、不重新分析）；新方案的 `planHash` 带「由哪张方案改来」，所以改回旧标注也是一张新方案。
- 原方案是 `READY`（含已过期）→ 变成 `STALE`（`PlanView.status="STALE"`，`supersededBy` = 新方案号），之后 `POST /plans/{no}/confirm` 回 **409 `PLAN_STALE`**「方案刚更新了，请再看一眼价格」；
  原方案已 `CONFIRMED`（已确认 / 正在确认占用中）→ **409 `PLAN_CONFIRMING`**「这条方案正在生成，生成完可以基于它再做一条」，不出子方案（要再来一条用「保存 → 再做一条」）。
- 已 `STALE` 的方案再改：同样的改动（网络重试）返回当初那张新方案；不同的改动回 409 `PLAN_STALE`（请在最新的方案上改）。
  重放判定先于出方案：按 `superseded_by` / 子方案（`parent_plan_id`）比对规范化后的标注、参考图 rid 序列、模型 / 清晰度 / 画幅，命中直接返回已有子方案（报价取快照、不重审）；
  未命中且原方案已 `STALE` 直接 409——这两种情况都不解析参考图、不报价、不审核（参考图是 http 地址时无法本地比对，按未命中处理）。
- 确认没走完（预留失败 / 提交超时被回收）放回方案时：没有子方案 → `READY`；兜底旧数据 / 竞态里占用期间已有子方案 → 原方案 `STALE`（`superseded_by` = 最新子方案），
  且该原方案下 `READY` 的子方案收敛为只剩最新一张（其余 `STALE`，指向最新那张），不会多张同时可确认。
- 缺项照常 `code=1003` + `data.gaps`，原方案不动。

我的方案的规则：每人最多 **50** 个，满了回 409 `SAVED_PLAN_LIMIT`「方案满了，请删除不用的」（并发保存按用户串行，不会超）；同一张方案只存一份（再存只改名）。
保存的是改法（标注 + 参考图 + 模型 / 清晰度 / 画幅）和原片引用；原片（分析）不在了 → `sourceAvailable=false`，「用它做」不传新视频时回 404 `SOURCE_GONE`「原片已经不在了，需要重新上传原片（方案设置会保留）」。
用新视频再做时，旧改法里新视频没有的项会按缺项返回（`DIM_ABSENT` 等）。

示例下架后：基于示例的方案「改标注」与基于示例的保存「再做一条」（不传新视频时）都要核对示例未删除、未停用、且方案引用的分析行仍是示例当前发布的那一行
（换原片 / 重新分析后旧分析行即失效），否则 404 `SOURCE_GONE`「这个示例已下架，换一个试试」；已确认的 job 输入已冻结，照常执行。

### 10.2 结构

```json
// SampleList
{"items":[{"id":3,"title":"女装上新·模特换装","industry":"womenswear","coverRid":"…","coverUrl":"https://…(签名)",
           "durationS":12.4,"summary":"看懂了：3 个镜头，…","hasResult":true}],
 "urlExpiresAt":"2026-11-14T11:00:00+08:00"}

// SampleTry
{"sample":{…同上一项…},
 "sourceRid":"…","sourceUrl":"https://…","resultRid":"…","resultUrl":"https://…",
 "analysis":{…与 AnalysisView.result 同结构…},
 "annotations":{"dims":{"product":{"mode":"replace","refs":[1]}},"audio":"regenerate"},   // 示例的改法，可能为 null
 "freeText":"这是示例效果，用的是平台自有素材，不花点数；换成你的视频，先看报价再决定",
 "nextText":"换成你的素材，按这个改法做同款",
 "urlExpiresAt":"…"}

// PlanView 新增字段（其余同 §4）
{"status":"READY|CONFIRMED|EXPIRED|STALE", "supersededBy":"VRP…(被哪张新方案取代)", "parentPlanNo":"VRP…(由哪张改来)",
 "sampleId":3, "annotations":{…规范化后的标注…}, "refImages":[{"index":1,"rid":"…"}], "resolution":"720p", "aspectRatio":""}

// SavedPlanView
{"id":5,"name":"白色运动鞋·换商品","planNo":"VRP…","title":"看懂了：…","labels":["换商品","重新配乐"],
 "modelGroup":"vid.seedance-2.5","resolution":"720p","aspectRatio":"",
 "annotations":{…},"refImages":[{"index":1,"rid":"…","url":"https://…"}],
 "sourceThumbRid":"…","sourceThumbUrl":"https://…","sourceAvailable":true,"sampleId":0,
 "useCount":2,"lastUsedAt":"…","createdAt":"…","created":true}

// SavedPlanList
{"items":[SavedPlanView…],"total":12,"page":1,"size":20,"limit":50,"urlExpiresAt":"…"}
```

示例原片 / 成片 / 封面的地址都是现签的读地址（有效期同 `URLTTLSeconds`），只用于页面播放；示例原片不入用户素材库。

### 10.3 示例的分析与媒体权限

- 示例的分析在 `afv_replicate_analysis` 里以 **`user_id=0`（系统）** 跑一次正常解析流水线（同一轮询器推进，不扣点、不计任何用户限频），`source_key = sha256("sample:<id>:<轮次>")`，每次「重新预分析」轮次 +1，是新的一行；
  `afv_replicate_sample.analysis_id` 指向已发布的那一行，`pending_analysis_id` 指向正在跑的那一行，跑成功（读示例时惰性发布）才替换，失败只记 `analysis_error`、不影响已发布的。
  用户拿不到系统分析的编号（`GET /analyses/{no}` 按用户查），只能经 `sampleId` 引用。
- 用示例出的方案 `afv_replicate_plan.analysis_id` = 当时已发布的分析行、`sample_id` = 示例 id：示例之后重新分析 / 下线都不影响已建方案与在途 job（输入已冻结）。
- 出片时示例原片 rid（运营登记的任意合法 rid）只被读取（切段 / 参考视频），产物是用户自己的中间 rid。

## 11 后台：示例管理（admin-api `/api/v1/replicate/admin/samples/**`）

权限：`GET` → `replicate:view`；写（新建 / 修改 / 删除 / 触发预分析）→ handler 内再要求 **`replicate:sample`**（迁移 `Go_V1.0.41` 插入，授 super_admin 与 operator）。
中间件 `replicateRoutePerm` 已对 `/samples/**` 的非 GET 路由级覆盖为 `replicate:sample`（handler 内再查一次同一个码）。

| 方法 路径 | 请求 | 响应 |
|---|---|---|
| `GET /samples?industry=&enabled=&page=&size=` | `enabled` 为 `0` / `1` / 空；size 默认 20 最大 100 | `{items:[SampleAdminRow],total,page,size}` |
| `GET /samples/{id}` | — | `SampleAdminRow` + `analysis`（已发布的分析结果，供预览） |
| `POST /samples` | `{title, industry, sourceRid, coverRid?, resultRid?, annotations?, sortOrder?, enabled?, analyze?}`：`*Rid` 收裸 rid 或 `mr://rid`；`analyze=true` 建完立即预分析 | `SampleAdminRow` |
| `PUT /samples/{id}` | 同上（整体覆盖）；换了 `sourceRid` 会清掉已发布的分析，须重新预分析 | `SampleAdminRow` |
| `DELETE /samples/{id}` | — | `{id}`（逻辑删除；已建方案不受影响） |
| `POST /samples/{id}/analyze` | — | `SampleAdminRow`；上一轮还在跑 → 409 `SAMPLE_ANALYZING` |

校验：`title` 必填 ≤40 字；`industry` ≤64；rid 必须是媒体服务合法 rid 且为**后台上传类别 `asset`**（用户上传类 `-user-`、中间产物 `work`、交付 `deliver` 一律拒收，`msg` 说明原因）；`annotations` 只认十一项 key 与三种改法；启用但还没有分析的示例用户端看不到（`userVisible=false`）。

```json
// SampleAdminRow
{"id":3,"title":"女装上新·模特换装","industry":"womenswear","sourceRid":"…","coverRid":"…","resultRid":"…",
 "annotations":{…}|null,"sortOrder":10,"enabled":true,
 "analysisStatus":"none|queued|running|ready|failed","analysisStatusLabel":"已就绪","analysisStage":"vision",
 "analysisError":"","analysisNo":"VRA…","pendingAnalysisNo":"","round":2,"durationS":12.4,"summary":"看懂了：…",
 "analyzedAt":"…","published":true,"userVisible":true,"createdAt":"…","updatedAt":"…"}
```

`analysisStatus`：有正在跑的一轮 → `queued` / `running`（`analysisStage` 为当前阶段）；否则有已发布分析 → `ready`（上一轮失败时 `analysisError` 仍给出原因）；否则上一轮失败 → `failed`；从没分析过 → `none`。
预分析由 gateway-api 的视频做同款轮询器推进（装配即运行，与总开关无关）。

## 12 模型选择 / 方言 / 写手 / 取消剩余段

### 12.1 接口

| 方法 路径 | 作用 |
|---|---|
| `GET /models?analysisNo=&sampleId=&resolution=&aspectRatio=` | 可选出片模型列表（读接口）。带视频（`analysisNo` 或 `sampleId`）时按分段判断时长并给预计点数 |
| `GET /limits` | 新增 `defaultModel`（推荐模型）、`models[]`（开放的模型组码）、`writerEnabled` |
| `POST /plans`、`PATCH /plans/{no}/annotations`、`POST /saved-plans/{id}/reuse` | `modelGroup` 必须在开放列表里，否则 `1003` + `gaps[{code:MODEL_UNKNOWN}]`；`resolution` 空 = 该模型默认清晰度；清晰度 / 画幅 / 分段时长按模型校验（`RESOLUTION_UNSUPPORTED` / `ASPECT_UNSUPPORTED` / `DURATION_UNSUPPORTED`）。换模型 = 新方案（新 `planHash` / 新报价 / 新方言提示词） |
| `POST /jobs/{id}/cancel-rest` | 不做剩下的段，返回新的 JobView（写接口，带 `Idempotency-Key`） |

`ModelView`：`modelGroup, name, features[], recommended, dialect, resolutions[], aspectRatios[], defaultResolution, minDurationSec, maxDurationSec, resolution（单价按哪档算）, pointsPerSecond（10 秒报价/10）, priceText, estimatedPoints?（带视频时：各段合计）, supportsDuration, supportsAspect, supportsResolution, available, unavailableText?`。报价失败（未配价）的模型 `available=false`，不报错。

内置目录（硬限制取自网关 `video_specs.py`）：

| 模型组 | 名称 | 时长 | 清晰度 | 画幅 | 方言 |
|---|---|---|---|---|---|
| `vid.seedance-2.5` | Seedance 2.5（默认推荐） | 4–30 | 480p/720p/1080p | 6 种 | `seedance-2.5` |
| `vid.seedance-2` | Seedance 2.0 | 4–15 | 480p/720p/1080p | 6 种 | `seedance-2`（fast / mini 同族复用） |
| `vid.minimax-h3` | MiniMax H3 | 4–15 | 480p/**768p**/1080p | 6 种 | `minimax-h3` |
| `vid.wan-3.0` | 万相 3.0 | 2–30 | 480p/720p/1080p | 无 21:9 | `wan-3.0` |

运行参数：`replicate.models`（逗号分隔，空 = 全部）、`replicate.default_model`、`replicate.writer.enabled`（默认开）、`replicate_writer`（写手模型组，空 = 用 `replicate_vision`）。yaml：`Replicate.Models / DefaultModel / WriterEnabled / WriterModelGroup`。

### 12.2 方言

段提示词按所选模型组选方言（`PlanView.dialect`），未知模型回落基础模板（`base`）。模板内嵌在 `app/replicate/core/prompts/dialects/*.tmpl`，版本进 `plan_hash`。
素材标签用各家原生写法（网关不改写）：Seedance `@视频1/@图片n`；MiniMax H3 `<Video 1>/<Picture n>`（英文分节：subject_definitions / summary / retention_analysis / detailed_description / overall_soundscape / negative）；万相 `视频1/图n`（总体描述 + 参考素材引用 + 分镜N + 风格 + 负向清单）。

### 12.3 写手

开了写手的 job，确认时每段多一个步骤 `seg/NN/write`（`kind=writer`，幂等键 `{前缀}rpl-{job}:seg/NN/write`，重写第 n 次追加 `#a<n>`）：调网关 `chat.completion`，输入为确认时冻结的系统提示词 + 本段事实（分析、用户改动、方言要求、模板稿）。产出经确定性校验（标签、图号、长度、风控词）；不合格回灌问题重写（默认最多 2 次），仍不合格 / 网关失败 / 超时（3 分钟）→ 回落模板稿，出片照常。该段的出片（含自动重试、用户重做）都用写好的那一稿。写手**不收费**：报价只含出片秒数，写手成本平台承担（`limits.max_cost` 封顶）。未放行的段（先做第 1 段时的其余段）在继续后才写。

### 12.4 取消剩余段

- 只取消还没开始出片的段（排队中 / 等自动重试 / 已预留未开始的重做），在途段做完并正常扣点；已完成段照常入库（成片不为取消的段占位）。
- 取消段的预留全部退回（退回失败进补扫）；段 `state=canceled`、`stateLabel=未做`。
- 全部段都是被取消的 → job `canceled`；有成功段 → `partial`。
- 幂等：没有可取消的段（重复点击 / 已结束）直接返回当前 JobView；`waiting` 时等同「不继续了」（`/stop`）；提交中 / 继续中回 409 `IN_PROGRESS`。
- 二次确认用 `JobView.cancelRest{available, segments, points, running, text}`（排队中 / 生成中才有）。

## 13 ETA 历史校准 / 分析纠偏 / 发送前二次确认

不新增迁移：ETA 由查询现算，两个运行参数是 `afv_system_config` 的普通键（没有行 = 默认）。

### 13.1 ETA 历史校准（`core/eta.go`、`repo/eta.go`）

- **样本**：近 30 天完成的**成功** job（`status=2`）里每一段「第一次开始生成 → 成功出片」的耗时（`afv_replicate_step` 出片步骤 `kind=gateway`，
  含自动重试 `#rN`，不含用户重做 `#uN`；原链没成功、靠重做才成功的段不计）；生成秒数取该段非重做预留镜像的 `seconds`。最多取最近 5000 段。
  一段的耗时就是它所在那一轮的耗时（排队等空位、待继续的等待都不算在内，排队由轮数 `R` 体现）。
- **分桶**：模型组（`job.input.modelGroup`）× 段数桶 `1 / 2 / 3 / 4+` × 单段生成时长桶 `0-5 / 6-10 / 11-15 / 16+` 秒（方案按各段最长的那段取桶）。
  每桶算 P50 / P90（最近秩法）。
- **取用**：精确桶样本 ≥ 20 → 用精确桶；否则用同模型组同时长桶、不分段数的粗桶（≥ 20）；仍不足 → 先验（`Config.EtaRoundP50/P90`），`prior=true`。
  整张方案用同一个桶，`first / rest / total` 要么全部来自历史、要么全部是先验。
- **刷新**：gateway-api 进程内缓存，10 分钟后由下一次查询在后台异步重算（单飞，查库时限 10 秒；请求从不等库）；重算失败沿用上一次统计并 ERROR `判据=ETA_STATS_FAILED`。
  进程刚启动、第一次重算完成前回落先验。

### 13.2 分析纠偏规则（`rules/analysisfix`）

**读取时套用，不重跑分析。** 分析完成时只落未纠偏的规范化结果（`afv_replicate_analysis.result`）与画面理解原文（`state.visionContent`）；
返回用户端分析 / 生成方案（含改标注、我的方案、做同款示例）/ 后台示例详情时，用**当前**规则配置对原文重新规范化（`core.ResolveAnalysisWith`，
纠偏在夹紧时间之前，时长类规则要看到模型给的原始时间），按规则表顺序应用。读出的结果带
`corrections[] = {rule, stage, field, count?, before?, after?, note}` 与 `rulesVersion`（读取时现算，不落库）：
后台示例详情（`GET /samples/{id}` 的 `analysis`）可见，**用户视图（`GET /analyses/{no}`、`POST /samples/{id}/try`）去掉 `corrections`**（保留纠偏后的结果与 `rulesVersion`）。
没有原文的分析行（直接写入结果的数据）原样返回落库结果。分析完成时按当前规则预演一次，有纠偏打日志 `判据=ANALYSIS_CORRECTED`。

| 规则码 | 阶段 | 判据 → 纠偏 |
|---|---|---|
| `TIME_SWAPPED` | understand | 镜头 / 台词 / 其它时段（服装出现、画面文字）`t1 < t0` → 交换 |
| `DURATION_SCALE` | understand | 镜头最晚结束时间与原片时长误差同时超过 `durationTolerance`（相对，默认 15%）与 `durationMinDriftS`（默认 1 秒）：偏短 → 全部画面理解时间等比放大到原片时长；偏长且**有镜头整段落在原片之外**（开始 ≥ 时长）→ 等比缩小（只是末尾写超的交给夹紧）。台词取自转写时不动台词 |
| `SHOT_NO_KEYFRAME` | shots | 至少 2 张关键帧、2 个镜头：某镜头时段内（边界容差 `frameSlackS` 默认 0.05 秒）一张关键帧都没有 → 并入前一个镜头（第一个并入后一个） |
| `SHOT_OVER_FRAMES` | shots | 镜头数 > 关键帧数 → 从最短的镜头开始合并到不多于关键帧数 |
| `LINE_DUPLICATE` | lines | 相邻两句文字相同且间隔 ≤ `lineJoinGapS`（默认 0.3 秒）→ 合并 |
| `SPEECH_NO_LINES` | lines | `audioKind` 为 speech / mixed，转写跑过却一句台词也没有 → 改为 music（无音轨为 silent），台词项置无 |
| `LINE_PRESENT_EMPTY` | lines | 台词项标了有，但没有任何台词也没有描述 → 台词项置无 |
| `LINES_NO_SPEECH_MARK` | lines | 有音轨、转写出了台词，却标成 silent / music → silent→speech、music→mixed |

- **配置**：`afv_system_config.replicate.analysis_rules`（JSON，60 秒生效；非法 JSON / 未知规则码整条忽略并告警，用默认）：
  `{"disabled":["LINE_DUPLICATE"],"durationTolerance":0.15,"durationMinDriftS":1,"frameSlackS":0.05,"lineJoinGapS":0.3}`；阈值越界回默认。
- **版本与缓存键**：规则版本 = `analysisfix.Version`（当前 `fix-2026-11-14.1`，改规则必须升）；非默认配置追加 `+<配置指纹 8 位>`，写进读出结果的 `rulesVersion`。
  分析缓存键 `source_key = sha256(去 query 的源地址)`，**不含规则版本**：改规则或配置不让已有分析失效、不重新分析、不调网关，下一次读取即按新规则套用。
  已生成的方案 / 已确认的 job 用的是生成时读出的结果（分段、提示词已固化进方案与 `job.input`），改规则不回溯；之后新生成 / 改标注的方案按新规则。
  示例的预分析键按「示例 + 轮次」。

### 13.3 发送前二次确认（后端部分）

- 配置：`afv_system_config.replicate.confirm_threshold_points`（点数，整数 0~100000000，**默认 0 = 不启用**；非法值告警并按 0）。
- 启用后确认（`POST /plans/{no}/confirm`）时：方案总价（全部段合计，与这次选「先做第 1 段」还是「全部做」无关）**超过**阈值，
  则 `expectedPoints` 必须等于本次现算的预留点数（先做第 1 段 = 第 1 段价；全部做 = 合计），缺失或不一致 → 409 `code=4002 reason=QUOTE_CHANGED`（与未超阈值时的价变同一个码），
  `msg`「点数有变化：A → B 点，请再确认一次」/「这次要预留 B 点，请确认点数后再提交」，不建 job、不预留、退回本次确认限频额度；日志 `判据=CONFIRM_THRESHOLD`（带方案总价 / 阈值 / 本次 / 页面值）。
- 未超过阈值（或阈值 0）：沿用原口径——带了 `expectedPoints` 且不一致回 `QUOTE_CHANGED`。HTTP 接口本来就要求 `expectedPoints` 必填（缺失 1002），阈值对 HTTP 调用方的差别只在 `msg` 文案（reason 都是 `QUOTE_CHANGED`）；对直接调内核的调用方，超阈值时 `expectedPoints` 变为必填。
- 继续 / 重做不受阈值影响（仍按各自的 `expectedPoints` 比对，不一致 `QUOTE_CHANGED`）。
