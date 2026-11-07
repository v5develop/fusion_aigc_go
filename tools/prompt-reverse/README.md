# prompt-reverse（内部 sidecar）

Python 标准库服务（`serve_reverse.py` + 网关客户端 `aig_client.py`），给 Go 侧（studio-rpc / 商拍与姿态编排）做「读图 + 反推提示词」。模型仍是 DeepSeek 视觉模型，但**经 ai-gateway 调用**（调用方 `prompt-reverse`，模型组默认 `vlm.deepseek-flash`），不再直连 api.deepseek.com、不持有 DeepSeek Key；**不生成图片**，没有页面、没有计费、没有对外转发。

版本：`2026-11-07.merged`（`/api/status` 返回）。来源：上一版 `2026-10-30.merged` 三方合并产品方两个交付包——姿态段取 `prism-pose-fission-delivery-2026-11-07.1`（多人 1–4 人、`mapping`、人数漏检重问、全员未成年校验），商拍段（反推 / 试穿 / 组合 / 裁切）取 `prism-reverse-update-2026-11-07.2`（裁切重试与兜底框、target/keep、【层叠顺序】、按品类覆盖范围锁、内衣专用结构、Seedream `prompt_version=new`）；本仓自己的改动（经 ai-gateway、只留内网路由、`_colors_text` 修复、10-30.1 的「按件面料」）保留。合并清单写在 `serve_reverse.py` 文件头注释里。**提示词模板、清洗函数、CATS、读图字段整理一律以交付包为准，不自己改**——要改先过黄金回归。

### 2026-11-07 这次变了什么

- `POST /api/pose/source`：`source` 新增 `person_count`、`group`、`interaction`、`people[]{label,position,primary,gender_age,face,skin,hair,makeup,body,expression,outfit[],accessories[],pose,occluded}`、`people_truncated`（按画面左→右，最多 4 人）；顶层人物字段仍等于 `primary` 那位，单人调用不变。`person_count` / `group` 里的 Pn 多于 `people[]` 时自动重问一次；未成年校验覆盖所有人物。
- `POST /api/pose/prompt`：请求新增可选 `mapping`（长度 = `source.people`，值为 `pose.people` 下标，`-1` = 保持原姿势，原样生效、不按默认配对补位；不传或无效（长度不符 / 越界 / 重复 / 全 `-1`）时整组用默认：左→左、参考图单人配给 primary）；响应新增 `mapping`（实际使用的配对，原图单人为 null）与 `pose.person_count/group/interaction/people[]`。原图 ≥2 人走 `build_pose_prompt_group`（短版 `build_pose_prompt_short_group`），单人提示词与旧版一致。
- `POST /api/reverse/*`：crop-plan 关键点 / 候选表空结果重试（单件 3 次、look/候选 2 次）+ 确定性兜底框；提示词统一 target/keep、组合【层叠顺序】、按实际品类生成版型锁、内衣覆盖范围锁；`prompt_version=new` + `target_model=seedream|nano` 仍是实验版（默认稳定版）。
- 读图耗时变长：多人原图约 20–60s、姿势图约 40–140s（含重问与网关重试），仍受 `REVERSE_REQUEST_BUDGET_S`（默认 190s）约束，超出回 504。

### 下次交付包来了怎么升级

1. 找三份文件：上一版合并结果（本目录 `serve_reverse.py`）、交付包的基线（它是从哪一版改的，看交付包 CHANGELOG / diff；10-30 两份在 `reference/小岐AI-5181-s1.1-后端交付包-2026-10-30/code/`）、交付包新版。
2. 按段三方合并（姿态段 = `# ---- 姿势裂变` 到 `def analyze(` 之前，其余为商拍段）：`git merge-file ours.py base.py theirs.py`，两段分别做；冲突只会出在本仓改过的地方（模型调用、路由表、`/api/status`）。
3. 检查交付包是否「早于」本仓已有的产品改动（这次 11-07.2 的分支早于 10-30.1，合并会删掉「按件面料」）：`diff 基线 交付包` 里的删除项逐条确认是产品有意删除还是分支早，后者恢复。
4. `grep -nE 'DEEPSEEK|_post_json|urllib.request|PRISM_EMBED|UPSTREAM' serve_reverse.py` 必须为空（模型调用一律 `llm_chat`）；改文件头合并清单和 `VERSION`，同步 `tests/test_routes.py` 的版本断言。
5. 跑下面全部离线测试 + `golden_tryon.py selftest`；交付包自带的离线测试（如 `test_multi_person.py`）拷进 `tests/`，改成加载同级 `serve_reverse.py`。

## 路由

未知路由一律 404。所有 POST 为 `application/json`，body ≤ 24MB，否则 413；JSON 无法解析 400；业务校验失败 422（pose、crop-plan、crop-check）或 400（analyze-batch）；上游读图失败/未配置网关 502。

图片字段一律是 **data URL**（`data:image/...;base64,...`），非 data URL 的图会被静默丢弃。

| 方法 路径 | 请求（主要字段） | 响应 |
|---|---|---|
| `GET /api/status` | - | `service`("prompt-reverse")、`version`、`llm_configured`（网关地址与 secret 均已配置）、`model` / `model_group`（实际配置的模型组）、`model_label`、`vision`（固定 true，读图能力由模型组声明）、`gateway`（网关地址，不含凭据）、`gateway_ready` / `gateway_reason`（`POST /v1/catalog/readiness` 结果，缓存 60s；未配置时 null）、`mock` |
| `POST /api/pose/source` | `image`（data URL，单张 ≤12MB） | `{source{has_person,gender_age,face,hair,outfit[],scene,background,light,camera,framing,...,person_count,group,interaction,people[],people_truncated}, meta{model,elapsed_ms,usage}}`（`meta.model` 为模型组名，`usage` 为网关口径 `input_tokens/output_tokens/...`）；无人物/任一人物疑似未成年 422 |
| `POST /api/pose/prompt` | `source`（上一步返回的 source）、`image`（姿势参考图 data URL）、`notes`、`mode`（`short` 走短版，其余长版）、`mapping`（可选，多人配对） | `{prompt, mapping, pose{orientation,arms,legs,expression,confidence,...,person_count,group,interaction,people[]}, meta}`；强表情（露齿/大笑/惊讶…）时提示词顶部带「表情按图2」指令 |
| `POST /api/reverse/analyze` | `category`、`items[]`、`images[]{role,label,data}`（role：主图角色 / `局部细节` / `人物模特`；最多选 9 张）、`fabric`、`notes`、`lengths{}`、`sleeves{}`、`prompt_version`("new" 走实验版)、`target_model` | `{prompt, meta{category,model,degraded,vision,images,garment_match,garment_type}, warning}`；读图失败时降级为文字模板，`meta.degraded=true` |
| `POST /api/reverse/analyze-batch` | 同 analyze，另加 `models[]{id,label,data}`（≤9，缺省则取 `images` 里 role=人物模特）、`concurrency`（1–6，默认 3） | `{results[]{index,model_id,prompt,meta,warning,error?}, meta{models,concurrency,prompt_version,target_model}}`；单个模特失败只在该项写 `error`，不影响其它 |
| `POST /api/reverse/crop-plan` | `images[]{data,angle?,role?,candidates?}`（**只处理前 6 张**）、`category`、`items[]`、`fabric`、`notes` | `{plans[]{index,angle,garment_box?,crops[]{label,box[x,y,w,h]归一化,confidence}}, meta{images,plans,mode?,elapsed_ms}}`；空 plan 服务内自动补一次并用兜底框 |
| `POST /api/reverse/crop-check` | `items[]{data,label}`（**一次 ≤12 张**，超出静默截断） | `{results[]{index,ok,confidence}, meta{items,elapsed_ms}}` |

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `AIG_BASE_URL` | 空 | ai-gateway 地址（内网），必填才能读图 |
| `AIG_SECRET` | 空 | 调用方 `prompt-reverse` 的 HMAC secret（= ai-gateway 侧 `AIG_CALLER_REVERSE_SECRET`），必填；只在 sidecar 里，不下发、不写日志 |
| `AIG_KEY_ID` | `prompt-reverse-v1` | |
| `AIG_CALLER` | `prompt-reverse` | 须与 key 绑定的 caller 一致 |
| `REVERSE_MODEL_GROUP` | `vlm.deepseek-flash` | 组内 DeepSeek 官方 `deepseek-chat.vl.deepseek-flash`（tier 0）优先，百炼 `bailian-chat.vl.deepseek-flash`（tier 1）自动切换备用 |
| `REVERSE_ALLOWED_DEPLOYMENTS` | 空 | 排障钉渠道，逗号分隔；非空时写入 `constraints.allowed_deployments`，只走列出的部署（不再切换备用）。如 `deepseek-chat.vl.deepseek-flash` 或 `bailian-chat.vl.deepseek-flash`；`/api/status` 的 `allowed_deployments` 回显当前值，排障完清空 |
| `AIG_WAIT_S` | `25` | 长轮询 `GET /v1/calls/{id}?wait=` 秒数（≤60） |
| `REVERSE_BIND` | `127.0.0.1:8090` | `host:port`；容器内用 `0.0.0.0:8090` |

仍会读取同目录 `.env`（不覆盖已有环境变量），但 `.env` 不随镜像分发，见 `.env.example`。

## 启动

```bash
cd tools/prompt-reverse
AIG_BASE_URL=http://127.0.0.1:8080 AIG_SECRET=... python3 -I serve_reverse.py     # 127.0.0.1:8090
docker build -t prompt-reverse . && docker run --rm -e AIG_BASE_URL=... -e AIG_SECRET=... prompt-reverse
```

`-I` 必须带（隔离模式，不从当前目录加载模块）；`aig_client.py` 由 `serve_reverse.py` 按绝对路径加载，两个文件必须放同一目录。仅依赖 Python 3.9+ 标准库。

## 模型调用（经 ai-gateway）

所有读图 / 文字反推都走 `llm_chat()` → `POST {AIG_BASE_URL}/v1/calls`（HMAC v1 七头签名，purpose `calls`，每次一个新 `Idempotency-Key`），body：
`{"operation":"chat.completion","model_group":REVERSE_MODEL_GROUP,"input":{messages,response_format?,thinking?,temperature,max_tokens},"constraints":{"deadline_at":now+超时,"allowed_deployments"?:REVERSE_ALLOWED_DEPLOYMENTS},"labels":{"caller":"prompt-reverse","route":<路由>}}`；
图片 part 为 `{"type":"image_url","uri":"data:image/...","detail":"low|high"}`（单张 ≤14,000,000 字符）。提交后 `GET /v1/calls/{id}?wait=AIG_WAIT_S` 长轮询到终态，取 `result.message.content`（为空取 `reasoning_content`）。

| 原 DeepSeek 参数 | 网关字段 |
|---|---|
| `response_format: json_object` | `input.response_format={"type":"json_object"}` |
| `thinking: {"type":"disabled"}` / 不传 | `input.thinking="off"` / 不传（模型默认） |
| `temperature` / `max_tokens` | `input.temperature` / `input.max_tokens` |
| HTTP 超时（180s / 120s） | `constraints.deadline_at`，本地等待预算 = 超时 + 15s |
| 400/422 去掉 `response_format` 重发 | 网关判 INPUT_REJECTED 且错误提到 response_format/json 时，去掉 JSON 模式、换新幂等键重发一次（crop-check 原来就不重发，保持） |

失败分类（`aig_client.py`）：`AigNetworkError`（连不上/超时，对应原 URLError）、`AigRetryable`（429/5xx、FAILED 且 NO_CAPACITY/VENDOR_FAILED/DEADLINE_EXCEEDED 等）、`AigRejected`（其它 4xx、FAILED 且 INPUT_REJECTED/CONTENT_POLICY 等）、`AigConfigError`。原降级链不变：视觉失败 → 文字降级 → 模板兜底；路由报错文案中 `读图返回 <code>` 的 code 为 HTTP 状态码或 `FAILED/<失败类别>`。

## 安全边界

- 默认只听 `127.0.0.1`；放进容器/内网时由网络层限制，不要映射公网端口。
- 无鉴权：靠网络隔离，只允许 Go 服务访问。
- 网关 secret 只存在于 sidecar 环境变量；`/api/status` 只返回是否已配置与网关地址（不含凭据）。日志只记路由名、耗时、状态码/失败类别，不记 secret、图片、提示词。
- 请求体 24MB 上限（超限在读 body 前直接 413）；pose 单张图 12MB 上限。
- 不提供文件服务：`/.env`、`/serve_reverse.py` 等一律 404；代码中无任何上游转发、无外部固定 IP。

## 与 Go 的调用关系

由 studio-rpc 的反推编排调用（设计见 `docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md` §4.2）：

- 商拍：crop-plan（>6 张分批）→ crop-check（≤12）→ analyze 每个模特一次，或 analyze-batch。
- 姿态：pose/source（按人物图 sha1 每批一次）→ pose/prompt（每个姿势一次；用户付费前经 `POST /api/v1/studio/pose/preview` 确认过配对时带 `mapping`，批次缓存键含 mapping）。
- 姿态配对预览（不计费）：pose/source 一次 + 原图 ≥2 人时每张姿势图 pose/prompt 一次（不带 mapping，只取 `pose.people` 与默认 `mapping`，提示词丢弃）。
- 商拍提示词灰度：studio-rpc `TryonPrompt.NewPercent`（或 `afv_system_config.studio_tryon_new_prompt_percent`）> 0 时，Seedream 批次按批次号哈希命中的带 `prompt_version=new,target_model=seedream`；Nano 一律稳定版。
- 超时：读图单次最长 180s，Go 侧 HTTP 超时应 ≥ 200s（姿态读图内部重试 4 次、间隔 2.5s×n）。
- 生图提示词由 Go 在首行加图序声明，正文逐字不动。

## 黄金回归

提示词必须与产品方交付逐字一致：

```bash
cd tools/prompt-reverse
# 1) 路由与边界 + 伪 gateway 联调（离线，不需要真网关）
python3 -I tests/test_routes.py -v
python3 -I tests/test_aig_client.py -v     # 签名测试向量、canonical query、长轮询、错误分类
python3 -I tests/test_multi_person.py      # 多人提示词 / 反转配对 / 参考图多一人 / 单人回归 / mapping 入参回参（读图打桩）
# 2) 姿态提示词验收：prompts.json 为字符串数组，取自真实 /api/pose/prompt 的 prompt 字段；
#    source.json（可选）为 /api/pose/source 响应，用于放行原图自带的词
python3 -I tests/validate_prompts.py prompts.json [source.json]   # 有错误退出码 1
```

改动 `serve_reverse.py` 后，还需用同一组 source/pose 对比改动前后的 `build_pose_prompt` / `build_pose_prompt_short` 输出（应逐字相等）。

### 商拍（试穿）黄金回归

用例在 `tests/golden/cases/`（3 组：case1 上衣真丝+鞋子皮革 E4；case2 两件两模特 analyze-batch D1/D2/D3；case3 整套合影路径）。**用例里的图是 64x64 纯色占位 PNG，真实样本由运营替换**（替换时保持 role / items / fabric 不变，图要互不相同，否则会被 sha1 去重）。

```bash
cd tools/prompt-reverse
# 1) 纯函数：组图规则 D1-D4（离线）。含 1 条 expectedFailure，记录 D3 与验收的冲突
python3 -I tests/select_images_test.py -v
# 2) 已配网关时：先起 sidecar（AIG_BASE_URL=... AIG_SECRET=... python3 -I serve_reverse.py），录制并写 tests/golden/expected/
REVERSE_BASE=http://127.0.0.1:8090 python3 -I tests/golden_tryon.py record
#    之后每次改动：在线重跑并与 expected 逐字 diff；未配网关（llm_configured=false）/ 连不上 / 加 --offline 时只做离线结构校验
python3 -I tests/golden_tryon.py check [--offline] [--case case1-top-silk-shoes-leather]
```

- `GOLDEN_IGNORE_REGEX`：diff 前从两边删掉匹配片段（时间戳类波动）；`GOLDEN_TIMEOUT` 单次请求超时（默认 200s）。
- `record` 拒绝在未配网关（llm_configured=false）时录制（降级模板不能当黄金），响应 `degraded=true` 的用例不写入。expected 里 `locked` 默认 false，产品确认后手工改 true。
- 没有 expected 的用例只校验输入并提示先 record，退出码 0；任一 FAIL 退出码 1，WARN 不影响退出码。
- `python3 -I tests/golden_tryon.py selftest`：清空内存中的网关配置，用 sidecar 的无读图模板自检校验器（不联网，结果是降级模板，不是黄金）。
