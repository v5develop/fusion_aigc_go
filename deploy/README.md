# 部署说明

## 两种部署方式

| 方式 | 用途 | 入口 |
|---|---|---|
| **Docker Compose** | **正式环境** | 开发机 `make image ... PUSH=1` 推镜像，服务器上 `docker compose up -d`，步骤见 [`docker/README.md`](docker/README.md) |
| 服务器一键脚本 | 备选（没有 Docker 的服务器） | `make package VERSION=<版本>` 出安装包，服务器上 `bash install.sh`，步骤见 [`server/README.md`](server/README.md) |

两种方式的校验规则、迁移、配置生成是同一套代码（`server/install.sh`；容器里由 `prepare` 服务调用它的 `prepare` 子命令），不会出现两边不一致。

## 进程与端口

一套服务 6 个进程，**全部只监听 127.0.0.1**，公网入口统一经 nginx：

| 进程 | 端口 | 说明 |
|---|---|---|
| `gateway-api` | 18888 | App 接口 `/api/v1/local-life/*`，nginx 转发 |
| `admin-api` | 18889 | 运营后台 `/api/v1/local-life/admin/*`，经 nginx 对外，与 Java 后台共用登录令牌，按 RBAC 校验权限 |
| `copywriting-rpc` | 19201 | AI 文案、店铺档案 |
| `digitalhuman-rpc` | 19202 | AI 数字人 |
| `note-rpc` | 19203 | AI 图文笔记 |
| `imagegen-rpc` | 19204 | 海报 / 长图 / 动作迁移 / 数字人口播 / 探店视频 / 带货短剧 / 爆款复刻 |

正式部署（两种方式）rpc 之间本机直连，不需要 etcd；只有宿主机上 `make run-all` 本地开发时用 etcd。

## 与 Java（fusion-aigc）的关系

- **共用数据库** `fusion_aigc`：本项目只新建自己的表、给自己的表加列、写入自己的分类与计费配置；迁移记录在 `go_schema_history`，与 Java 的 `flyway_schema_history` 分开。
- **共用登录会话**：Go 签发的令牌写在 Java 的会话 Redis（库号 10）里，一个令牌两边都认；令牌刷新走 Java 的 `/api/v1/auth/refresh`。
- **nginx 分流**：`/api/v1/local-life` 与 `/api/v1/local-life/admin` → Go，其余 `/api/v1/` → Java；Java 的 `/api/v1/admin`、Swagger、actuator 不对公网。
- **共用执行面**：`AIGC_ENDPOINT` 与 Java 的 `AIGC_BASE_URL` 相同，任务号前缀区分来源（Go 为 `AiCreationLife_`）。
- **共用计费表**：点数账户、预留单、计费方案都在 Java 的计费表里，Go 按同一套规则冻结 / 确认 / 退回。

## 目录

| 路径 | 内容 |
|---|---|
| `etc/` | 各服务的基础配置（开发值）。两种部署方式都由 `mkconf` 改写成正式配置 |
| `server/` | 部署逻辑 `install.sh`（一键安装与容器 `prepare` 共用）、迁移模块 `migrate.sh`、一键脚本的配置模板 `deploy.env.example`、nginx 站点模板 `nginx/` |
| `sql/` | 版本化迁移 `Go_V<版本>__<描述>.sql`（从 1.0.0 开始） |
| `sql/ops/` | 核对与一次性修补用的 SQL：`check-*.sql`（只读）、`fix-*.sql`（执行前评审），**不会自动执行** |
| `scripts/` | 运维脚本：音色试听生成、联调用占位素材（**不要在正式库执行**） |
| `docker/` | 容器部署：`Dockerfile`、正式编排 `docker-compose.yml`（不含 mock）、联调叠加 `docker-compose.dev.yml`、本地开发 etcd `docker-compose.etcd.yml`、配置模板 `.env.example` |

## 数据库迁移

- 规则：版本号从 1.0.0 递增；**已发布的迁移不能改**，改动必须放进新版本号（安装时按 sha256 校验，被改过会中止）。
- 正式环境由 `install.sh` 执行并登记，**不要手工逐个 `mysql < 文件`**。
- 老库已经手工执行过的脚本会按「库里已有对应结构」登记为 baseline，不重复执行。
- 迁移逻辑的离线测试：`make test-migrate`。

还需要**手动配置一次**的（脚本不含，避免误改现有环境）：

```sql
-- 1. 配音模型（若该环境还没有）：afv_ai_model 插一行
--    code=qwen-audio-3.1-tts-flash，model_type=4（语音合成），ai_provider_id=百炼那条
-- 2. 指向它（再执行一次 install.sh 也会自动补）
INSERT INTO afv_system_config (config_key, config_value, remark)
VALUES ('ai_tts_model_id', '<上面那行的 id>', 'AI数字人配音使用的TTS模型ID');
```

已有的 `ai_writing_model_id`（文案模型）、`storage_public_*` / `storage_private_*`（OSS 与 STS 角色）直接复用。

## 本地开发

```bash
make build                                         # 编译到 bin/（含本地联调用的 mock）
docker compose -f deploy/docker/docker-compose.etcd.yml up -d   # rpc 注册发现用（2379 只绑本机）
make run-all                                       # 本地一键启动，日志在 logs/
make test && make test-migrate                     # 单元测试 + 迁移逻辑测试
```

本地用 `deploy/etc/` 的开发配置：`AUTH_MODE=dev`（信任 `X-User-Id`）、计费关闭、存储 local、执行面指向 mock——**这些都不能用于正式环境**，正式安装脚本会拒绝。

## 运营需要先准备的数据

否则 App 端对应的列表是空的。前五项可以用 `seed` 工具（`go run ./tools/ops/seed -kind ... -csv ...`，安装包里是 `bin/seed`）批量导入：

| 内容 | 在哪维护 |
|---|---|
| 公共数字人 | 现有后台「模板」：`afv_public_template_assets` + 投放位 `localLife / digitalHuman / publicModel`，`extra` 填 `{"role":"店长","avatarUrl":"…"}` |
| 海报模板 | 投放位 `localLife / poster / template`，按筛选维度 `posterType` 打标签 |
| 长图模板 | 投放位 `localLife / longImage / template` |
| 预置动作视频 | 投放位 `localLife / actionTransfer / actionVideo`，素材类型 `video` |
| 数字人口播预设背景 | 投放位 `localLife / voiceover / background`，`-kind voiceoverBackground` |
| 系统音色试听 | 容器部署见 `docker/README.md`「生成系统音色试听」，一键脚本用 `bash install.sh voices`（都用后台账号鉴权）；也可在后台逐个点「生成试听」 |
