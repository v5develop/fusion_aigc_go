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
- 所有进程只监听 `127.0.0.1`：网关 18888、后台 18889、rpc 19201–19204（**避开了宝塔面板的 8888**）。端口被占用时脚本停下来提示，不抢端口。
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

公网只开 80 / 443。网关、后台、各 rpc **都只监听 127.0.0.1**，不要在云安全组或宝塔「安全」里放行 18888、18889、19201–19204。

安装后 `/opt/aigc-local-life/nginx/aigc-local-life.conf` 已按 `PUBLIC_DOMAIN`、`GATEWAY_PORT`、`ADMIN_PORT`、`JAVA_UPSTREAM` 渲染好，路由如下：

| 路径 | 去向 | 说明 |
|---|---|---|
| `/api/v1/local-life` | Go 网关 | 本地生活全部 App 接口 |
| `/api/v1/local-life/admin` | Go 后台（与 Java 后台共用登录令牌，按 RBAC 校验权限） |
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
