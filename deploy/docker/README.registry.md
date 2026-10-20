# aigc-local-life 线上部署（每服务独立镜像 + 私有仓库 + webhook 发布）

本文档讲**每服务独立镜像**这条发布链路：`Makefile_x` + `deploy/docker/_Dockerfile.svc`（服务）+ `_Dockerfile.ops`（prepare）。

单镜像方式见 [README.md](README.md)。两条链路的**拓扑、prepare 流程、配置生成、迁移完全相同**，差别只有镜像粒度与发布动作：

| | 单镜像（README.md） | 每服务独立镜像（本文档） |
|---|---|---|
| 镜像 | 1 个 `aigc-local-life`（6 个服务 + 工具全在一起） | 7 个：6 个服务各一个 + `aigc-local-life-ops` |
| 构建推送 | 主 Makefile 的 `make image ... PUSH=1` | `make -f Makefile_x push-all` |
| 服务器 `.env` | `AIGC_IMAGE`（建议带 digest） | `AIGC_REGISTRY` + `AIGC_TAG` |
| 服务器 compose | `docker-compose.yml` | `docker-compose.registry.yml` |
| 发布触发 | 手工改 `AIGC_IMAGE` 后 `up -d` | `make -f Makefile_x deploy`（推送 + webhook） |
| 回滚 | 换回旧 digest | `.env` 换回旧 `AIGC_TAG` |
| 服务器需改动 | — | 多一步：确认能拉私有仓库（`docker login` 或 insecure-registries） |

## 镜像清单

| 镜像 | 对应服务 | 说明 |
|---|---|---|
| `aigc-local-life-copywriting-rpc` | `app/copywriting/rpc` | 文案生成 |
| `aigc-local-life-digitalhuman-rpc` | `app/digitalhuman/rpc` | 数字人 |
| `aigc-local-life-note-rpc` | `app/note/rpc` | 图文笔记 |
| `aigc-local-life-imagegen-rpc` | `app/imagegen/rpc` | 视频/图片生成 |
| `aigc-local-life-gateway-api` | `app/gateway/api` | App 网关（127.0.0.1:18888） |
| `aigc-local-life-admin-api` | `app/admin/api` | 运营后台（127.0.0.1:18889） |
| `aigc-local-life-ops` | `tools/ops/*` | prepare 一次性准备 + 运维工具（迁移、生成配置、渲染 nginx、计费自检） |

服务镜像只装一个二进制，配置在运行时由 ops 镜像生成到挂载的 `/app/etc`；镜像自带启动命令 `/app/<服务> -f /app/etc/<服务>.yaml`，非 root（uid 10001）运行。服务镜像运行时是 alpine（自带 `curl`、`bash`，compose 的健康检查要用）。ops 镜像运行时是 debian：RDS 账号用 MySQL 8 默认的 `caching_sha2_password`，MariaDB 客户端没有该插件，必须用官方 MySQL 客户端，而它是 glibc 二进制、musl 的 alpine 跑不了。实测体积：`aigc-local-life-note-rpc` 47MB、`aigc-local-life-ops` 46MB（其余服务镜像未实测，同量级）。

---

## 一、本地构建与推送（开发机 / CI）

```bash
make -f Makefile_x login                      # 登录私有仓库
make -f Makefile_x build-all                  # 构建全部 7 个镜像到本机（--load）
make -f Makefile_x note-rpc                   # 只构建某一个：服务名即目标名
make -f Makefile_x push-all                   # 构建并推送全部
make -f Makefile_x deploy TAG=v1.0.1          # 推送全部 + 触发部署 hook
make -f Makefile_x clean                      # 清理本机上带仓库前缀的镜像
```

常用覆盖：`REGISTRY=<仓库>`、`TAG=v1.0.1`、`PLATFORM=linux/amd64`、`BUILDER=orbstack`、`HOOK_PARAM=<部署单元>`。

构建环境要点（都是实际踩过的）：

- **必须在 Apple 芯片的 macOS 上构建**，固定 `--platform linux/amd64`（线上是 x86_64）；Dockerfile 内已强制 `GOARCH=amd64`。
- `BUILDER` 默认 `orbstack`（走 OrbStack 的 dockerd，镜像加速与 insecure-registries 生效）；换 Docker Desktop 用 `BUILDER=default`。
- **不要删掉 Dockerfile 里的 `GOPROXY=https://goproxy.cn,direct`**：容器内默认的 `proxy.golang.org` 在国内拉不动，`go mod download` 会直接失败。
- daemon 镜像源（daocloud）偶发 502 时，buildx 不会自动降级：先 `docker pull golang:1.26-alpine` 预热基础镜像再构建。
- 发布时显式给 `TAG`：默认值取 `git describe --dirty`，工作区不干净时 tag 会带 `-dirty`，别拿它上线。

## 二、服务器一次性准备

服务器**不构建镜像**，只拉取。

**1. 放部署文件**（目录建议 `/opt/aigc-local-life`，与一键脚本部署保持一致）

```bash
mkdir -p /opt/aigc-local-life && cd /opt/aigc-local-life
# 从仓库拷这两个文件（服务器上不需要源码）
cp <仓库>/deploy/docker/docker-compose.registry.yml .
cp <仓库>/deploy/docker/.env.example .env && chmod 600 .env
```

**2. 填 `.env`**：原有变量（数据库、Redis、JWT、执行面等）照 `.env.example` 注释填，本方式额外加两项：

```
AIGC_REGISTRY=uniontest.pengqiwl.com:5000
AIGC_TAG=v1.0.0
```

**2.1 共享媒体服务（aigc-media）**：本栈不含媒体服务，它由 **Java 栈**（fusion-aigc 的 compose）编排，两栈同机、都是 host 网络。prepare 会用 `MEDIA_SVC_TOKEN` 校验这项配置（空则 `prepare` 直接失败，因为配错时表现为「封面静默消失」，线上很难归因）：

```
MEDIA_SVC_TOKEN=            # 留空则从 JAVA_ENV_FILE 里 Java 的 MEDIA_SERVICE_TOKEN 读（推荐，单一真相源）
MEDIA_SVC_BASE_URL=http://127.0.0.1:18090
MEDIA_NACOS_ADDR=127.0.0.1:8848   # 填则按服务名 aigc-media 订阅实例；留空只用静态地址
MEDIA_NACOS_USERNAME=       # r-nacos 开了 OpenAPI 鉴权时必填，与 Java 栈同账号
MEDIA_NACOS_PASSWORD=
```

媒体服务不可达只告警不拦（两栈独立发布，Java 栈可能还没起），但此时 imagegen 的封面/水印会失败且**不报错**。

**3. 确认服务器能拉私有仓库**

```bash
docker login uniontest.pengqiwl.com:5000        # 若仓库需要认证
# 或在 /etc/docker/daemon.json 配 insecure-registries 后 systemctl restart docker
```

**4. 首次手工拉起**（先不依赖 hook，把流程跑通）

```bash
docker compose -f docker-compose.registry.yml pull
docker compose -f docker-compose.registry.yml up -d
docker compose -f docker-compose.registry.yml logs -f prepare    # 最后一行「准备完成」
docker compose -f docker-compose.registry.yml ps                 # prepare Exited(0)，6 个服务 healthy
```

prepare 会检查数据库、执行迁移（登记到 `go_schema_history`）、生成配置到 `./etc/`、渲染 `./nginx/aigc-local-life.conf`、做计费自检。**任何一步失败服务都不会启动**，规则与单镜像方式一致。

**5. 配置宿主机 nginx**：把 prepare 渲染出的 `./nginx/aigc-local-life.conf` 加进宝塔站点配置并 reload（路由说明见 [README.md](README.md) 第二节）。

**6. 配置 webhook**：在 hook 服务里注册部署单元 `aigc-local-life`（对应 `Makefile_x` 的 `HOOK_PARAM`），执行的命令约定为：

```bash
cd /opt/aigc-local-life && docker compose -f docker-compose.registry.yml pull \
  && docker compose -f docker-compose.registry.yml up -d
```

hook 侧实际执行什么命令需要和运维确认一次：只要最终效果是「拉新镜像 + 重建容器」即可，容器名与项目名不变，服务会原地重建。

## 三、日常发布

```bash
cd <仓库根目录>
make -f Makefile_x deploy TAG=v1.0.1
```

依次做三件事：构建 7 个镜像 → 推送私有仓库 → `curl` 触发 hook。需要更细的控制时：

```bash
make -f Makefile_x push-all TAG=v1.0.1                     # 只推不触发
make -f Makefile_x gateway-api TAG=v1.0.1                   # 只构建这一个到本机（不进仓库），要推需手动 docker push
curl -X GET "https://<hook域名>/hook?access_key=<key>&param=aigc-local-life"   # 只触发部署
```

## 四、验证

```bash
cd /opt/aigc-local-life
docker compose -f docker-compose.registry.yml ps                     # 6 个服务 healthy
docker compose -f docker-compose.registry.yml logs -f --tail=200 gateway-api
docker compose -f docker-compose.registry.yml run --rm prepare bash /app/pkg/install.sh check-billing   # 计费复查（只读）
TOKEN=<App 登录返回的 token>
curl -s -H "Authorization: Bearer $TOKEN" https://<PUBLIC_DOMAIN>/api/v1/local-life/copywriting/options | head -c 200
```

## 五、升级与回滚

**升级**：`.env` 里 `AIGC_TAG` 改成新版本号 → `docker compose -f docker-compose.registry.yml pull && up -d`（正常情况由 hook 自动完成）。镜像变了，全部容器重建，prepare 先跑新版本的迁移，有几秒中断。

**回滚**：`AIGC_TAG` 换回上一个版本号 → `pull && up -d`。本项目的迁移只新增表 / 列 / 数据，旧版本可以在新表结构上运行；需要连数据一起回退时用升级前的 RDS 备份恢复。

一条纪律：**不要重推已经发布过的 TAG**。tag 是可变的，重推后回滚会拉到与上次不同的内容。需要严格不可变时，把 compose 里的 `image:` 改成带 `@sha256` digest（7 个镜像都要改）。

## 六、从单镜像方式切过来

1. 记下旧 `AIGC_IMAGE`（含 digest）作为回退保险；
2. 拷 `docker-compose.registry.yml` 到服务器，`.env` 填好 `AIGC_REGISTRY` / `AIGC_TAG`；
3. `pull && up -d`。项目名与服务名都相同（`aigc-local-life` / `copywriting-rpc`…），compose 直接重建容器，**不会出现新旧两套并存**；
4. 验证通过后从 `.env` 删掉 `AIGC_IMAGE`（留着也不影响，新 compose 不读它）。

切换瞬间容器会重建、有几秒中断；prepare 会按 ops 镜像里的 sql 继续推进迁移。

## 七、常见问题

| 现象 | 处理 |
|---|---|
| 服务器拉镜像失败（unauthorized / manifest unknown） | `docker login` 或配 insecure-registries；确认 `AIGC_REGISTRY` 的 `:5000` 端口没漏 |
| 本地推送失败 | 先 `make -f Makefile_x login`；确认 `REGISTRY` 地址与服务器一致 |
| 点了 hook 没反应 | hook 的 `param` 必须与 `HOOK_PARAM` 一致（默认 `aigc-local-life`）；hook 执行用户要能写 `/opt/aigc-local-life` |
| `prepare` 退出非 0 | `logs prepare`，按 ✗ 那行的原因修正 `.env` 或数据库后重新 `up -d`，与单镜像方式处理方式一致 |
| 服务一直 starting / unhealthy | `logs <服务名>`；常见是端口被宿主机其它进程占用；健康检查用容器内的 `curl` / `bash`，镜像已自带 |
| 想让两套并存 | 改其中一套的 `COMPOSE_PROJECT_NAME`（容器名不同即可共存），但两者共用同一个数据库和 `./etc`，**不能同时跑** |
| 本机镜像太多 | `make -f Makefile_x clean` |

## 相关文件

| 文件 | 作用 |
|---|---|
| `Makefile_x` | 构建 / 推送 / 登录 / 发布 hook |
| `deploy/docker/_Dockerfile.svc` | 服务镜像（参数化：一个服务一个镜像） |
| `deploy/docker/_Dockerfile.ops` | ops 镜像（prepare + 运维工具） |
| `deploy/docker/docker-compose.registry.yml` | 本流程的 compose |
| `deploy/docker/.env.example` | `.env` 模板（含 `AIGC_REGISTRY` / `AIGC_TAG`） |
| [README.md](README.md) | 单镜像方式的部署手册（校验规则、迁移、nginx 路由细节） |
| [../server/README.md](../server/README.md) | 没有 Docker 的服务器用一键脚本部署 |