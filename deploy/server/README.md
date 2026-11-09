# aigc-local-life 服务器部署手册（宝塔 / 一键脚本，备选方式）

> **正式环境用 docker compose 部署**，见 [`../docker/README.md`](../docker/README.md)。本手册是没有 Docker 的服务器上的备选方式；两者的校验规则、迁移、配置生成是同一套代码。

一个安装包 + 一条命令：`bash install.sh`。脚本会检查环境与数据库、执行数据库迁移、生成配置、注册系统服务并启动、最后自检，同时渲染好 nginx 站点配置。**可重复执行**，升级也是同一条命令。

> **改配置改哪一份**：改**你运行 `install.sh` 的那个目录**里的 `deploy.env`（比如解压出来的 `/root/aigc-local-life/deploy.env`）。
> 脚本优先读它，装完会覆盖到 `/opt/aigc-local-life/deploy.env`——直接改装好的那份会在下次安装时被覆盖。两份不一致时脚本会停下来提醒。

## 正式版的几条硬规则

安装脚本会强制检查，不满足直接退出，没有放行开关：

| 规则 | 说明 |
|---|---|
| 鉴权只能是 `AUTH_MODE=token` | `dev` 会信任请求头 `X-User-Id` / `X-Admin-Id`，任何人可冒充用户或管理员 |
| 计费只能是 `BILLING_MODE=on` | 线上不允许免费（`off` / `shadow` 都不扣点）；计费配置自检有错误时中止安装 |
| 必须有执行面地址 | `AIGC_ENDPOINT`，或经 `JAVA_ENV_FILE` 从 Java 的 `.env` 读取 `AIGC_BASE_URL` |
| 必须有公网域名 | `PUBLIC_DOMAIN`，用来渲染 nginx 站点配置 |
| 必须与 Java 共用会话 | 会话 Redis 从 Java 的 `.env` 读取（`JAVA_ENV_FILE`）：nginx 把登录、点数等接口转给 Java，App 令牌必须两边都认 |
| 安装包不含 mock | 正式包里没有 mock 执行面，打包时自检 |

**对服务器上已有项目的影响**：
- 只新建 `aigc-local-life-*` 这几个系统服务，装在单独目录（默认 `/opt/aigc-local-life`）。
- 所有进程只监听 `127.0.0.1`：网关 18888、后台 18889、rpc 19201–19207（**避开了宝塔面板的 8888**）。端口被占用时脚本停下来提示，不抢端口。
- 数据库只新建本项目的表、给本项目的表加列、写入本项目的分类与计费配置；迁移记录写在独立的 `go_schema_history`，**不碰 Java 的 `flyway_schema_history`**。
- 不需要装 etcd、Docker、Go，服务之间本机直连。

---

## 一、准备

| 需要 | 说明 |
|---|---|
| 服务器 | Linux x86_64（CentOS 7+ / Ubuntu / Debian），root |
| 数据库 | 现有系统的 `fusion_aigc` 库，一个能建表、改本项目表结构的账号 |
| 会话 Redis | Java（AIGC）服务在用的那台 Redis，从 Java 的 `.env` 读取（`REDIS_HOST` / `REDIS_PORT` / `REDIS_PASSWORD` / `REDIS_DB`） |
| Java 服务 | 本机可达的地址（默认 `127.0.0.1:18080`），nginx 把非本地生活的 `/api/v1/` 转给它 |
| nginx | 宝塔自带即可，需要能给 `PUBLIC_DOMAIN` 配证书 |
| 出网 | 能访问阿里云百炼（文案、配音）、OSS 与 AIGC 执行面 |
| 安装包 | `aigc-local-life-<版本>.tar.gz`（开发机 `make package VERSION=<版本>` 生成在 `dist/` 下） |

现有系统里必须已有的配置（安装时检查，缺了直接停）：`afv_system_config` 里的公有桶 `storage_public_*`（含 STS 角色）、私有桶 `storage_private_*`、文案模型 `ai_writing_model_id`。

## 二、安装（第一次）

**1. 上传并解压**

宝塔「文件」→ `/root` → 上传 `aigc-local-life-<版本>.tar.gz` → 右键「解压」，得到 `/root/aigc-local-life/`。
（或命令行：`cd /root && tar xzf aigc-local-life-*.tar.gz`）

**2. 第一次运行，生成配置文件**

```bash
cd /root/aigc-local-life
bash install.sh
```

会提示「已生成 deploy.env，请先填写」然后退出。

**3. 填写 `deploy.env`**

必填项：

| 变量 | 填什么 |
|---|---|
| `DB_HOST` `DB_PORT` `DB_NAME` `DB_USER` `DB_PASS` | `fusion_aigc` 库的连接信息（宝塔「数据库」页可查 / 重置密码） |
| `PUBLIC_DOMAIN` | 前端访问的域名，线上为 `aigc.pengqiwl.com` |
| `JAVA_UPSTREAM` | Java 服务地址，线上为 `127.0.0.1:18080` |
| `JAVA_ENV_FILE` | Java 服务的 `.env` 路径（推荐）。执行面地址与会话参数从这里读，保证和 Java 一致 |
| `AIGC_ENDPOINT` | 不填 `JAVA_ENV_FILE` 时必填，与 Java 的 `AIGC_BASE_URL` 相同 |
| `SESSION_REDIS_*` | 默认不用填：从 `JAVA_ENV_FILE` 读。只在覆盖时才填；Java 的 `REDIS_HOST` 若是容器服务名，宿主机解析不到，必须在这里填 `SESSION_REDIS_ADDR` |
| `UNION_BASE_URL` | 联盟用户服务地址，登录用（与 Java 相同，只走 HTTP）；不配则登录接口返回「暂未开放」 |

与 Java 保持一致的项：

| 变量 | Java 侧对应 | 来源 | 默认 |
|---|---|---|---|
| `AIGC_ENDPOINT` | `AIGC_BASE_URL` | 留空时从 `JAVA_ENV_FILE` 读 | — |
| `SESSION_MAX_CONCURRENT` | `APP_SECURITY_TOKEN_MAX_CONCURRENT_SESSIONS` | 留空时从 `JAVA_ENV_FILE` 读 | `0`（不限） |
| `TOKEN_ACCESS_HOURS` | `TOKEN_ACCESS_HOURS` | 留空时从 `JAVA_ENV_FILE` 读 | `2` |
| `TOKEN_REFRESH_DAYS` | `TOKEN_REFRESH_DAYS` | 留空时从 `JAVA_ENV_FILE` 读 | `7` |
| `SESSION_REDIS_DB` | `REDIS_DB` | 留空时从 `JAVA_ENV_FILE` 读 | `0`（与 Java 默认一致） |

取值优先级：`deploy.env` 里显式写的 > `JAVA_ENV_FILE` 里的 Java 配置 > 默认值。安装和 `prepare` 会打印每个值的来源，便于核对。`JAVA_ENV_FILE` 指向的文件不可读时直接报错，不会静默回退。

可选项：

| 变量 | 说明 |
|---|---|
| `BACKUP_BEFORE_MIGRATE=1` | 有待执行的迁移时，先整库备份到 `/opt/aigc-local-life/backup/`。默认关闭；**首次上线建议打开**：迁移只能前进，备份是唯一的回滚手段 |
| `TASK_ID_PREFIX` | 提交执行面的任务号前缀，默认 `AiCreationLife`（生成 `AiCreationLife_<32位hex>`），只能字母数字 |
| `SWAGGER_ENABLED=1` | 打开接口文档页（仅本机可访问）；正式环境保持 `0` |
| `LINK_PARSE_URL` | 分享链接解析服务；留空则「爆款视频复刻」只能上传视频 |
| `FUSION_*` | 新用户建档交给 Java 的 `/api/v1/union/user-register`；留空由本服务自己写表 |

**4. 正式安装**

```bash
bash install.sh
```

6 步都是绿色 ✓、最后出现「部署完成」就好了。各步做的事：

| 步骤 | 内容 |
|---|---|
| 1/6 检查环境 | 系统、root、curl、安装包完整性、端口占用 |
| 2/6 检查数据库 | 连接、现有系统的表与配置、私有桶配置 |
| 3/6 数据库迁移 | 执行 `Go_V*.sql`，登记到 `go_schema_history`，已执行的跳过（见第四节） |
| 4/6 安装程序与配置 | 计费配置检查、生成服务配置（权限 600）、渲染 nginx 配置 |
| 5/6 启动服务 | 注册系统服务并启动 6 个服务 |
| 6/6 自检 | App 网关、图文笔记、海报口播、数字人、OSS 直传凭证、运营后台 |

自检在 token 模式下只确认各进程**在线**（没带令牌的请求返回 401 属正常）；业务是否可用见第五节。

**5. 配置 nginx**（见第三节），然后**生成系统音色试听**（只需一次）：

```bash
yum install -y jq     # Ubuntu/Debian：apt install -y jq
read -rp '后台账号: ' ADMIN_USER; read -rsp '口令: ' ADMIN_PASS; echo; export ADMIN_USER ADMIN_PASS
bash /opt/aigc-local-life/install.sh voices   # 正式环境是 token 鉴权，用现有后台账号登录后生成
```

## 三、对外入口（只经 nginx）

公网只开 80 / 443。网关、后台、各 rpc **都只监听 127.0.0.1**，不要在云安全组或宝塔「安全」里放行 18888、18889、19201–19207（含 studio 19205、assetlib 19206、canvas 19207；新增的三个端口尤其别忘，云安全组的入方向规则只保留 80 / 443，其余一律不放行）。

安装后 `/opt/aigc-local-life/nginx/aigc-local-life.conf` 已按 `PUBLIC_DOMAIN`、`GATEWAY_PORT`、`ADMIN_PORT`、`JAVA_UPSTREAM` 渲染好，路由如下：

| 路径 | 去向 | 说明 |
|---|---|---|
| `/api/v1/local-life` | Go 网关 | 本地生活全部 App 接口 |
| `/api/v1/local-life/admin` | Go 后台（与 Java 后台共用登录令牌，按 RBAC 校验权限） |
| `/api/v1/studio` | Go 网关 | studio（商拍换装 / 姿态复刻 / 图片生成）用户侧接口 |
| `/api/v1/studio/admin` | Go 后台 | studio 运营后台；两条 `^~` location 都已在模板里，`/admin` 那条在前 |
| `/api/v1/admin` | 404 | Java 的运营后台接口不走公网（运营走内网 / VPN）；确需公网访问时删掉模板里这一段 |
| `/api/v1/`（其余） | Java | 登录、用户信息、点数、素材库、刷新令牌等 |
| `/swagger-ui`、`/v3/api-docs`、`/actuator` | 404 | Java 的接口文档与监控不对公网 |
| 其它 | 404 | Go 接口文档 `/swagger` 只在本机 |

宝塔操作：
1. 「网站」→ 添加站点 `PUBLIC_DOMAIN`（纯静态即可）→「SSL」申请证书；
2. 站点「配置文件」里加入 `aigc-local-life.conf` 的 `upstream` 与 `server` 内容（或 `include` 进来）；
3. `nginx -t` 检查通过后重载。

前端地址：`https://<PUBLIC_DOMAIN>/api/v1/...`。

**运营后台（Go）经 nginx 对外**，路径 `/api/v1/local-life/admin`，与 Java 后台共用登录令牌，无需 ssh 隧道。

### studio 与反推 sidecar（脚本部署）

- `studio-rpc` 由 `install.sh` 与其它 rpc 一起装（端口 `STUDIO_PORT`，默认 19205），同样只听 127.0.0.1，不要在安全组放行。
- `assetlib-rpc`（资产库增强，端口 `ASSETLIB_PORT`，默认 19206）同上；nginx 模板已带 `/api/v1/asset-library/admin` 与 `/api/v1/asset-library` 两条 location。上线前的接入检查见根目录 README「资产库增强」。
- 反推 sidecar（`tools/prompt-reverse/serve_reverse.py`，Python 3.9+ 标准库）**不在安装包里**，需单独部署：用 `REVERSE_BIND=127.0.0.1:8090` 启动（`python3 -I serve_reverse.py`），**只听本机，不对公网、不进 nginx**；`deploy.env` 的 `REVERSE_SIDECAR_URL` 指向它（留空用 `http://127.0.0.1:8090`）。
- sidecar 目录需同时有 `serve_reverse.py` 与 `aig_client.py`。模型调用**经 ai-gateway**（调用方 `prompt-reverse`，模型组默认 `vlm.deepseek-flash`：官方优先、百炼备用），不再直连 DeepSeek；DeepSeek 官方密钥只在 gateway 管理端配置（账号 `deepseek-chat.main`）。sidecar 环境变量：`AIG_BASE_URL`（网关地址，同 `AI_GATEWAY_URL`）、`AIG_SECRET`（= ai-gateway 侧 `AIG_CALLER_REVERSE_SECRET`）必填；`AIG_KEY_ID`（默认 `prompt-reverse-v1`）、`AIG_CALLER`（默认 `prompt-reverse`）、`REVERSE_MODEL_GROUP`（默认 `vlm.deepseek-flash`）、`REVERSE_ALLOWED_DEPLOYMENTS`（排障钉渠道，逗号分隔，如 `deepseek-chat.vl.deepseek-flash` 或 `bailian-chat.vl.deepseek-flash`，写进 `constraints.allowed_deployments`；平时留空）、`AIG_WAIT_S`（默认 25）可选。
- `AIG_SECRET` **只放 sidecar 自己的环境**（systemd `Environment=` / 宝塔进程守护的环境变量），不要写进 `etc/` 下任何 yaml，也不要给 studio-rpc；`deploy.env` 里的 `REVERSE_AIG_*` / `REVERSE_MODEL_GROUP` / `REVERSE_ALLOWED_DEPLOYMENTS` 仅供 docker 部署的 compose 使用。
- `AI_GATEWAY_URL` / `AI_GATEWAY_KEY_ID` / `AI_GATEWAY_SECRET` 写在 `deploy.env`，由 `install.sh` 写进 studio 配置；URL 留空 = studio 提交返回「暂未开放」，其它功能不受影响。ai-gateway 侧需登记调用方 `fusion-go`（`AIG_CALLER_KEYS`）。
- `STUDIO_INPUT_URL_PREFIXES`（逗号分隔）写进 studio 的 `InputURLPrefixes`：素材库公有桶 / CDN 域名。本服务上传的地址自动放行，用户从素材库选的图不在本服务前缀下，不配会被拒「不是本服务上传的地址」。
- **`studio-rpc` 可多实例部署，依赖库里的行租约；扩容无需额外配置**：批次（编排 / 派发）与子任务（提交 / 轮询 / 交付）各有一把租约（`lease_owner` / `lease_until`，`Go_V1.0.21`），同一行同一时刻只由一个实例推进，不会重复调 sidecar / ai-gateway、重复转存。实例崩溃后租约到期（编排 10 分钟、轮询 3 分钟）由其它实例自动接管，接管时先按已有的 gateway 调用号查上游、没有才按幂等键提交，不会重复扣费。各实例时钟无需同步（租约时间用库的 `NOW(3)`）。
- `BILLING_MODE=on` 时 `studio-rpc` 自带预留单回收器（与 imagegen-rpc 的同一个 `billing.Reclaimer`，release 是 CAS，同时跑安全）：
  生成结果未知（gateway `UNKNOWN`，任务失败原因「结果未知，待对账」）的子任务**不主动退款**、不再续期，预留单到期由回收器自动退回；
  在途子任务（含排队 / 切图 / 反推阶段）每分钟续期一次，不会因排队久而过期。
- 状态口径：子任务 `5-交付中`（已出图、确认扣点中，不可被强制失败）；批次 `8-提交中`（逐张冻结中，不可取消；进程在此状态崩溃时 5 分钟后由轮询器退回已冻结点数并置失败）。

> **gateway 调用方密钥**：用 `bash deploy/scripts/gen-gateway-callers.sh --go-env deploy.env --gw-env aig-callers.env` 一次生成 `fusion-go` 与 `prompt-reverse` 两个密钥，并同时写好 Go 侧 `deploy.env` 与 gateway 侧环境文件（详见脚本头注释）。


#### studio 真实接入检查清单（上线前逐项过，mock 只用于单测与 CI）

| # | 检查项 | 怎么验 | 不满足时的现象 |
|---|---|---|---|
| 1 | ai-gateway 部署变量 `AIG_CALLER_GO_SECRET`（即 `AIG_CALLER_KEYS` 里 `key_id=fusion-go-v1` 的 secret）与本机 `deploy.env` 的 `AI_GATEWAY_SECRET` **完全一致**，`AI_GATEWAY_KEY_ID` 同为 `fusion-go-v1`，scopes 含 `calls`、`catalog` | 后台 `GET /api/v1/studio/admin/sidecar/status` 的 `ai-gateway` 项 ok=true | 每次提交 401 `AUTH_FAILED`（`hmac.invalid_signature` / `hmac.caller_mismatch`），子任务「提交生图失败，点数已退回」 |
| 2 | `POST /v1/catalog/readiness` 生图模型组（通用 `img.gpt-image-2`，具名档 `img.gpt-image-2.6-sunburst` / `img.nano-banana-2.1-dev` / `img.seedream-5.5`，按 `ModelGroups` 配置）带 `operation=image.edit` 均 ready；润色组 `llm.deepseek-flash`（2026-10-31 由 `llm.deepseek-v4-flash` 改名，旧组随同一次 gateway 发布删除）、`vlm.qwen-3.8-flash`（`chat.completion`）ready；反推组 `vlm.deepseek-flash` ready（`check-studio-readiness.sh` 默认已查） | 同上，`detail` 列出未就绪组与 reason（`operation.mismatch` / `deployments.all_balance_excluded` / `runtime.unavailable` …） | 该档模型的子任务失败「生图服务繁忙」；润色失败只降级为原提示词 |
| 3 | 反推 sidecar `GET /api/status` 显示 `llm_configured=true`（`AIG_BASE_URL` + `AIG_SECRET` 只在 sidecar 环境里）、`gateway_ready=true`、`model_group` 为预期组（默认 `vlm.deepseek-flash`）、`allowed_deployments` 为空；gateway 侧已给账号 `deepseek-chat.main` 写入 DeepSeek 官方密钥并登记调用方 `prompt-reverse`（`AIG_CALLER_REVERSE_SECRET`）；`REVERSE_SIDECAR_URL` 指向 sidecar | 状态接口 `sidecar` 项 ok=true、`version` 有值；sidecar 本机 `curl -s http://127.0.0.1:8090/api/status` | 未配置：商拍 / 姿态整批失败「反推服务未配置，点数已退回」；secret 不一致：读图 502「读图返回 401」，analyze 降级为模板 |
| 4 | 媒体服务可达且接了 OSS（本地存储模式没有可访问地址，FetchBytes / 转存都会失败），`MEDIA_SVC_TOKEN` 与 Java、media-svc 一致 | 状态接口 `media` 项 ok=true（`GET /healthz`）；跑一次真实链路看 result_url 能 HEAD 200 | 商拍 / 姿态「图片下载或处理失败」；成功出图后「成品转存失败，点数已退回」 |
| 5 | Java 后台三个 offer 已配：`studio_tryon` / `studio_pose` / `studio_imagegen`（`deploy/sql/ops/studio-offers-2026-10-30.sql` 或后台手填，口径见 `docs/billing/studio-入口码配置指引.md`），并给用户发 `ecom_image_gen` 能力 | `bash install.sh check-billing` 全部通过 | **install.sh 第 6/6 步计费检查中止部署**；线上提交被拒「暂未开通该功能」/「暂时无法计费」 |
| 6 | `STUDIO_INPUT_URL_PREFIXES` 含素材库公有桶域名 | 从素材库选图提交一次 | 「参考图第 n 张 不是本服务上传的地址」 |
| 7 | 迁移 `Go_V1.0.21__studio.sql`、`Go_V1.0.22__admin_studio_permission.sql` 已登记；运营账号有 `studio:view` / `studio:update` | `SELECT version FROM go_schema_history` | 后台 studio 页 2005「没有权限：studio:view」 |
| 8 | 真实链路回归 | `GW=… TOKEN=… ADMIN_TOKEN=… SAMPLE_GARMENT_URLS=… SAMPLE_MODEL_URL=… SAMPLE_POSE_URLS=… bash tests/e2e/e2e-real-studio.sh`（**会产生真实费用**） | 任一用例失败即不上线 |

### 画布 canvas-rpc（脚本部署）

- `canvas-rpc`（画布 `/api/v1/canvas` + 提示词库 `/api/v1/prompt-library`）同样只听 127.0.0.1；接口与状态口径见 `docs/api/api-canvas.md`。
- **`canvas-rpc` 可多实例部署，依赖库里的行租约；扩容无需额外配置**：每个 run 一把租约（`lease_owner` / `lease_until`，`Go_V1.0.25`），提交 / 轮询 / 转存 / 结算同一时刻只由一个实例做；实例崩溃后 3 分钟内由其它实例接管（有 `backend_ref` 的先查上游，没有的按幂等键重提）。Java 兼容工具用的用户 Bearer 缓存仍只在各实例内存里：Java 工具的 run 由收到过该用户请求（带 Bearer）的实例推进，其它实例抢到后发现没有 Bearer 会立即放回，不影响正确性。
- `canvas-rpc` 启动自检：`Timeouts` 各项（秒）须小于对应的 `Billing.Hold`（视频 `VideoMinutes`，其余 `DefaultMinutes`，分钟），否则拒绝启动并打印超限项。轮询器每分钟给排队 / 运行中、已冻结的 gateway run 续期预留单。
- `BILLING_MODE=on` 时 `canvas-rpc` 自带预留单回收器（与 imagegen-rpc / studio-rpc 同一个 `billing.Reclaimer`，release 是 CAS，同时跑安全）：
  gateway 结果未知（`UPSTREAM_UNKNOWN`，后台 `billing_status=review`）的 run **不主动退款**，预留单到期（图片 30 分钟、视频 200 分钟，按 `Billing.Hold`）由回收器**自动退回**；退款 / 确认写库失败的单子同样到期兜底退回。
- 状态口径：run `5-提交中`（已落库、正在冻结点数；进程在此状态崩溃时 5 分钟后由轮询器退回可能已冻结的点数并置失败 `SUBMIT_TIMEOUT`）；`billing_status=unsettled` = 已交付但确认扣费失败（预留单已过期回收 / 已退），日志有 ERROR，需人工对账。
- 同一节点同一时刻只允许一个未结束的 run，由 `afv_canvas_node_run.active_node` 生成列 + 唯一键 `uk_active_node` 保证（`Go_V1.0.25`）。
- Java 兼容工具（放大 / 扩图 / 局部重绘）的取消：用户取消、运营强制失败、超时、所在画布被删时，`canvas-rpc` **通过 Java 现有取消接口 `POST /api/v1/creation/{taskId}/cancel` 真实取消，由 Java 退款**（Go 不退点数；取消时 Java 已成功则用户取消 / 超时 / 画布已删按成功交付）。用户取消用该次请求的 Bearer，其余场景用实例内存里该用户最近的 Bearer（2 小时）；**无有效登录态时退化为停止跟踪**——run 的 `error_reason` 写明「Java 任务未能取消（无有效登录态），可能仍会执行并扣费」，日志有 `WARN … check=canvas.java_cancel_degraded`（含 `run_id` / `user_id` / `java_task`），计数 `canvas_java_cancel_degraded_total{scene,reason}`，运营按 `java_task` 到 Java 后台手动取消。多实例时 Bearer 缓存按实例，处理该 run 的实例没见过该用户请求也会退化。Java 暂时不可达时 run 保持运行中，轮询器重试取消，最多 5 次后按停止跟踪处理。提交时遇暂时性错误、`backend_ref` 没写上的 run：首次发送前已把原始请求存进 `backend_request`（`Go_V1.0.25`），取消 / 强制失败 / 排队超时 / 画布已删时按同一 `Idempotency-Key` 逐字节重放找回 Java 任务号再取消；Java 没收到首次请求时重放会新建任务并冻结点数，随即取消由 Java 退款；找不回（Java 回 1003 同键内容不同）时日志 ERROR + 退化 WARN，带 `idempotency_key`，运营可按它在 Java `afv_creation_task.submit_idempotency_key` 查任务。
- 局部重绘的蒙版：`mask_url` 交给媒体服务拉取（画布服务不直连用户给的地址），`mask_data_url` 先校验尺寸（宽高各 ≤4096）再拉入媒体服务，所以**重绘依赖媒体服务**（`MEDIA_SVC_*` 未配时重绘返回「暂未开放」）。

## 四、数据库迁移

- 迁移脚本在安装包 `sql/` 下，命名 `Go_V<版本>__<描述>.sql`，版本从 `1.0.0` 开始，按版本号顺序执行。
- 每执行一个就登记到 **`go_schema_history`**（版本、文件名、sha256、耗时），与 Java 的 `flyway_schema_history` 分开，两边互不影响。
- 已登记且内容未变 → 跳过；**已登记但文件被改过 → 中止安装**（已发布的迁移不能改，要改请新增版本号）。
- 老库上手工执行过的脚本：库里已经有它建的表 / 列时，登记为 baseline（`execution_ms=0`），不重复执行。
- 不要手工逐个 `mysql < 文件`：那样不会登记。
- 查看记录：`SELECT version, script, installed_on, execution_ms FROM go_schema_history ORDER BY installed_rank;`

安装包 `scripts/` 下另有两份**不会自动执行**的 SQL：`check-billing-online.sql`（只读，给 Java 在线上库核对计费配置）、`fix-billing-offers-2026-10-16.sql`（一次性计费配置补齐，执行前先评审）。

## 五、验证

**计费配置**（安装时已自动跑过，可随时复查，只读）：

```bash
bash /opt/aigc-local-life/install.sh check-billing
```

**业务接口**：用 App 登录拿到的令牌（`Authorization: Bearer <令牌>`）：

```bash
TOKEN=<App 登录返回的 token>
curl -s -H "Authorization: Bearer $TOKEN" https://<PUBLIC_DOMAIN>/api/v1/local-life/copywriting/options | head -c 200
curl -s -H "Authorization: Bearer $TOKEN" https://<PUBLIC_DOMAIN>/api/v1/points/balance | head -c 200   # 走 Java，验证令牌两边通用
```

都返回 `{"code":0,...}` 即正常。第二条不通，多半是会话 Redis 的地址、口令或库号与 Java 不一致。

**入口路由**（从外网执行）：`/api/v1/admin/...`、`/swagger-ui/...`、`/api/v1/local-life/admin/...` 都应返回 404。

**接口文档（Swagger）**：默认关闭。联调需要时在 `deploy.env` 设 `SWAGGER_ENABLED=1` 后重装，经 ssh 隧道在本机访问 `http://127.0.0.1:18888/swagger`、`http://127.0.0.1:18889/swagger`。前端对接说明在安装目录 `docs/api/` 下（`api-app.md`、`api-voiceover.md`）。

## 六、日常操作

```bash
bash /opt/aigc-local-life/install.sh status            # 各服务状态
bash /opt/aigc-local-life/install.sh restart           # 重启全部
bash /opt/aigc-local-life/install.sh stop              # 停止全部
bash /opt/aigc-local-life/install.sh logs gateway-api  # 看日志（Ctrl+C 退出）
bash /opt/aigc-local-life/install.sh check-billing     # 计费配置检查（只读）
bash /opt/aigc-local-life/install.sh uninstall         # 停止并注销服务（不删库、不删目录）
```

- 服务名：`gateway-api` `admin-api` `copywriting-rpc` `digitalhuman-rpc` `note-rpc` `imagegen-rpc`。
- 已注册为系统服务：服务器重启后自动启动，进程挂了 3 秒后自动拉起（`systemctl status aigc-local-life-gateway-api` 也能看）。
- 日志在 `/opt/aigc-local-life/logs/`：`<服务>.out` 是启动输出（如 `gateway-api.out`）；业务日志在去掉 `-api` / `-rpc` 后缀的目录下（如 `gateway/`、`copywriting/`），按天切分，保留 `LOG_KEEP_DAYS` 天（默认 7）。
- 配置在 `/opt/aigc-local-life/etc/`，权限 600（含数据库口令、令牌密钥）。
- 重启不丢任务：生成中的记录都在库里，服务起来后会接着查结果；冻结超时的点数由回收任务自动退回。

## 七、升级与回滚

**升级**：开发机 `make package VERSION=<新版本>` → 上传解压（可覆盖 `/root/aigc-local-life`）→

```bash
cd /root/aigc-local-life && bash install.sh
```

- 配置自动沿用：新包里没有 `deploy.env` 时，用 `/opt/aigc-local-life/deploy.env`。
- 新增的迁移自动执行，已登记的跳过；会先停旧进程再启新的，中间有几秒不可用。
- 含迁移的版本建议设 `BACKUP_BEFORE_MIGRATE=1`。
- 早期开发版遗留的 mock 服务会在安装时自动注销。

**回滚**：
- 只回滚程序：重新解压上一版安装包，`bash install.sh`。本项目的迁移只新增表 / 列 / 数据，旧程序可以在新表结构上运行。
- 连数据库一起回滚：停服务 → 用迁移前的备份恢复（`gunzip < /opt/aigc-local-life/backup/<库名>-<时间>.sql.gz | mysql ...`）→ 装上一版。**没开备份就没有这条路**。

## 八、运营数据（否则 App 里对应列表是空的）

公共数字人、海报 / 长图模板、预置动作视频、数字人口播预设背景：在现有后台「模板」里维护，或用安装包里的 `seed` 工具按 CSV 批量导入：

```bash
/opt/aigc-local-life/bin/seed -dsn 'root:密码@tcp(127.0.0.1:3306)/fusion_aigc' -kind voiceoverBackground -csv bg.csv -dry  # 先看要写什么
/opt/aigc-local-life/bin/seed -dsn 'root:密码@tcp(127.0.0.1:3306)/fusion_aigc' -kind voiceoverBackground -csv bg.csv      # 真写
```

`-kind`：`publicModel` / `posterTemplate` / `longImageTemplate` / `actionVideo` / `voiceoverBackground`，CSV 列名见 `seed -h`。

> ⚠️ `install.sh seed-templates` 写的是**占位素材**（公有桶里的示例文件），只用于联调环境，**不要在正式库执行**。

## 九、常见问题

| 现象 | 处理 |
|---|---|
| `正式版只支持 AUTH_MODE=token` / `正式版只允许 BILLING_MODE=on` | 正式版不接受开发配置，按提示改 `deploy.env` |
| `必须填 AIGC_ENDPOINT` | 填 `AIGC_ENDPOINT`，或设 `JAVA_ENV_FILE` 指向 Java 的 `.env` |
| `PUBLIC_DOMAIN 必填` | 填前端访问的域名 |
| `会话 Redis 未知` | 在 Java 的 `.env` 里要有 `REDIS_HOST`（设 `JAVA_ENV_FILE`），或在 `deploy.env` 显式设 `SESSION_REDIS_ADDR` |
| `SESSION_REDIS_DB=… 与 Java 的 REDIS_DB=… 不一致` | 删掉 `deploy.env` 里的 `SESSION_REDIS_DB`，让它从 Java 读取 |
| `Java 的 REDIS_HOST=… 看起来是容器服务名` | 在 `deploy.env` 显式设 `SESSION_REDIS_ADDR`（如 `127.0.0.1:6379`） |
| `BILLING_MODE=on 但计费配置有问题` | 按列出的入口补齐计费配置，`install.sh check-billing` 全部通过后再装 |
| `… 已执行过，但文件内容被改了` | 已发布的迁移被改动。恢复原文件，改动放进新的版本号 |
| `端口 xxx 已被占用` | 在 `deploy.env` 换一个端口，重新 `bash install.sh` |
| `连不上数据库` | 检查 `DB_*`；MySQL 在别的机器上时，放开该用户对这台服务器 IP 的访问 |
| `找不到 mysql 客户端` | `deploy.env` 填 `MYSQL_BIN=/www/server/mysql/bin/mysql` |
| `afv_system_config 缺少 …` | 连错库了，或现有系统没配 OSS / 文案模型；本项目复用这些配置 |
| `缺少私有桶配置` | 执行面的产物在私有桶里，必须能转存；在现有后台系统设置里补齐 `storage_private_*` |
| `缺少配音模型配置`（黄色） | 只影响数字人配音和音色试听。在 `afv_ai_model` 加一行 `code=qwen-audio-3.1-tts-flash`、`model_type=4`、`ai_provider_id=百炼那条`，再装一次会自动补 `ai_tts_model_id` |
| 自检某项 ✗ | `bash install.sh logs <服务名>` 看报错 |
| App 登录后调 Java 接口 401 | 会话 Redis 地址 / 口令 / 库号与 Java 不一致 |
| 外网访问不了 | 确认 nginx 已加载 `aigc-local-life.conf`、证书已配；网关端口不对外是正常的 |

## 十、上线检查清单

- [ ] 测试机上从空库完整装过一遍：迁移、自检、App 登录、Go 与 Java 令牌互通、文案计费扣点 / 退回
- [ ] `deploy.env`：`PUBLIC_DOMAIN`、`JAVA_UPSTREAM`、`JAVA_ENV_FILE`（或 `AIGC_ENDPOINT`）、`UNION_BASE_URL` 已填；安装输出的「配置来源」里 Redis 与会话参数都来自 `Java .env`
- [ ] 会话参数与 Java 生产一致（上限、令牌时长、Redis 库号）
- [ ] 首次上线已设 `BACKUP_BEFORE_MIGRATE=1`，并确认备份盘空间
- [ ] `install.sh check-billing` 全部通过
- [ ] nginx 已加载、证书已配；外网访问 `/api/v1/admin`、`/swagger-ui`、`/api/v1/local-life/admin` 都是 404
- [ ] 安全组只放行 80 / 443
- [ ] 运营数据（数字人、模板、动作视频、口播背景）已用真实素材配置，音色试听已生成
