# aigc-local-life 正式部署手册（docker compose）

**正式环境用这种方式部署。** 一键脚本（[`../server/README.md`](../server/README.md)）是没有 Docker 的服务器上的备选方式，两者的校验规则、迁移、配置生成完全是同一套代码（`install.sh` 的 `prepare` 子命令）。

本文档讲**单镜像**（`AIGC_IMAGE`，一个镜像装全部服务）。另有一套**每服务独立镜像**的发布链路（私有仓库 + webhook，`Makefile_x` + `docker-compose.registry.yml`），见 [`README.registry.md`](README.registry.md)。

## 部署拓扑

```
            公网 80/443
                │
        宿主机 nginx（加载 ./nginx/aigc-local-life.conf）
        ├─ /api/v1/local-life、/api/v1/studio ──► 127.0.0.1:18888  gateway-api ─┐
        ├─ /api/v1/（其余）  ──► 127.0.0.1:18080  Java          │  rpc 直连 127.0.0.1:19201–19207
        └─ /api/v1/admin、/api/v1/local-life/admin、Swagger、其它 ──► 404  │  copywriting / digitalhuman / note / imagegen / studio
                                                                  │
   127.0.0.1:18889 admin-api（只在本机，ssh 隧道访问）            │
                                                                  ▼
   阿里云 RDS（fusion_aigc，与 Java 共用）     宿主机 Redis 127.0.0.1:6379（会话库 10，与 Java 共用）
```

- 所有容器用 **host 网络**，服务只监听 `127.0.0.1`。公网只经宿主机 nginx，安全组只放行 80 / 443。
- MySQL 是阿里云 RDS，按内网地址连接（服务器 IP 要在 RDS 白名单里）；Redis 在宿主机；Java 在 `127.0.0.1:18080`。
- 一个镜像（`aigc-local-life`）包含 6 个服务和部署工具，正式镜像**不含 mock**。

## 启动顺序

`docker compose up -d` 时：

1. **`prepare`**（一次性）：检查数据库 → 执行迁移（登记到 `go_schema_history`）→ 生成服务配置到 `./etc/`（只有服务用户可读）→ 渲染 `./nginx/aigc-local-life.conf` → 计费配置自检。**任何一步失败都以非 0 退出，服务不会启动。**
2. 7 个 rpc（含 studio-rpc、assetlib-rpc、canvas-rpc）和反推 sidecar `prompt-reverse`：`prepare` 成功退出后启动，健康检查是端口可连。
3. `gateway-api`、`admin-api`：7 个 rpc（copywriting / digitalhuman / note / imagegen / studio / assetlib / canvas）都健康后启动，健康检查是 HTTP 能应答（未带令牌返回 401 属正常）。

正式环境的硬规则在 `prepare` 里强制检查，没有放行开关：`AUTH_MODE=token`、`BILLING_MODE=on`、执行面地址（`AIGC_ENDPOINT` 或 `JAVA_ENV_FILE`）、`PUBLIC_DOMAIN`、会话 Redis（来自 `JAVA_ENV_FILE` 或显式填写）。JWT 密钥不需要。

---

## 一、构建与推送镜像（开发机 / CI）

```bash
make image IMAGE=<镜像仓库>/aigc-local-life VERSION=v1.0.0 PUSH=1
```

- 固定构建 `linux/amd64`（线上服务器是 x86_64；在 Apple 芯片的机器上构建也会产出 amd64）。
- 推送后记下 digest，部署时 `AIGC_IMAGE` 填 `<镜像仓库>/aigc-local-life:v1.0.0@sha256:…`：版本不可变，回滚就是换回上一版的 digest。
- 服务器上**不构建镜像**，只拉取。

## 二、首次部署（服务器）

**1. 放置部署文件**

在服务器上建一个目录（如 `/opt/aigc-local-life`），放入本目录的 `docker-compose.yml` 和 `.env.example`（不需要源码）：

```bash
mkdir -p /opt/aigc-local-life && cd /opt/aigc-local-life
# 拷入 docker-compose.yml、.env.example
cp .env.example .env && chmod 600 .env
```

**2. 填写 `.env`**

| 变量 | 填什么 |
|---|---|
| `AIGC_IMAGE` | 镜像地址，带 digest |
| `DB_HOST` `DB_PORT` `DB_USER` `DB_PASS` | 阿里云 RDS 的**内网**地址与账号（`DB_NAME=fusion_aigc`） |
| `MYSQL_OPTS` | 账号是 `caching_sha2_password` 且不走 TLS 时可能需要 `--ssl`；默认留空 |
| `PUBLIC_DOMAIN` | `aigc.pengqiwl.com` |
| `JAVA_UPSTREAM` | `127.0.0.1:18080` |
| `JAVA_ENV_FILE` | 宿主机上 Java 服务 `.env` 的**绝对路径**（只读挂进 `prepare`）：执行面地址与会话参数从这里读，保证和 Java 一致 |
  > **最小权限建议**：`prepare` 只需要下面几个键，不必把 Java 的整个 `.env`（含所有其它密钥）挂进去。在宿主机上抽出一份只含这些键的文件，`JAVA_ENV_FILE` 指向它：
  > ```bash
  > grep -E '^(AIGC_BASE_URL|REDIS_HOST|REDIS_PORT|REDIS_PASSWORD|REDIS_DB|TOKEN_ACCESS_HOURS|TOKEN_REFRESH_DAYS|APP_SECURITY_TOKEN_MAX_CONCURRENT_SESSIONS)=' /path/to/java/.env > /opt/aigc-local-life/java-session.env && chmod 600 /opt/aigc-local-life/java-session.env
  > ```
  > Java 的 `.env` 有改动时，重跑这条命令再 `docker compose up -d` 即可。
| `AIGC_ENDPOINT` | 不填 `JAVA_ENV_FILE` 时必填，与 Java 的 `AIGC_BASE_URL` 相同 |
| `SESSION_REDIS_ADDR` `SESSION_REDIS_PASS` `SESSION_REDIS_DB` | 默认从 `JAVA_ENV_FILE` 读（`REDIS_HOST:REDIS_PORT`、`REDIS_PASSWORD`、`REDIS_DB`），与 Java 一致；只在需要覆盖时才填 |
| `UNION_BASE_URL` | 联盟用户服务地址，登录用（与 Java 相同，只走 HTTP；不直连联盟库或联盟 Redis） |

与 Java 保持一致的会话参数（`SESSION_MAX_CONCURRENT`、`TOKEN_ACCESS_HOURS`、`TOKEN_REFRESH_DAYS`）留空时从 `JAVA_ENV_FILE` 读，读不到用 Java 的默认值 0 / 2 / 7。其余可选项见 `.env.example` 里的注释。

**3. 启动**

```bash
docker compose pull
docker compose up -d
docker compose logs -f prepare      # 看准备过程，最后一行是「准备完成」
docker compose ps                   # prepare 为 Exited (0)，6 个服务为 healthy
```

`prepare` 失败时按它打印的原因修正 `.env` 或数据库，然后重新 `docker compose up -d`。

**4. 配置宿主机 nginx**

`prepare` 已把站点配置渲染到 `./nginx/aigc-local-life.conf`。在宝塔站点 `PUBLIC_DOMAIN` 的配置里加入它的 `upstream` 与 `server` 内容（或 `include`），配好证书，`nginx -t` 通过后重载。路由：

| 路径 | 去向 |
|---|---|
| `/api/v1/local-life` | Go 网关 `127.0.0.1:18888` |
| `/api/v1/local-life/admin` | Go 后台（与 Java 后台共用登录令牌，按 RBAC 校验权限） |
| `/api/v1/plaza/`、`/api/v1/replicate/` | Go 网关（智能体模板页、视频做同款；结尾斜杠不可省，避免吞掉 Java 的 `/api/v1/plaza-templates`） |
| `/api/v1/plaza/admin/`、`/api/v1/replicate/admin/` | Go 后台 |
| `/internal/` | 404（服务间入口只给内网 Java 直连网关端口） |
| `/api/v1/admin` | 404（Java 运营后台不走公网；确需公网访问时删掉模板里这一段） |
| `/api/v1/`（其余） | Java `JAVA_UPSTREAM` |
| `/swagger-ui`、`/v3/api-docs`、`/actuator`、其它 | 404 |

**5. 生成系统音色试听**（只需一次；脚本在镜像里，用现有后台账号鉴权，口令交互输入、不进命令行历史）：

```bash
read -rp '后台账号: ' ADMIN_USER; read -rsp '口令: ' ADMIN_PASS; echo; export ADMIN_USER ADMIN_PASS
docker compose run --rm -e ADMIN=http://127.0.0.1:18889/api/v1/local-life/admin -e ADMIN_USER -e ADMIN_PASS \
  prepare bash /app/pkg/scripts/gen-voice-previews.sh
```

## 三、验证

```bash
docker compose ps                                    # 全部 healthy
docker compose run --rm prepare bash /app/pkg/install.sh check-billing   # 计费配置复查（只读）
TOKEN=<App 登录返回的 token>
curl -s -H "Authorization: Bearer $TOKEN" https://<PUBLIC_DOMAIN>/api/v1/local-life/copywriting/options | head -c 200
curl -s -H "Authorization: Bearer $TOKEN" https://<PUBLIC_DOMAIN>/api/v1/points/balance | head -c 200   # 走 Java，验证令牌两边通用
```

外网访问 `/api/v1/admin/...`（Java 后台）、`/swagger-ui/...` 都应返回 404；`/api/v1/local-life/admin/...` 用后台账号的令牌访问，无令牌返回 401。

## 三（补）、studio（商拍换装 / 姿态复刻 / 图片生成）

- **studio-rpc**：端口 `STUDIO_PORT`（默认 19205），与其它 rpc 同镜像同配置生成流程；依赖 ai-gateway 与反推 sidecar。`AI_GATEWAY_URL` 留空时 studio 提交返回「暂未开放」，不影响其它功能。
- **prompt-reverse（Python sidecar）**：独立镜像，`docker build -t aigc-prompt-reverse:<版本> tools/prompt-reverse`，`.env` 的 `REVERSE_IMAGE` 填它。**只给 studio-rpc 用**：host 网络下只绑 `127.0.0.1:${REVERSE_PORT:-8090}`，不映射宿主端口、不经 nginx，无鉴权，靠只听本机隔离；`REVERSE_SIDECAR_URL` 留空即默认 `http://127.0.0.1:8090`。
- **sidecar 经 ai-gateway 调 DeepSeek**（调用方 `prompt-reverse`，模型组默认 `vlm.deepseek-flash`：官方 `deepseek-chat.vl.deepseek-flash` 优先、百炼 `bailian-chat.vl.deepseek-flash` 自动切换备用；排障用 `REVERSE_ALLOWED_DEPLOYMENTS=deepseek-chat.vl.deepseek-flash` 或 `bailian-chat.vl.deepseek-flash` 钉单一渠道，sidecar 写进 `constraints.allowed_deployments`，排障完清空），不再直连 api.deepseek.com；DeepSeek 官方密钥只在 gateway 管理端配置（账号 `deepseek-chat.main`）。
- **`REVERSE_AIG_SECRET` 只在 `.env` 里**（须与 ai-gateway 侧 `AIG_CALLER_REVERSE_SECRET` 一致），compose 把它映射成 `AIG_SECRET` 注入 `prompt-reverse` 容器（不写镜像、不进 `etc/`，运行中的 rpc / 网关容器读不到）。注意：一次性的 `prepare` 容器用 `env_file: [.env]` 加载整个 `.env`，所以它也能读到这个值（它不会把该值写进 `etc/`，但需要时请当作可见范围包含 `prepare`）；不要提交 `.env`。`REVERSE_AIG_BASE_URL` 留空复用 `AI_GATEWAY_URL`，`REVERSE_AIG_KEY_ID` 默认 `prompt-reverse-v1`。没配地址或 secret 时 sidecar 的 `/api/status` 里 `llm_configured=false`，批次会在反推阶段失败并退款；配好后看 `gateway_ready`（readiness 缓存 60s）。
- **自动校色（colorfix-solver，可选）**：商拍成品的「自动校色」按钮背后的求解服务（`tools/colorfix-solver`，Python，只返回五个校色滑杆值、不出图，不进 ai-gateway）。**默认关闭**：`.env` 里 `COLORFIX_SOLVER_URL` 留空时 `POST /api/v1/studio/colorfix/auto` 回 `7003 · COLORFIX_AUTO_DISABLED`、`GET /api/v1/studio/models` 的 `colorfix_auto=false`，前端不显示按钮，其它功能不受影响。开启步骤：① `docker build -t aigc-colorfix-solver:<版本> tools/colorfix-solver`（registry 部署用 `aigc-local-life-colorfix-solver`），`COLORFIX_IMAGE` 填它；② `.env` 填 `COLORFIX_SOLVER_TOKEN`（`openssl rand -hex 24`，>= 16 位；compose 映射成求解端的 `COLORFIX_TOKEN`，`prepare` 同时把它写进 studio 的 `ColorFixSolver.Token`）；③ **`COLORFIX_FETCH_ALLOW_HOSTS` 填 OSS / CDN 域名**（成品私有桶签名地址、素材库公有桶、服装参考图所在存储，缺任何一个对应的单品都会 502 → 接口回 `COLORFIX_AUTO_UNAVAILABLE`）；求解端拒绝抓取内网 / 回环地址，只有 OSS 走内网域名解析到私网时才加 `COLORFIX_ALLOW_PRIVATE_FETCH=1`（仅内网部署）；④ `docker compose --profile colorfix up -d colorfix-solver`，`.env` 填 `COLORFIX_SOLVER_URL=http://127.0.0.1:18095` 后 `docker compose run --rm prepare`（重新生成 `etc/`）并重启 studio-rpc 与 gateway-api。验证：本机 `curl -s http://127.0.0.1:18095/healthz` 返回 `{"status":"ok",...}`；用一个成功的商拍任务调接口，日志里 `studio colorfix auto usage` 是成功，`check=colorfix_auto_solve` 带 `kind / status / code` 是求解失败（401 = token 不一致，403 = 域名没放行，502 = 求解端下载失败）。限频在 gateway-api 的 `Studio.ColorFixAutoPerMinute / ColorFixAutoPerDay`（默认 6 / 200），求解并发在 studio 的 `ColorFixSolver.MaxConcurrent`（默认 4，超出 `COLORFIX_AUTO_BUSY`）。
- **nginx**：`/api/v1/studio/admin`（→ admin-api）与 `/api/v1/studio`（→ gateway-api）两条 location 已在 `nginx/aigc-local-life.conf.tmpl` 里，`prepare` 渲染时自带，不用手改。
- 验证：`curl -s http://127.0.0.1:8090/api/status`（在服务器本机）应返回 `service=prompt-reverse`、`llm_configured=true`、`model_group`、`gateway_ready=true`；后台「studio / sidecar 状态」页能看到 sidecar、ai-gateway、媒体服务三项依赖。

## 四、数据库迁移

- 迁移脚本打在镜像里（`Go_V<版本>__<描述>.sql`，从 1.0.0 开始），由 `prepare` 按**版本号数字顺序**执行。
- 每个版本登记到 `go_schema_history`（与 Java 的 `flyway_schema_history` 分开）；已登记跳过，**已登记但内容被改过会中止**。
- 老库上手工执行过的脚本按「库里已有对应结构」登记为 baseline，不重复执行。
- 迁移前备份可选：`BACKUP_BEFORE_MIGRATE=1` 时，**只在有待执行的迁移时**整库 `mysqldump` 到 `./backup/`（没有新迁移的 `up -d` 不会备份）。RDS 大库建议改用 RDS 控制台的手动备份（更快、不占服务器磁盘），升级前做一次。

## 五、日常运维

```bash
docker compose ps                         # 状态与健康
docker compose logs -f --tail=200 gateway-api
docker compose restart gateway-api        # 重启单个服务（不会重跑 prepare）
docker compose down                       # 停止并删除容器（数据都在 RDS / Redis，不受影响）
```

- 日志写标准输出（JSON），由 Docker 轮转：每个服务最多 5 × 50MB。
- 每个服务内存上限 `AIGC_MEM_LIMIT`（默认 1g）；进程退出会自动拉起（`restart: unless-stopped`）。
- 配置在 `./etc/`，权限只有服务用户（uid 10001）可读。改配置就改 `.env`，然后 `docker compose up -d --force-recreate`：`prepare` 重新生成配置，服务容器也随之重建（只 `up -d` 的话服务还在用内存里的旧配置）。
- 重启不丢任务：生成中的记录在库里，服务起来后接着查结果；冻结超时的点数由回收任务自动退回。

## 六、升级与回滚

**升级**：

```bash
# 修改 .env 里的 AIGC_IMAGE 为新版本的 digest
docker compose pull
docker compose up -d        # 镜像变了，全部容器重建：prepare 先执行新版本的迁移，服务随后重建（有几秒中断）
```

**回滚**：把 `AIGC_IMAGE` 改回上一版的 digest，`docker compose up -d`。本项目的迁移只新增表 / 列 / 数据，旧版本可以在新表结构上运行；需要连数据一起回退时，用升级前的 RDS 备份恢复。

## 七、本地联调（不用于正式环境）

```bash
# .env 里：AIGC_IMAGE=aigc-local-life:dev，AIGC_ENDPOINT=http://127.0.0.1:5055，
#          UNION_BASE_URL=http://127.0.0.1:5056，LINK_PARSE_URL=http://127.0.0.1:5058
docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
```

`docker-compose.dev.yml` 在本地构建含 mock 的镜像，并起 mock 执行面、联盟、链接解析，以及 studio 联调用的 `mock-reverse`（5057，替代反推 sidecar）和 `mock-aigateway`（5059，替代 ai-gateway）。联调 studio 时 `.env` 里设 `REVERSE_SIDECAR_URL=http://127.0.0.1:5057`、`AI_GATEWAY_URL=http://127.0.0.1:5059`、`AI_GATEWAY_SECRET=test-secret`（与 mock 的 `MOCK_AIG_SECRET` 一致）；`MOCK_REVERSE_FAIL=analyze` 可让 mock 反推返回 502 以测失败退款。端到端脚本：`tests/e2e/e2e-studio.sh`。正式环境的校验规则同样生效。

宿主机上直接 `make run-all` 开发时，只需要 etcd：`docker compose -f deploy/docker/docker-compose.etcd.yml up -d`。

## 八、常见问题

| 现象 | 处理 |
|---|---|
| `prepare` 退出非 0 | `docker compose logs prepare`，按 ✗ 那一行的原因修正后重新 `up -d` |
| `连不上数据库` | RDS 白名单是否包含服务器 IP、`DB_*` 是否是内网地址；认证报错时试 `MYSQL_OPTS=--ssl` |
| `SESSION_REDIS_ADDR 必填` | 按 `.env.example` 填写 |
| `计费配置有问题` | 补齐计费配置；`check-billing` 全部通过后再 `up -d` |
| 口令里有 `$`，连不上数据库 / Redis | `.env` 里用单引号包住该值（compose 会替换 `$xxx`） |
| `JAVA_ENV_FILE 指向的是目录而不是文件` | `.env` 里的路径在宿主机上不存在，Docker 挂载时建成了空目录；改成正确的绝对路径，并删掉那个空目录 |
| `… 已执行过，但文件内容被改了` | 已发布的迁移被改动，属于镜像问题：改动必须放进新的版本号 |
| 服务一直 starting / unhealthy | `docker compose logs <服务>`；常见是端口被宿主机上其它进程占用 |
| App 登录后调 Java 接口 401 | 会话 Redis 地址 / 口令 / 库号与 Java 不一致 |
