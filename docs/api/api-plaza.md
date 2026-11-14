# 广场模板域接口（行业 / 模板）

设计：fusion-aigc 仓库 `.scratch/agent-template-industry/DESIGN.md`。迁移：`Go_V1.0.33`（表结构）、`Go_V1.0.34`（种子与回填）、`Go_V1.0.36`（后台权限码）、`Go_V1.0.35`（头条）、`Go_V1.0.43`（收藏 / 我的模板 / 营销节点）。
入参 schema 契约见 `plaza-slot-schema-v1.md`。

## 用户端 `/api/v1/plaza/*`（gateway-api，信封 `{code,reason?,msg,data,requestId,timestamp}`，字段 camelCase，不经 jsoncase）

严格垂直：除目录、偏好、行业建议、按 id 取单条 / 做同款准备外，**必须带 `industry`**，缺（或只给 `subIndustry`）→ HTTP 400 `PARAM_MISSING`「请先选择行业」；
`industry` 不存在 / 停用 / 为 `ALL` / `subIndustry` 不属于它 → 业务码 4001 `reason=INDUSTRY_NOT_FOUND`；参数格式错 → HTTP 400 `PARAM_INVALID`；限流 → HTTP 429 `reason=TOO_FREQUENT`。

| 接口 | 说明 |
|---|---|
| `GET /industries` | 行业目录（无数量 / 模板）：`gateEnabled`、`industries[{id,code,name,iconUrl,hint,audienceName,examples,nextActions,children[{id,code,name,hint}]}]`；`Cache-Control: public, max-age=60` |
| `GET /counts?industry=&subIndustry=` | `{scope,image,video,newCount7d}` |
| `GET /templates?industry=&subIndustry=&mediaType=&keyword=&page=&size=` | 列表与搜索（仅所选行业内；`size` 默认 24、上限 100）；分页 `{current,pages,total,data[]}`；项字段见下；**不含 slots** |
| `GET /templates/{id}` | 按 id 取单条（深链还原，不带行业）；**含 `slots`**；不可见 → 4001「这个模板刚刚下架了，看看相似的吧」 |
| `POST /templates/{id}/prepare` | 做同款准备：`{"inputs":{…}}` → 按 schema 校验并规整 `{"inputs":{…}}` |
| `GET /samples?industry=&subIndustry=&templateId=` | 示例素材：模板级 → 所选行业（及其一级），≤6 条，不取其他行业 |
| `GET /quote?industry=&subIndustry=&templateId=&variants=&refImageCount=&resolution=` | 匿名标价（Go → Java 只读 `image-quote`）；带 `Authorization` 时转发取个性化报价（含 `turnPriceUnavailable`：Java 没读到轮次费，前端写「轮次费以实际为准」）。`refImageCount` 可选 0~50：请求里实际填了几张参考图，缺省退回 `needsProductImage ? 1 : 0`；`resolution` 可选 `1k`/`2k`/`4k`（缺省 `1k`，不分大小写，非法 400 `PARAM_INVALID`），按所选档位向 Java `image-quote` 报价，匿名缓存键含档位；模板自带的分辨率配置不读（待 Java 受理侧支持后再接入）。响应含 `mediaType`、`maxVariants`（图 4）。**视频模板不报价**：返回 `QUOTE_UNAVAILABLE`「视频模板在生成视频页报价」（视频不经智能体对话出片）。Java 不可用沿用 1 小时内缓存（`stale=true`），再久 `QUOTE_UNAVAILABLE`「登录后查看报价」；`etaSeconds` 样本 <30 为 null |
| `GET /recent?industry=&subIndustry=&mediaType=&limit=` | 「接着做」（需登录，严格同行业，默认 12） |
| `GET/PUT /industry-preference` | 需登录。PUT `{industryIds,primaryIndustryId}` 整体替换（1~3 个，可含二级） |
| `POST /industry-suggestions` | 匿名可提交 `{text?,sid}`：清洗、疑似手机号 / 邮箱静默丢弃、同 sid 同文本同日去重、每 sid / IP 每天 ≤3 条（`SUGGESTION_RATE_LIMITED`） |
| `POST/DELETE /favorites/{templateId}` | 收藏 / 取消收藏（需登录，幂等）。POST 要求模板此刻对用户可见（否则 4001「这个模板刚刚下架了…」），每人上限 500，满了且是新收藏 → 4002 `reason=FAVORITE_LIMIT`「收藏已满，取消一些再收藏」；DELETE 不要求模板仍可见。响应 `{templateId,favorited}` |
| `GET /favorites?industry=&subIndustry=` | 当前用户在所选行业里的收藏（需登录；**industry 必填**）：只含「属于所选行业 + 此刻可见」的模板，收藏时间倒序；模板下线 / 移出行业后不显示但**不删记录**（重新上架自然回来）。`{items[模板项 + favoritedAt],total,limit}`；`Cache-Control: private, no-store` |
| `POST /my-templates` | 存为我的模板（需登录）：`{name(≤40字),templateId,inputs,variants(1~4，缺省 1),resolution(1k/2k/4k，缺省 1k，非法 400 `PARAM_INVALID`),text(≤500字,可空),industry?,subIndustry?}`。`inputs` 按模板**当前** slots schema 校验（复用 `ValidateInputs`，不合格 → 1003 `PARAM_INVALID`，data 带 `field/errors`）；`industry` 记录创建时所选行业（给了必须是模板所属范围，缺省取模板自己的第一个有效行业）；每人上限 100，满了 → 4002 `reason=MY_TEMPLATE_LIMIT`。响应 = 单条我的模板（见下） |
| `GET /my-templates?industry=&subIndustry=` | 当前用户在所选行业里的我的模板（需登录；**industry 必填**，创建时间倒序）：「记录的行业属于所选范围，或模板自己属于所选范围」才算当前行业的。`{items,total,limit}` |
| `PUT /my-templates/{id}` | 改名称 / 入参 / 版数 / 文字（给了才改；`inputs` 重新按当前 schema 校验，模板已下线则拒）。只能改自己的，别人的 / 不存在 → 4001 |
| `DELETE /my-templates/{id}` | 删除（幂等，只能删自己的）。`{deleted:true}` |
| `GET /nodes?industry=&subIndustry=` | 营销节点推荐条（公开，**industry 必填**，`Cache-Control: public, max-age=60`）：节点前 `lead_days`（默认 14）天起至节点当天出现，日期升序、最多 3 个；每个节点只带「属于所选行业 + 此刻可见」的节点模板（≤12，`templateCount` 为总数），该行业没有可用模板的节点不出现。`{items[{code,name,why,eventDate,daysLeft,templateCount,templates[模板项]}]}`；仅内容推荐，不含任何优惠。「同时只展示 1 个、关闭后 7 天不再出现」是前端按账号记在本地的规则，服务端不记 |
| `POST /overlays` | 模板文字叠加（需登录，**叠字不收费**、不走报价）：`{templateId,sourceUrl,values{slotKey:文字}}`。模板须 `textOverlay=true`，只叠 slots 里 `type=text` 且带合法 `layout` 的位（别的 key 忽略，空值不叠，至少一处有值，超 `maxLength` 拒）；`sourceUrl` 必须是该用户自己的成品图（素材库行，或创作任务结果 / 水印结果），否则 4001。流程：媒体服务摄入无字图 → `image/text`（layout 相对值按图高换算像素，缺字体用 `Plaza.Overlay.DefaultFontRid`）→ 转存素材库桶 → 登记素材（无字版 `properties.textVariant=plain`，叠字版新增一行 `textVariant=overlay`+`plainUrl`）。同一（用户,模板,成品图）已有成功记录且文字没变直接返回。**叠字失败不抛错**：HTTP 200 + `status:"failed"`（记录已存、可重试），原图照常交付。响应 `{id,templateId,sourceUrl,renderedUrl|null,status:"ok"\|"failed",failReason?,values,updatedAt}`；媒体服务没配 → 7003 `PLAZA_OVERLAY_NOT_READY`；限频 → 4002 `PLAZA_OVERLAY_TOO_FREQUENT` |
| `PUT /overlays/{id}` | 改字 / 重试：`{values?}`，不带 values = 按已存文字重试；总是从无字原图重渲染（非破坏性），只能改自己的 |
| `GET /overlays/{id}` · `GET /overlays?sourceUrl=a&sourceUrl=b` | 取单条 / 按成品图批量取（刷新后恢复叠字结果，≤20 个地址）；`Cache-Control: private, no-store` |
| `POST /overlay-intents` | 自动叠字意图（需登录，不收费）：`{requestId,templateId,enabled?(缺省 true),values{slotKey:文字}}`，做同款**发智能体流之前**调用，`requestId` = 本轮 `X-Request-Id`（8~60 位 `[A-Za-z0-9_-]`）。`enabled=true` 时按模板当前 slots 校验（同 `POST /overlays`：须 `textOverlay=true`、只留带 `layout` 的文字位、至少一处有值、超长拒），把文字值 + 文字位版式冻结；`enabled=false` = 明确「本次不加字」（不校验、不存文字）。同一（用户,requestId）先到先得，重发原样返回已冻结的那条。响应 `{id,requestId,templateId,enabled,values,createdAt}`；媒体服务没配 → 7003 `PLAZA_OVERLAY_NOT_READY`；自动叠字关闭 → 7003 `PLAZA_OVERLAY_AUTO_OFF`（前端退回逐张 `POST /overlays`）；限频 → 4002 `PLAZA_OVERLAY_TOO_FREQUENT`。规则见下「自动叠字」 |

### 自动叠字（服务端触发，不依赖页面开着）

迁移 `Go_V1.0.49__plaza_overlay_auto.sql`（`afv_plaza_overlay_intent` 意图、`afv_plaza_overlay_job` 作业，Go 独写）。代码 `app/plaza/core/overlay_auto.go`，后台循环只在 gateway-api 跑（`Plaza.Overlay.Auto.Enabled` 且媒体服务已配时启动，每 10 秒一轮，多副本靠条件更新 / 行租约互斥）。

- **触发条件**：本轮登记了 `enabled=true` 的意图（前端「本次给图片加字」开关默认开）+ 模板在登记时 `textOverlay=true` + 产物已入库。是否叠字、文字值、文字位版式都以登记时冻结的为准，**不读用户当前设置**；提交后模板改版式 / 下架不影响这一轮。
- **找产物**：Java 智能体工具产物的创作任务 `request_params.source.runId = "run-" + requestId`（含分组父任务），加上会话内对失败图的重试任务（`request_params.retryOf` 指回原任务，一跳）；再取这些任务入库的素材行（`afv_asset.source_task_id`，`entry_code ∈ Plaza.Overlay.Auto.EntryCodes`，缺省 `agentImage`；排除视频与叠字版自身）。每张图叠用户看到的那张：有水印图（免费档）叠水印图，否则原图；不是 http(s) 地址的跳过。每张图一条作业（唯一键 用户+模板+成品图）。Java 轮次（`afv_agent_turn.run_id`）的 `plaza_template_id` 与意图模板不一致 → 意图作废（判据 `TURN_TEMPLATE_MISMATCH`）。意图登记后 `MatchWindowMinutes`（缺省 1440）内持续扫描：前 30 分钟每 15 秒、3 小时内每分钟、之后每 5 分钟。
- **执行**：复用手动叠字的渲染与登记（媒体服务 `image/text` → 转存素材库桶 → `pkg/asset.SaveTextOverlayVariant` 登记叠字版）；不收费、不占用户叠字配额、不再验所有权（来源就是该用户自己的素材行）。作业开始时该图已有成功叠加记录（前端已叠 / 用户已改字）→ 不重渲染、不覆盖，只补素材登记。
- **重试**：每张图最多 `MaxAttempts`（缺省 5）次，第 n 次失败后退避 30 秒 / 2 分钟 / 10 分钟 / 30 分钟（之后都按 30 分钟）。中间失败**不落** failed 记录（前端此时查不到记录 = 还在处理）；最后一次失败、永久失败（意图快照无效、没有可叠的文字、模板与系统都没配字体）或进程反复中断用完次数，落 `status=failed` 叠加记录，前端照旧显示「文字没加上，可重试」，用 `PUT /overlays/{id}` 手动重试 / 改文字。叠字成功但叠字版没登记进素材库（无字版素材行还没同步）→ 退避再补，次数用完按完成收尾，之后读取时照旧补登记。
- **与前端并存**：前端仍可在页面开着时逐张 `POST /overlays`（幂等，同一张图只有一条叠加记录），刷新后 `GET /overlays?sourceUrl=` 恢复；自动叠字只是保证页面关了也会叠上。
- **配置**：`Plaza.Overlay.Auto: { Enabled: true, MaxAttempts: 5, MatchWindowMinutes: 1440, Batch: 10, EntryCodes: [agentImage] }`（gateway-api）。
- **已知边界**：只覆盖智能体做同款（Java `/api/v1/agent/stream` 带 `plazaTemplateId` 的那一轮）；Go 的 imagegen / studio / canvas 出图请求不携带广场模板，不在自动叠字范围。表单续答轮次的 runId 不由 requestId 派生，其产物不自动叠字（前端手动叠字照旧可用）。

模板项：`plazaTemplateId,title,agentSkillId(字符串),tags,payload(摘除 prompt 与元数据键,键序原样),authorName,usageCount(null 当 <20),usageVisible,mediaType,videoUrl,posterUrl,coverThumbUrl,isNew,createdAt,needsProductImage,textOverlay,targetChannels[{code,name,width,height,ratio}],industries[{id,code,name,parentCode}]`。
`keyword` 在所选行业首页 0 结果时，服务端记一次行业内搜索缺口（只记清洗后的词；疑似手机号 / 邮箱 / 纯数字丢弃；不含任何用户标识）。

## 后台 `/api/v1/plaza/admin/*`（admin-api，默认信封，权限 `creaft_catalog:view/edit`；`plaza:publish/delete/audit` 路由级覆盖）

行业：`GET/POST /industries`、`PUT /industries/sort`、`PUT/DELETE /industries/{id}`（`?reassignTo=`）、`GET /industries/{id}/templates`、`PUT /industries/{id}/templates/order`。
模板：`GET/POST /templates`、`GET /templates/trash`、`GET/PUT/DELETE /templates/{id}`、`PUT /templates/{id}/slots`（只改入参，见 `plaza-slot-schema-v1.md`）、`PUT /templates/{id}/industries`（增删行业）、`PUT /templates/{id}/attrs`（渠道 / 媒体类型等）、`GET /templates/unassigned-summary`（未分配行业模板计数）、`POST /templates/{id}/duplicate|publish|offline|restore`、`DELETE /templates/{id}/purge`、`POST /templates/batch/{publish|offline|delete|restore|assign|unassign|replaceIndustries|setOrder}`、`GET /templates/{id}/audit`、`GET /audit`、`GET /preview/templates`。
渠道 `GET/POST /channels`、`PUT /channels/{id}`；示例素材 `GET/POST /samples`、`PUT/DELETE /samples/{id}`；
行业建议 `GET /industry-suggestions`、`POST /industry-suggestions/{normText}/adopt|ignore`；搜索缺口 `GET /search-misses`、`POST /search-misses/status`。
营销节点 `GET/POST /nodes`、`GET/PUT/DELETE /nodes/{id}`、`PUT /nodes/{id}/enabled`；节点 × 模板（按行业）`GET/POST /nodes/{id}/templates`、`PUT /nodes/{id}/templates/order`、`DELETE /nodes/{id}/templates/{templateId}`，见下「营销节点后台」。

整体编辑 `PUT /templates/{id}` 的「未带字段」语义：`title` 必填；`description` / `category` / `tags` / `mediaType`（回 image）/ `videoUrl` / `posterUrl` / `channels` **缺省 = 清空**（整体替换，同 Java）；
以下**缺省 = 不改**：`payload`（保留原载荷）、`slots`（保留原入参；显式 null / `[]` / `items:[]` 才清空）、`industryIds`（`[]` = 清空关系）、`agentSkillId`（`""` = 解绑）、
`needsProductImage` / `textOverlay` / `sortOrder`；换了 `videoUrl`（或图片改视频）时 `mediaStatus` 进入 `PROCESSING`，见「视频处理流水线」。

乐观并发（整体 `PUT /templates/{id}` 与 `PUT /templates/{id}/slots` 相同）：
- `updatedAt` **必填**（读到的 `updateTime`），缺 → HTTP 400 `PARAM_MISSING`；`revision` 可选（详情 / 列表行下发的 `revision`）。
- 请求带 `revision` 时只按它比对（精确到每次写）；不带则比 `updatedAt`（秒级，老前端只传它也能用）。
- 服务端每次写模板 `revision + 1`，并以 `UPDATE … WHERE id = ? AND revision = ?` 的影响行数兜底判冲突（同一秒内的并发保存也比得出来）。
- 不一致 → `4002 · TEMPLATE_CONFLICT`（只改入参接口是 `4002 · CONFLICT`）。
- 成功返回新的 `{id, updatedAt, revision}`（只改入参接口另带 `slots`），下次保存带上。
入参 schema 校验失败统一 `1003 · SLOTS_INVALID`，`data.field` 为路径（`items[n].field`），`data.errors` 为全部错误。

后台模板列表行 `headlineBrief`：`{onBoard, version, scopes[{scope,name,rank,source}], activeRules, suppressed}`（最新榜单版本；头条没配库为 null）；
详情 `headlineStatus`：同 `GET /headlines/templates/{id}/headline-status`（模板不在目录——草稿 / 回收站——或读取失败为 null）。
彻底删除模板时同一事务里逻辑删除它的头条规则、物理删除抑制（历史榜单条目保留）。

模板写接口受配置 `Plaza.AdminWriteEnabled`（默认 false，交接前 Java 是写者）保护，关闭时返回 `ADMIN_WRITE_DISABLED`；行业 / 渠道 / 示例素材 / 建议 / 缺口（Go 独写表）不受影响。
用户端可见性口径随写权属变：`AdminWriteEnabled=false`（Java 是写者，只维护 `enabled`）时，目录 / 列表 / 按 id 取 / 头条读模板都**同时要求 `enabled=1`**；
为 true（Go 是写者）时按 `state=PUBLISHED` + `[online_at, offline_at)` 窗口判断。按 id 取单条（详情 / 做同款准备 / 报价）另要求模板至少挂在一个有效（启用、父级也启用）的行业下，否则按不存在。
边界任务（每分钟，仅 `AdminWriteEnabled=true` 时跑）：下翻不限（`enabled=1` 但已不可见 → 0）；上翻只针对定时上线行（`online_at IS NOT NULL`）——Go 立即发布时已在同一条 UPDATE 里写了 `enabled=1`。

### 写权切换步骤（Java → Go）

1. 对账：Java 写权属期间停用的模板只改了 `enabled=0`、`state` 仍是 `PUBLISHED`。切换前把它们改成 `OFFLINE`，否则 Go 按 state 口径会把它们重新放出来：
   ```sql
   UPDATE afv_compat_plaza_template SET state='OFFLINE' WHERE deleted=0 AND enabled=0 AND state='PUBLISHED';
   ```
2. **先关 Java**：`app.plaza.legacy-admin-write-enabled=false`，发布并确认 Java 后台模板写接口已拒绝。
3. **再开 Go**：gateway-api 与 admin-api 的 `Plaza.AdminWriteEnabled=true`，重启。两边永不同时为 true。
4. 验证：后台编辑一个模板（返回带 `revision`），用户端列表与 Java 老 v1 列表 / Agent 快照一致；定时上线到点后 1 分钟内 `enabled` 翻成 1。
5. 回滚：反序（先关 Go，再开 Java）；对账 SQL 无需回滚（`OFFLINE` + `enabled=0` 在两种口径下都不可见）。
用户侧行业功能开关 `Plaza.Industry.Enabled`（默认 false）：关闭时目录 `gateEnabled=false` 且 `industries=[]`。

### 交接期可写范围（`AdminWriteEnabled=false`，功能上线与写权切换解耦）

原则：用户端只展示挂了行业的模板，所以「挂行业」不能等到整体切换写权。Go 后台在交接期只写 **A 类（Go 独占，Java 从不读写）**，**B 类（Java 也写）仍拒**（`ADMIN_WRITE_DISABLED`）。
A 类写模板行时不碰 B 类列，**包括 `update_time`**：该列 DDL 带 `ON UPDATE CURRENT_TIMESTAMP`，交接期它归 Java（Java 管理端列表展示它、MyBatis-Plus 写时填充它），
所以交接期对模板行的 A 类写显式 `SET update_time = update_time` 抑制自动更新，并发判据只用 `revision`（Java 不写 `revision`，A 类各列也只有 Go 写，不会与 Java 互相覆盖）。完整模式（`true`）下行为不变（`update_time` 照常推进）。

**B 类（Java 也写；依据 `AdminCreaftPlazaTemplateController` 的 insert / `LambdaUpdateWrapper` / `deleteById`，实体 `CompatPlazaTemplate` + `BaseEntity`）**：
`title`、`category`、`description`、`tags_json`、`data_json`（含 `agentSkillId` / `v1PlazaTemplateId`）、`sort_order`、`enabled`、`deleted`、`create_time`、`update_time`。
另有三组由 `enabled` / `deleted` 派生、Java 不写但必须与它们同步的 Go 列，交接期同样不写：`state` / `online_at` / `offline_at`（上下线与定时）、`deleted_at` / `deleted_by`（回收站）、`created_by`（随新建）。
Java 新建 / 编辑 / 停用 / 删除只落 B 类列，Go 列取默认值（`state=PUBLISHED`、`media_type=image`、`revision=0`……）——这正是切换前要跑对账 SQL 的原因。

**A 类（Go 独占）**：
- 表：`afv_plaza_template_industry`（行业关系与行业内 `sort_order`）、`afv_template_industry`（行业）、`afv_plaza_channel`、`afv_plaza_sample_asset`（示例素材）、`afv_plaza_audit_log`、建议 / 缺口表。
- `afv_compat_plaza_template` 的 Go_V1.0.33 新增列中归 A 类的：`slots_json`、`channels_json`、`media_type`、`video_url`、`poster_url`、`needs_product_image`、`text_overlay`、`media_status`（换预览视频时重置）、`updated_by`、`revision`（每次 A 类写 +1）。

| 接口 | 类 | 交接期 | 完整模式 | 备注 |
|---|---|---|---|---|
| `GET /capabilities` | 读 | 放行 | 放行 | `{adminWriteEnabled, writable{status, recycle, fullEdit, slots, industryRelation, industryOrder, attrs, samples}}`，交接期 `status` / `recycle` / `fullEdit` 为 false，其余为 true；完整模式全 true；前端据此置灰按钮 |
| `GET` 全部读接口（列表 / 详情 / 回收站 / 预览 / 审计 / `unassigned-summary`） | 读 | 放行 | 放行 | 列表 `?unassigned=true` 为未分配清单，响应带 `unassignedCount`；`GET /templates/unassigned-summary` → `{total, enabled, adminWriteEnabled}`，`enabled` = 已被 Java 上线、只差挂行业就对用户可见的那批 |
| `POST/PUT/DELETE /industries*`、`PUT /industries/sort` | A | 放行 | 放行 | 行业表 Go 独写 |
| `PUT /industries/{id}/templates/order` | A | 放行 | 放行 | 只改关系行 `sort_order`，不碰模板行 |
| `PUT /templates/{id}/industries` | A | 放行 | 放行 | 新。`{assign?,unassign?}` 增量幂等（`revision` 可选）；`{industryIds, revision}` 整体替换（`revision` 必填）。有变化才 `revision+1` 并记审计 `TEMPLATE_SET_INDUSTRIES`；返回 `{id, industryIds, updatedAt, revision, changed}` |
| `POST /templates/batch/{assign\|unassign\|replaceIndustries\|setOrder}` | A | 放行 | 放行 | 只写关系表，模板行不动、不 bump `revision`；逐条审计，共用 `batchId` |
| `PUT /templates/{id}/slots` | A | 放行（`revision` 必填，`updatedAt` 可省） | 放行（`updatedAt` 必填，`revision` 可选） | 交接期 `update_time` 冻结，所以只按 `revision`；缺 → 400 `PARAM_MISSING`，旧值 → `4002 · CONFLICT` |
| `PUT /templates/{id}/attrs` | A | 放行（`revision` 必填） | 放行（`revision` 优先；只带 `updatedAt` 也可） | 新。`{revision, updatedAt?, mediaType?, videoUrl?, posterUrl?, channels?, needsProductImage?, textOverlay?}`，缺省 = 不改，`channels` 显式 null / `[]` = 清空；改 `image` 清 `videoUrl`，改 `video` 须有 `videoUrl`；渠道须存在且启用；返回 `{id, updatedAt, revision}`，审计 `TEMPLATE_UPDATE_ATTRS` |
| `GET/POST /channels`、`PUT /channels/{id}` | A | 放行 | 放行 | 渠道字典 |
| `POST /samples`、`PUT/DELETE /samples/{id}` | A | 放行 | 放行 | 每范围最多 6 条（不分 role）；Go 独写表，无 `revision`（逐行条件写，最后写入者胜） |
| 建议 / 缺口处理 | A | 放行 | 放行 | |
| `POST /templates`、`PUT /templates/{id}`、`POST /templates/{id}/duplicate` | B | **拒** | 放行 | `ADMIN_WRITE_DISABLED`（7003） |
| `POST /templates/{id}/publish\|offline`、`POST /templates/batch/{publish\|offline}` | B | **拒** | 放行 | 写 `enabled` / `state` / 窗口 |
| `DELETE /templates/{id}`、`POST /templates/{id}/restore`、`DELETE /templates/{id}/purge`、`POST /templates/batch/{delete\|restore}` | B | **拒** | 放行 | 写 `deleted` |

整体 `PUT /templates/{id}` 里带的 `industryIds` / `slots` / `channels` 等 A 类字段，交接期也只能走上表的专用接口改（整体 PUT 整个被拒）。
用户端可见性在交接期不变：仍要求 `enabled=1` ∧ 挂了有效行业；挂上行业后无需改 Java 即对用户可见，`enabled=0`（Java 已停用）的模板挂了行业也不可见。
所有 A 类写都记审计（`afv_plaza_audit_log`，同一事务）。测试：`app/plaza/core/handover_integration_test.go`（两种模式逐接口放行 / 拒绝、A 类写前后比对 B 类整行含 `update_time`、`revision` 冲突）。

### 我的模板项（`GET/POST/PUT /my-templates`）

`id,name,templateId,industry(创建时所选行业 code),inputs(仍然有效的入参，失效的已剔除),variants,resolution,text,createdAt,updatedAt,templateAvailable,template(模板项 + slots；不可用时 null),stale,staleKeys[],staleHint`。
读取时用模板**当前** schema 逐键检查已存入参：键已不在 schema、或值不再满足约束（选项被删、超长、越界…）→ 进 `staleKeys`，`stale=true`、`staleHint="部分设置已失效"`，库里的原值不改；新增必填项而没存值**不算**失效。
模板整体下线：记录保留，`templateAvailable=false`、`template=null`、`inputs` 原样给出，可改名 / 删除，不能恢复到输入区。

### 收藏 / 我的模板 / 节点的表（迁移 `Go_V1.0.43__plaza_user_lib.sql`，判存在、可重复执行）

Go 独写：`afv_plaza_favorite(user_id,template_id)` 唯一；`afv_plaza_my_template`（`inputs_json`、`industry_id`、`variants`、`resolution`、`text`）；`afv_plaza_node`（`code` 唯一、`event_date`、`lead_days`、`enabled`、`deleted`）、`afv_plaza_node_template`。
上限由接口保证：收藏 500、我的模板 100，都用「`INSERT … SELECT … WHERE 当前数量 < 上限`」单语句，并发不会超。
节点种子 13 条（2026-12 ~ 2027-11 一个年度，按 code 判存在，不覆盖运营改动）；节点与模板的关联、下一年度节点由后台 `/plaza/admin/nodes`（下节）维护；没挂模板的节点用户端不出现。

### 营销节点后台（`/plaza/admin/nodes`，`creaft_catalog:view/edit`，写操作进审计 `resource_type=NODE`）
`afv_plaza_node` / `afv_plaza_node_template` 是 Go 独占表，交接期（`AdminWriteEnabled=false`）也可写，不碰 Java 的模板表。
- `GET /nodes?keyword=&enabled=1|0&page=&size=`：`{total,page,size,items[{id,code,name,why,eventDate,leadDays,showFrom,sortOrder,enabled,status,templateCount,createTime,updateTime}]}`，按 `eventDate` 倒序。`showFrom = eventDate - leadDays`；`status`：`UPCOMING`（未到展示窗口）/ `ACTIVE`（窗口内）/ `ENDED`（已过节点当天），与启停无关；`keyword` 匹配 name / code（`%` `_` 按字面）。
- `POST /nodes`：`{code,name,why?,eventDate(yyyy-MM-dd),leadDays?(默认 14，0~365),sortOrder?,enabled?(默认 true)}` → `{id}`。`code` 小写字母 / 数字开头，字母数字下划线，2~64；`name` ≤ 40 字，`why` ≤ 60 字（仅内容推荐，不含优惠 / 折扣 / 赠送，由运营把关）。code 重复 `4002 · NODE_CODE_DUPLICATE`。
- `PUT /nodes/{id}`：同上字段，缺省 = 不改；`code` 不可改。`PUT /nodes/{id}/enabled`：`{enabled}`。`DELETE /nodes/{id}`：逻辑删除，同事务清掉它的模板关联，并把 code 改名为 `<code>~del<id>` 释放唯一键（同 code 可重建）。节点不存在 / 已删 `4001 · NODE_NOT_FOUND`。
- `GET /nodes/{id}/templates?industryId=`：节点关联的模板（节点内顺序），`industryId>0` 只列属于该行业（一级含二级）的，行带 `nodeSortOrder`；`total` 是全部行业的关联总数；已删除的模板不返回。
- `POST /nodes/{id}/templates`：`{industryId,templateIds[≤50]}` 按行业添加（`industryId` 必填；模板须存在、未删除、属于该行业，否则 `4001 · TEMPLATE_NOT_FOUND` / `1003`）；已挂的跳过，追加到节点内末尾，一个节点最多 300 个；返回 `{added[]}`。
- `PUT /nodes/{id}/templates/order`：`{industryId,templateIds[]}`，只排该行业：给定的模板（须都已关联、属于该行业，可为子集）按新顺序占用它们原有的排序槽位，不改其它行业模板的相对位置；有重复 → `1003`。
- `DELETE /nodes/{id}/templates/{templateId}`：取消关联（幂等）。
- 审计动作：`NODE_CREATE/UPDATE/ENABLE/DISABLE/DELETE`、`NODE_TEMPLATE_ADD/REMOVE/SORT`。用户端 `GET /plaza/nodes` 读同两张表，编辑即时生效（无缓存）。

## 本周头条（`app/plaza/headline`，迁移 `Go_V1.0.35`）

- 用户端（gateway-api）：`GET /api/v1/plaza/headlines?industry=&subIndustry=&keyword=`（公开；缺 industry → HTTP 400 `PARAM_MISSING`；`Cache-Control: public, max-age=30`）。
- 运营端（admin-api，权限同前缀 `creaft_catalog:view/edit`）：`/api/v1/plaza/admin/headlines/`
  `scopes`、`current`、`compare`、`templates/{id}/headline-status`、`snapshots`、`snapshots/at`、`snapshots/{version}`、`POST refresh`、`POST preview`、
  `GET/PUT params`、`GET/POST rules`、`PUT/DELETE rules/{id}`（`PUT` 是整体替换，`startDate` / `endDate` 必填，缺 → 400 `PARAM_MISSING`）、`POST position`、`POST why`、`POST suppress`、`GET suppressions`、`DELETE suppressions/{id}`。
- 后台任务（摄入 / 刷新 / 校准 / 清理）**只在 admin-api 跑**（与广场模板域后台任务同一进程），`PlazaHeadline.Jobs` 默认 `false`，上线时打开；
  多副本靠 `afv_plaza_headline_ctl` 行租约选主。gateway-api 恒不跑（配了也无效）。

```yaml
PlazaHeadline:            # gateway-api / admin-api 都可配；Jobs 只在 admin-api 生效
  Jobs: false
  RefreshIntervalSeconds: 600
  IngestIntervalSeconds: 15
  DefaultParams: ""        # JSON，如 {"top-n":10}；管理端保存的参数优先
  Redis: { Addr: "" }      # 不配复用 Auth.Session 的 Redis
```

## 配置（gateway-api / admin-api 的 `Plaza`）

```yaml
Plaza:
  Industry: { Enabled: false }
  AdminWriteEnabled: false
  UsageDisplayMin: 20
  FreshDays: 14
  Media: { ThumbRule: "?x-oss-process=image/resize,w_640/format,webp/quality,q_80" }
  Quote: { JavaBaseURL: "", ProbeUserID: 0, EtaEntryCodes: [], AnonPerMinute: 30, AnonPerDay: 600, GlobalPerMinute: 600 }
  Overlay: { DefaultFontRid: "", AssetEnabled: true, PerMinute: 30, PerDay: 500, Auto: { Enabled: true, MaxAttempts: 5, MatchWindowMinutes: 1440, Batch: 10, EntryCodes: [agentImage] } }   # 文字叠加（Auto = 服务端自动叠字）：兜底字体 rid（须有商用授权）；媒体服务地址沿用 Upload.Mediasvc.BaseURLs/Token（gateway-api 也要配）
  IPHashSalt: "<随机串>"   # 生产（Mode: pro）必须配置；为空或仍是默认值 plaza-ip-salt 时 gateway-api 拒绝启动
```

匿名限频 / 去重 / 审计的调用方 IP 由 `pkg/clientip` 取：只信最近一跳可信代理（回环 / 内网对端）写的头——入口 nginx 用 `$remote_addr`
覆盖 `X-Real-IP`，而 `X-Forwarded-For` 是追加写、首段可被客户端伪造，所以不取 XFF 首段；无 `X-Real-IP` 时取 XFF 最后一段；直连（非可信对端）只认 TCP 对端地址。

## 模板编辑器配套接口（素材上传 / 技能下拉 / 变更历史）

后台模板新建 / 整体编辑（`POST /templates`、`PUT /templates/{id}`，字段、`revision` 并发见上）的配套接口，前缀同 `/api/v1/plaza/admin`。

| 接口 | 权限 | 说明 |
|---|---|---|
| `POST /uploads/presign` | `creaft_catalog:edit` | `{kind?, contentType, fileName?, size}`。白名单：图片 `image/jpeg\|png\|webp` ≤ 10MB，视频 `video/mp4\|webm` ≤ 200MB（`size` 必填；`fileName` 给了就校验扩展名与类型一致）。返回 `{uploadUrl, method:"PUT", headers:{Content-Type}, url, key, kind, maxSize, expiresAt(ms)}`：前端把文件原样 PUT 到 `uploadUrl`（须带 `headers`），对象落公有桶 `<Storage.Prefix>/plaza-template/<yyyymm>/<32位hex>.<ext>`；签名有效期上限 10 分钟。存储不是 oss 模式 → 7003「当前环境未开启直传」 |
| `POST /uploads/confirm` | `creaft_catalog:edit` | `{url, kind?}`。校验：地址必须是本存储签发，且对象 key 严格匹配 `<Prefix>/plaza-template/<yyyymm>/<32位hex>.<jpg|png|webp|mp4|webm>`（拒外链 / 带查询串 / 路径穿越 / 用户上传路径里夹带 `plaza-template` 的 key），Stat 与返回的是同一个规范化 key；HEAD 对象存在、大小 ≤ 上限、Content-Type 在白名单且与扩展名一致；图片读取内容做真实类型嗅探 + 取宽高（512~8192）；视频校验文件头（mp4 `ftyp` / webm EBML）+ ffprobe 取宽高与时长（时长 ≤ 120 秒、分辨率 ≤ 3840×2160 不分横竖，超出拒收，可配）+ 抽首帧封面（媒体服务 `cover`，其次显式配置的 ffmpeg；产物转存到公有桶）。返回 `{url, kind, contentType, size, width, height, durationSec?, coverUrl?, mediaStatus, note?}`，`url` / `coverUrl` 可直接写进模板（`payload.result[]` / `videoUrl` / `posterUrl`）。视频的 `mediaStatus` 恒为 `PROCESSING`（保存进模板后由后台任务处理，见「视频处理流水线」）；`coverUrl` 仅供编辑器即时预览，抽不出来也不影响流程，`note` 说明原因 |
| `GET /skills?keyword=` | `creaft_catalog:view` | 技能下拉（只读 Java 表 `afv_compat_agent_skill`，不含 `config`）：`{items:[{agentSkillId(字符串), code, title, description, icon, category, enabled}]}`，按分类 / 权重排序，≤ 500 条；停用技能也列出（已绑定的模板要回显），`enabled=false` 由前端禁止新绑定 |
| `GET /templates/{id}/audit` | `plaza:audit` | 变更历史（已有，按模板分页 `page/size`）：每条 `action / operatorName / createTime / before / after / diff`，`diff` 为字段级 `{field:{from,to}}`，大字段（`prompt` / `payload`）只记长度与哈希，全文在 `before` / `after` 快照里 |

`TemplateInput.mediaStatus`（`READY|PENDING|PROCESSING`）仅为兼容保留，值被忽略：处理状态由服务端按「视频地址是否变化」决定。
整体编辑 / 新建补充校验：`payload.result` / `image_urls` 必须是 http(s) 地址数组（各 ≤ 20 张）、`payload.guidance` ≤ 1000 字、`prompt` ≤ 8000 字、`videoUrl` / `posterUrl` 必须是 http(s)、标签 ≤ 20 个且单个 ≤ 32 字。
`GET /capabilities` 的 `writable` 新增 `uploads`（装配了直传存储才为 true，与写权属无关；交接期上传可用，因为上传不写模板表）。
大小限制：`pkg/storage` 只有 URL 签名（V1），不支持 POST policy（`content-length-range`），大小签不进直传地址，只能在 confirm 时核对；因此签名有效期压到 ≤ 10 分钟。
**运维项**：废弃对象（被拒收 / 去重命中的重复对象 / 久未被引用的登记对象）由后台清理任务处理，见下方「废弃对象清理」，默认关闭。清理没开期间，仍可给桶的 `plaza-template/` 目录配 OSS 生命周期规则兜底（「7 天清理」）——注意生命周期规则不认模板引用，会误删仍被引用的老素材，所以开启本任务后应移除该规则，二选一。
confirm 拒收且对象已在桶里时，记一条错误日志，判据 `PLAZA_UPLOAD_REJECTED_ORPHAN`（含 key、url、拒收原因），同时入待清理队列 `afv_plaza_upload_orphan`。

**内容去重**（迁移 `Go_V1.0.44__plaza_upload_hash.sql`，表 `afv_plaza_upload_hash`，sha256 唯一）：confirm 全部校验通过后，流式读对象（上限同大小上限）算 sha256；同 sha256 已登记 → 直接返回**首次登记的结果**（`url` / `coverUrl` / 宽高等都是首次的），本次对象成为孤儿——记 `PLAZA_UPLOAD_DUPLICATE`（含 key、url、sha256、复用的 url）；清理开关开启时当场 `Delete`（失败入队重试），关闭时入待清理队列。命中同时刷新登记行 `last_hit_time`（Go_V1.0.48），清理宽限期据此顺延。没登记过 → 登记后原样返回。去重是优化不是校验：算哈希 / 读写登记表失败一律放行（返回本次结果，记错误日志）。同一对象重复 confirm 幂等。前端以返回的 `url` 为准（可能与 presign 给的 `url` 不同）。

**废弃对象清理**（迁移 `Go_V1.0.48__plaza_upload_cleanup.sql`：新表 `afv_plaza_upload_orphan` + `afv_plaza_upload_hash.last_hit_time`；代码 `app/plaza/core/upload_cleanup.go`；存储新增 `Store.Delete(ctx, key)`，幂等，OSS / local 都实现，key 必须在 `Storage.Prefix` 之下）。每天 04:00（北京时间，与回收站清理同一时点，Redis 锁保证多副本只跑一份）扫两类对象，**先删对象、成功后才删登记 / 队列行**：

| 来源 | 判据 | 宽限期 |
|---|---|---|
| 登记对象（`afv_plaza_upload_hash`） | 未被任何模板 / 示例素材引用 | `GREATEST(create_time, last_hit_time)` 起 `HashGraceDays`（默认 7 天）；登记结果里的视频封面（`coverUrl`）同样条件下一并删 |
| 孤儿对象（`afv_plaza_upload_orphan`：REJECTED 拒收 / DUPLICATE 重复） | 未被引用、也不是登记对象 | 入队起 `OrphanGraceHours`（默认 24 小时；拒收可能是读对象瞬时失败，客户端会对同一 URL 重试 confirm，所以不当场删） |

**删除的安全边界**（任何一项判不出来都当「不能删」）：
1. 总开关默认关；关闭时任务不跑、confirm 路径也不删对象（只入队）。
2. key 必须整体匹配 `<Prefix>/plaza-template/<yyyymm>/<32位hex>.<jpg|png|webp|mp4|webm>`（与 confirm 同一条正则），登记行还要求 `url` 含该 key；存储层 `Delete` 另有 `Prefix` 边界（拒绝 `..`、`//`、查询串、前缀之外）。
3. 引用检查：按 key 子串在 `afv_compat_plaza_template`（`video_url` / `poster_url` / `data_json` / `slots_json` / `channels_json` / `tags_json` / `description`）与 `afv_plaza_sample_asset`（`url` / `thumb_url`）里匹配，**不看 deleted / state / enabled**（回收站、下线、已删行都算引用，宁可多留）；用 key 而非整条 URL，换域名 / CDN 也认得出；引用检查出错 = 终止本轮。
4. 孤儿对象同时是登记对象（先拒收、重试 confirm 成功）→ 不删，只移出队列。
5. 删对象失败保留登记 / 队列行（队列行 `attempts` +1、`last_error`），下轮重试；对象已不在也算成功。
6. 分批（`BatchSize`，id 游标）、每轮最多删 `MaxDeletesPerRun` 个。

已知边界：引用检查与删除之间没有事务，理论上有「检查后、删除前刚好保存了引用」的毫秒级窗口，靠宽限期 + `last_hit_time` 刷新压缩；本任务只管登记 / 队列里的对象，不扫桶，也不管非模板域（如用户「做同款」入参）对 `plaza-template/` URL 的引用。

日志判据：`上传清理已删除登记对象 / 孤儿对象 / 封面对象`、`上传清理跳过 判据=…`（被引用 / key 不合规 / 同时是登记对象）、`上传清理 删除…失败`，以及每轮汇总 `上传清理完成 孤儿删除= 登记删除= 封面删除= 跳过= 删除失败=`。

**上线顺序**：先执行 `Go_V1.0.48`（未执行时入队会记错误日志但不影响 confirm）→ 部署 → 观察若干天 `afv_plaza_upload_orphan` 的增长与日志 → 再把 `Cleanup.Enabled` 改为 true。回滚：改回 false 即可，已删对象不可恢复。

配置（仅 admin-api 的 `Plaza.Upload`；默认不启用直传）：

```yaml
Plaza:
  Upload:
    Storage: { Mode: oss, Prefix: aigc-local-life }   # 须与 gateway-api 同一个公有桶（afv_system_config 的 storage_public_*）
    MaxImageMB: 10
    MaxVideoMB: 200
    Cleanup:                                           # 废弃对象清理，默认关闭
      Enabled: false                                   # 总开关，需显式开启
      HashGraceDays: 7                                 # 登记对象多少天没被引用才删
      OrphanGraceHours: 24                             # 孤儿对象入队多少小时后才删
      BatchSize: 100                                   # 每批扫描行数
      MaxDeletesPerRun: 1000                           # 每轮最多删除对象数
    PresignTTLMinutes: 10                              # 上限 10
    MaxVideoSeconds: 120                               # 视频 confirm 上限
    MaxVideoLongSide: 3840
    MaxVideoShortSide: 2160
    FFprobe: ""                                        # 留空取 PATH 里的 ffprobe；没有则视频不做深度校验、也不转码
    FFmpeg: ""                                         # 显式配置才启用本机 ffmpeg 抽帧兜底
    Mediasvc: { BaseURLs: [], Token: "" }              # 媒体服务（首帧封面）；不配则只走 FFmpeg 兜底
```

## creaft v1 兼容三接口（Go 复刻，strangler 切流用）

`GET /api/v1/plaza-templates/list`、`GET /api/v1/plaza-templates/tags`、`GET /api/v1/plaza-headlines`：老客户端在用，**线上字段与行为以 Java 为准，不改名、不删、不加**（不接受 `industry` 等任何新参数，传了也忽略）。新 Web 不调这三条，走上面的原生 `/api/v1/plaza/*`。

- 实现：`app/plaza/v1compat`（逐条复刻 Java `CreaftV1SupportController#plaza / #plazaTags` + `CompatSupportService#plazaTemplates`、`PlazaHeadlineController` + `PlazaHeadlineService#listCurrent` + `PlazaTemplateUsageStatsService`）；v1 头条在 Go 里永久是 legacy 实现（读 `afv_plaza_headline` 全部规则按模板去重合并，不看范围 / 抑制 / 快照）。
- 信封：`pkg/v1wire` 的 `WriteJavaOK / WriteJavaFail`，键序 `{code,reason?,msg,data,requestId,timestamp}`，成功不带 `reason`，`data` 为空写 `null`；`requestId` 照 Java `CreaftV1RequestIdFilter` 规整（入站 `X-Request-Id` trim 后 8~64 位字母数字 / 连字符才沿用，否则生成 32 位小写十六进制），并回写同名响应头。响应设 `X-Jsoncase: off`，jsoncase 中间件原样放行（没有改 `pkg/jsoncase` 的白名单）。
- 字段与 Java 的对应：列表项 `{payload, plazaTemplateId, title, agentSkillId(字符串), tags, authorName, usageCount}`，分页 `{current, pages, total, data}`（Java 用 `Map.of`，键序不确定，比对按规范化 JSON）；头条 `{statsFrom, statsTo, items[{headlineId, rank, plazaTemplateId, title, agentSkillId, tags, payload, why, uses, previousUses, trend, isNew, startDate, endDate}]}`。
- 值的写法照 Jackson 往返（Java 先把 JSON 列读成 `LinkedHashMap` 再写回）：键保持出现顺序、重复键后值覆盖且位置不变；整数原样（`-0` → `0`）；小数按 `Double.toString`（`1e10` → `1.0E10`，`5e-324` → `4.9E-324`）；字符串按 Jackson 转义表（只转 `"` `\` 与控制字符，`\u001F` 大写，`<>&`、U+2028 原样）。`agentSkillId` / 标签 / `authorName` 取 `String.valueOf`（数字 `7` → `"7"`，对象 → `{a=1, b=[true, null]}`）。
- 两处 Java 内部不一致按 Java 原样保留：列表 payload 摘掉 `authorName`，头条 payload **保留** `authorName`；列表标签不过滤（`null` → `"null"`、空串保留），头条标签去掉 `null` 与无文字的（含全角空格），都不 trim。数据损坏时列表报错（JSON 列是数组 / 字符串 → HTTP 500 `5002 DATABASE_ERROR`「兼容数据损坏」；是 JSON `null` → HTTP 500 `5001 SYSTEM_ERROR`），头条当空对象继续。
- 参数绑定照 Spring：`page` / `size` 缺省 1 / 10，空串取缺省，去掉所有空白，支持 `0x` / `#` 十六进制，重复传取第一个，转不了 → HTTP 400 `1003 PARAM_INVALID`「参数 page 格式不正确」+ `data.errors[{field,reason:INVALID_TYPE,message}]`（先校验 page 再校验 size）；`tags` 只传一个时按逗号拆开并逐个 trim，传多个时不拆不 trim，命中任一即返回；头条 `tag` / `keyword` 传多个时用逗号拼成一个。
- 统计：近 7 天 / 前 7 天滚动窗口（Asia/Shanghai），实例内缓存 10 分钟，刷新失败沿用上一份；与 Java 各自缓存，`usageCount / uses / previousUses / trend / isNew` 两边会因缓存相位差一点（diff 工具按容差放过）。
- 只读：本包只有 `SELECT`（`afv_compat_plaza_template`、`afv_plaza_headline`、`afv_agent_turn`），单测用 sqlmock 与源码扫描双重断言；不写任何共享表。
- 开关：gateway-api `Plaza.V1Compat.Enabled`（默认 `false` = 不注册路由，网关上这三条 404，nginx 也仍指向 Java）。

```yaml
Plaza:
  V1Compat:
    Enabled: false   # true 只是让网关能接；有没有流量由 nginx 决定
```

### 有意偏差（与 Java 不同，且故意不同）

| # | 情形 | Java | Go | 理由 |
|---|---|---|---|---|
| 1 | 分页越界：`page<1` 且 `size≠0`、`size<0`、`(page-1)×size` 溢出成负数 | `subList` 抛 IndexOutOfBounds / IllegalArgument → HTTP 500 `5001`「系统内部错误」 | HTTP 400 `1003 PARAM_INVALID`「参数 page/size 超出范围」+ `data.errors[{reason:OUT_OF_RANGE}]` | Java 缺陷不是契约（DESIGN 9.1）；diff 语料不含这类请求，golden 单测断言 Java 侧仍是 500、Go 侧是 400 |
| 2 | 同一参数传多个且第一个是空串（`page=&page=2`） | 基本类型 `long` 收到 null，按 Spring 行为推断为 500（未实测） | 取缺省值 | 同上，属 Java 缺陷 |
| 3 | `page` / `size` 用非 ASCII 数字（全角 `１`、阿拉伯数字） | `Long.valueOf` 接受 Unicode 数字 | HTTP 400 `PARAM_INVALID` | 老客户端不会发；不值得复刻 |
| 4 | 非 GET 方法（POST / PUT / HEAD…） | 匿名 401；带有效 token 405 | go-zero 路由不匹配（404 / 405） | 三条都是只读 GET，老客户端不会发别的方法 |

**不算偏差、照 Java 抄的边角**（看起来怪但 Java 线上就是这样，golden 已覆盖）：`size=0`（以及 `page≤0&size=0`）成功返回 `pages=0、data=[]、current=1`——DESIGN 9.1 写的「`size<1` 会 500」与 Java 实测不符，已按实测更正；`page` 大于 1 时 `current` 原样回显（越界页 `data=[]`）；`(page-1)×size` 溢出回绕成非负（如 `page=4611686018427387905&size=4`）时 Java 返回第一页且 `current` 为原值，Go 一样；标签清单只扫前 1000 条模板；标签清单按 `String.compareTo`（UTF-16 码元序，增补字符排在 U+E000–U+FFFF 之前）。

**理论上的残余差异**（库里不会出现，不处理，登记在此）：JSON 列里的孤立代理项转义 `\ud800`（Jackson 保留，Go 解成 U+FFFD；MySQL JSON 列不收）；`toLowerCase(Locale.ROOT)` 只补了 U+0130 与词尾 Σ 两条特殊规则，Java 21 与 Go 的 Unicode 版本在极新的字符上大小写表可能不同（只影响头条 `keyword` 的命中）。

### 黄金样本与测试

- `app/plaza/v1compat/testdata/fixture.json`：输入（模板 / 头条 / 统计 + 55 条用例：分页边界、size=0、溢出、标签并集 / 数字标签 / `"null"` / emoji / 增补字符、头条 tag / keyword / 希腊字母大小写、损坏数据、空库）。
- `testdata/java_golden.json`：`tools/plazav1diff/javagolden/GoldenGen.java` 用 **fusion-aigc 编译产物里的真实类** + Jackson 2.17.3 跑同一份 fixture 得到的 Java 响应（另含 Jackson 往返与 `String.compareTo` 样本）。`golden_test.go` 要求 Go：HTTP 状态相同、规范化 JSON 相同、每个 payload 原文逐字节相同（只有偏差 #1 例外，且断言 Java 仍是 500）。
- `integration_test.go`（`PLAZA_V1_TEST_DSN`，库名须含 scratch）：同一份数据灌进 MySQL，真库与内存两条路径输出一致（验证 SQL 的 enabled / deleted / 排序 / 日期窗口 / 统计窗口）。
- `http_test.go`：Spring 绑定、信封、requestId、jsoncase 放行、未知参数忽略、统计缓存 / 失败沿用、只读（sqlmock）。

### 切流步骤（Java → Go，按路径、可灰度、可秒级回滚）

前置：Java 继续是两张表的写者，Go 只读；切流与后台写权交接（上面「写权切换步骤」）互不牵连，谁先谁后都行。

1. **开 Go 开关**：gateway-api 配 `Plaza.V1Compat.Enabled: true` 并发布。此时 nginx 仍指向 Java，公网无流量；在机器上自检：
   `curl -s http://127.0.0.1:<网关端口>/api/v1/plaza-templates/tags | head -c 300`。
2. **跑 diff 到 0 差异**（`tools/plazav1diff`，用法见其 README）：
   - 预发（库拷自生产）：`go run ./tools/plazav1diff -java http://<Java 内网> -go http://127.0.0.1:<网关端口> -report /tmp/plaza-v1-diff.md`，退出码 0；
   - 生产只读影子：同一命令每 5 分钟跑一次（cron 或 `while sleep 300`），任一次退出码非 0 告警，**连续 24 小时 0 硬差异**才进下一步。容差内差异（统计相位）不算失败。
3. **nginx 把三个路径指到 Go**（`deploy/server/nginx/aigc-local-life.conf.tmpl` 里有说明性注释，模板默认不改路由）：在 `location /api/v1/` 之前加三条精确匹配，用变量控制上游：

   ```nginx
   # http 段：按比例灰度（先 10%，再 50%，最后 100%；回滚改回 aigc_java）
   split_clients "${remote_addr}${http_user_agent}" $plaza_v1_upstream {
       10%     aigc_gateway;
       *       aigc_java;
   }
   # server 段（放在 location /api/v1/ 之前）
   location = /api/v1/plaza-templates/tags { proxy_pass http://$plaza_v1_upstream$request_uri; }
   location = /api/v1/plaza-templates/list { proxy_pass http://$plaza_v1_upstream$request_uri; }
   location = /api/v1/plaza-headlines      { proxy_pass http://$plaza_v1_upstream$request_uri; }
   ```

   `proxy_pass` 带变量时 nginx 不再自动拼 URI，所以写 `$request_uri`（保留 query string）；变量值必须是 upstream 块名。先切 `tags` / `list`，稳定后再加 `plaza-headlines`（可以先只写前两条 location）。每次 `nginx -t && nginx -s reload`，并用 `curl -sI 'https://<域名>/api/v1/plaza-templates/list?page=1&size=1&tags=a&tags=b'` 确认 query 透传。
4. **观察**（每档至少 1 天）：gateway-api 日志里 `plaza v1compat:` 开头的错误、5xx 比例、接口耗时；diff 影子继续跑（此时 Java 仍可直连内网对照）。
5. **回滚**：把 `$plaza_v1_upstream` 改回 `aigc_java`（或删掉三条 location）→ `nginx -t && nginx -s reload`，秒级生效。Java 的三条路由与读逻辑一直在、读同两张表，回滚不需要任何数据操作；Go 开关可以不关（没流量）。Java 侧的回滚目标代码在回滚期（≥2 周）结束后才另起变更删除（DESIGN 9.4-6）。


## 视频处理流水线（迁移 `Go_V1.0.46__plaza_media_pipeline_channel_delete.sql`）

`media_status`：`PROCESSING | READY | FAILED`（旧值 `PENDING` 已回填为 `PROCESSING`，不再写入；列放宽到 `VARCHAR(16)`）。
- 进入 `PROCESSING`：新建带 `videoUrl` 的视频模板；整体编辑 / `PUT /attrs` 换了 `videoUrl` 或图片改视频；`FAILED` 的模板运营补了封面（`posterUrl` 由空变非空）；`POST /templates/{id}/reprocess-media`。进入时清空 `mediaError / mediaNote / 尝试次数 / 租约`。
- 后台任务（admin-api `RunBackground` 里单独的 goroutine，每 15 秒一轮，受 `AdminWriteEnabled` 保护）：认领 → 缺封面则抽首帧（媒体服务 `cover`，其次显式配置的 ffmpeg）→ 视频不是 H.264(yuv420p) MP4 则本机 ffmpeg 转码（`Plaza.Upload.FFmpeg` 显式配置才启用；产物落公有桶，`videoUrl` 换成新地址）→ 写回并置 `READY`。已有封面（运营传的）不会被覆盖；视频不在本站存储里（外部地址）不转码。**媒体服务目前没有通用转码接口**（`video/conform` 只在需要去音轨 / 变帧率 / 缩放 / SDR 重打标时才重编码），所以没配 ffmpeg 时只抽封面，`mediaNote` 记「未转码」，仍置 `READY`。
- 失败：重试 3 次（退避 1 分钟、5 分钟），第 3 次失败置 `FAILED` 并写 `mediaError`；能力缺失（没有封面来源、转码产物超限）直接 `FAILED` 不白耗重试。写审计 `TEMPLATE_MEDIA_READY / TEMPLATE_MEDIA_FAILED / TEMPLATE_MEDIA_REPROCESS`。
- 并发：行租约。认领是条件 `UPDATE`（`PROCESSING`、租约过期、到重试时间、次数 < 3）写 `media_lease_token`，租约 15 分钟（单次处理超时 14 分钟）；写回必须仍持有 token，处理期间换视频 / 重新处理会清 token，旧结果丢弃。进程崩溃：租约到期后重新认领；次数已满且租约过期的卡死行被置 `FAILED`（「处理多次中断或超时」）。
- 发布前检查只放行 `READY`；`PROCESSING` / `FAILED` 的拒绝文案分别提示处理中 / 带失败原因。
- 后台列表每行与详情新增 `mediaStatus / mediaError / mediaNote / mediaAttempts`。

| 接口 | 权限 | 说明 |
|---|---|---|
| `POST /templates/{id}/reprocess-media` | `creaft_catalog:edit` | 重新处理视频：`FAILED`（或想重做的 `READY`）重新排队；`PROCESSING` 拒绝（1003）；非视频模板拒绝。返回 `{id, updatedAt, revision}` |

## 渠道删除与审计检索补齐（同一迁移加 `afv_plaza_channel.deleted / deleted_at`）

- `DELETE /channels/{id}`（`creaft_catalog:edit`）：逻辑删除（`deleted=1, enabled=0`），审计 `CHANNEL_DELETE`。被模板引用（含回收站里的，恢复后会生效）时拒绝：`4002 · RESOURCE_CONFLICT`，文案带引用数、回收站数与示例模板 id；未引用才可删。删除后列表与保存校验都不再认它，**code 不可复用**（再建同 code 报冲突），已删除的渠道不可编辑。
- `GET /channels` 每行新增 `templateCount`（引用该渠道的模板数，含回收站）与 `trashCount`，前端据此提前置灰删除按钮。
- `GET /audit`（`plaza:audit`）新增：`operatorName`（操作人姓名模糊匹配：审计行里的 `operator_name`，或关联 `afv_admin_user` 的 `real_name / username`）、`batchId`（批量操作批次 id 精确检索）。`action` / `resourceType` 不含 `*` 为精确匹配（兼容下拉），含 `*` 为通配：`TEMPLATE_*` 前缀、`*_PUBLISH` 后缀、`*MEDIA*` 包含（其余字符按字面匹配，`_` `%` 已转义）。各条件可组合。
