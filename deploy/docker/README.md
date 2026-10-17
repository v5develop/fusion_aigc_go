# aigc-local-life 正式部署手册（docker compose）

**正式环境用这种方式部署。** 一键脚本（[`../server/README.md`](../server/README.md)）是没有 Docker 的服务器上的备选方式，两者的校验规则、迁移、配置生成完全是同一套代码（`install.sh` 的 `prepare` 子命令）。

## 部署拓扑

```
            公网 80/443
                │
        宿主机 nginx（加载 ./nginx/aigc-local-life.conf）
        ├─ /api/v1/local-life ──► 127.0.0.1:18888  gateway-api ─┐
        ├─ /api/v1/（其余）  ──► 127.0.0.1:18080  Java          │  rpc 直连 127.0.0.1:19201–19204
        └─ /api/v1/admin、/api/v1/local-life/admin、Swagger、其它 ──► 404  │  copywriting / digitalhuman / note / imagegen
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
2. 4 个 rpc：`prepare` 成功退出后启动，健康检查是端口可连。
3. `gateway-api`、`admin-api`：4 个 rpc 都健康后启动，健康检查是 HTTP 能应答（未带令牌返回 401 属正常）。

正式环境的硬规则在 `prepare` 里强制检查，没有放行开关：`AUTH_MODE=token`、`BILLING_MODE=on`、执行面地址、`PUBLIC_DOMAIN`、`SESSION_REDIS_ADDR`、两个 JWT 密钥都必须有。

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
| `AIGC_ENDPOINT` | 不填 `JAVA_ENV_FILE` 时必填，与 Java 的 `AIGC_BASE_URL` 相同 |
| `SESSION_REDIS_ADDR` `SESSION_REDIS_PASS` | 宿主机 Redis：`127.0.0.1:6379` 与它的口令 |
| `SESSION_REDIS_DB` | 与 Java 的 `REDIS_DB` 一致（线上 10）。**这一项不会从 `JAVA_ENV_FILE` 读，需手工对齐** |
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

`docker-compose.dev.yml` 在本地构建含 mock 的镜像，并起 mock 执行面、联盟、链接解析。正式环境的校验规则同样生效。

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
