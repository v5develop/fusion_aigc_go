# 部署说明

> **服务器上直接部署请用一键脚本**：开发机 `make package` 出安装包，服务器上 `bash install.sh`，
> 步骤见 [`deploy/server/README.md`](server/README.md)（宝塔环境已验证：不占 8888、不装 etcd、不改现有表）。
> 下面是各组件的手工部署说明，排查问题或非标准环境时参考。

一套服务 6 个进程（外加本地联调用的 mock）。目前**鉴权是开发模式、计费未接**，可以先给前端联调，上线前必须补上（见最后一节）。

| 进程 | 端口 | 对外 | 说明 |
|---|---|---|---|
| `gateway-api` | 8888 | ✅ | App 接口 `/api/v1/local-life/*` |
| `admin-api` | 8889 | ✅ | 运营后台 `/admin/v1/*` |
| `copywriting-rpc` | 9201 | ❌ | AI文案 |
| `digitalhuman-rpc` | 9202 | ❌ | AI数字人 |
| `note-rpc` | 9203 | ❌ | AI图文笔记 |
| `imagegen-rpc` | 9204 | ❌ | 海报 / 长图 / 动作迁移 / 数字人口播 |
| `mock-aigc` | 5055 | ❌ | **仅联调**：模拟 AIGC 执行面，拿到正式地址后不要部署 |

rpc 之间通过 etcd 发现，**只有两个网关需要对外暴露**。

## 1. 依赖

- MySQL：现有 `fusion_aigc` 库，服务用同一个账号连（配置里的 `DataSource`）。
- etcd：rpc 注册发现用。测试环境一个单节点即可：
  ```bash
  docker run -d --name etcd -p 2379:2379 \
    -e ALLOW_NONE_AUTHENTICATION=yes -e ETCD_ADVERTISE_CLIENT_URLS=http://0.0.0.0:2379 \
    bitnami/etcd:3.5
  ```
- 出网：调阿里云百炼（文案、配音）与 AIGC 执行面。

## 2. 数据库变更

按顺序执行，只新建表 / 插数据，不改现有表结构（112、113、115、117 是给本项目自己的表加列，**不能重复执行**；一键脚本会自动判断跳过）：

```bash
正式环境由 `install.sh` 执行 `deploy/sql/Go_V*.sql`：先整库备份，再按版本顺序执行，每个版本登记到 `go_schema_history`（与 Java 的 `flyway_schema_history` 分开）。不要手工逐个 `mysql < 文件`，那样不会登记。
```

还需要**手动加两行配置**（脚本不含，怕误改现有环境）：

```sql
-- 1. 配音模型（若该环境还没有）：afv_ai_model 插一行
--    code=qwen-audio-3.1-tts-flash，model_type=4（语音合成），ai_provider_id=百炼那条
-- 2. 指向它
INSERT INTO afv_system_config (config_key, config_value, remark)
VALUES ('ai_tts_model_id', '<上面那行的 id>', 'AI数字人配音使用的TTS模型ID');
```

已有的 `ai_writing_model_id`（文案模型）、`storage_public_*`（OSS 与 STS 角色）直接复用，不用动。

## 3. 编译

在开发机交叉编译出 Linux 二进制：

```bash
make release          # 产物在 bin/linux/
```

或在服务器上 `make build`（需要 Go 1.26+）。

## 4. 配置

配置文件在 `deploy/etc/`，按环境改这几项：

| 文件 | 要改的 |
|---|---|
| 全部 rpc | `DataSource`（数据库）、`Etcd.Hosts` |
| `digitalhuman-rpc.yaml`、`note-rpc.yaml`、`imagegen-rpc.yaml` | `Aigc.Endpoint`=执行面地址（联调期指向 mock-aigc）；`Storage.Mode: oss` |
| `gateway-api.yaml` | `Storage.Mode: oss`、`DataSource`、各 rpc 的 `Etcd` |
| `admin-api.yaml` | 各 rpc 的 `Etcd` |

**`Storage.Mode` 线上必须是 `oss`**：`local` 模式文件落在本机磁盘，AIGC 执行面拿不到，而且不支持前端直传。
网关与各 rpc 的 `Storage` 配置要一致（rpc 靠它判断前端回传的地址是不是我们签发的）。

## 5. 启动

```bash
bin/linux/copywriting-rpc  -f deploy/etc/copywriting-rpc.yaml
bin/linux/digitalhuman-rpc -f deploy/etc/digitalhuman-rpc.yaml
bin/linux/note-rpc         -f deploy/etc/note-rpc.yaml
bin/linux/imagegen-rpc     -f deploy/etc/imagegen-rpc.yaml
bin/linux/gateway-api      -f deploy/etc/gateway-api.yaml
bin/linux/admin-api        -f deploy/etc/admin-api.yaml
```

systemd 示例（每个进程一份，改名字与配置路径）：

```ini
[Unit]
Description=aigc-life gateway-api
After=network.target

[Service]
WorkingDirectory=/opt/aigc-life
ExecStart=/opt/aigc-life/bin/gateway-api -f /opt/aigc-life/deploy/etc/gateway-api.yaml
Restart=always
RestartSec=3
StandardOutput=append:/var/log/aigc-life/gateway-api.log
StandardError=inherit

[Install]
WantedBy=multi-user.target
```

服务重启不会丢任务：生成中的记录状态都在库里，各服务的轮询器启动后会接着查。

## 6. 验证

```bash
curl -s -H 'X-User-Id: 1' http://<host>:8888/api/v1/local-life/copywriting/options | head -c 200   # 文案选项
curl -s -H 'X-User-Id: 1' http://<host>:8888/api/v1/local-life/notes/options | head -c 200         # 笔记类型
curl -s -H 'X-Admin-Id: 1' http://<host>:8889/admin/v1/digital-humans/tts-model         # 配音模型
```

返回 `{"code":0,...}` 即正常。执行面没配时，生成类接口返回 `code=1006`（暂未开放），其余接口不受影响。

## 7. 运营需要先准备的数据

否则 App 端对应的列表是空的。前五项可以用 `go run ./tools/ops/seed -kind ... -csv ...` 批量导入：

| 内容 | 在哪维护 |
|---|---|
| 公共数字人 | 现有后台「模板」：`afv_public_template_assets` + 投放位 `localLife / digitalHuman / publicModel`，`extra` 填 `{"role":"店长","avatarUrl":"…"}` |
| 海报模板 | 投放位 `localLife / poster / template`，按筛选维度 `posterType` 打标签 |
| 长图模板 | 投放位 `localLife / longImage / template` |
| 预置动作视频 | 投放位 `localLife / actionTransfer / actionVideo`，素材类型 `video` |
| 数字人口播预设背景 | 投放位 `localLife / voiceover / background`（设计稿示例：卧室），`-kind voiceoverBackground` |
| 系统音色试听 | `deploy/scripts/gen-voice-previews.sh` 一次生成完（也可在后台逐个点「生成试听」） |

## 8. 上线前必须补

- **用户鉴权**：现在信任请求头 `X-User-Id`（`gateway/.../auth_middleware.go`）。
- **后台鉴权**：现在信任 `X-Admin-Id`（`admin/.../adminauth_middleware.go`）。
- **计费**：各生成动作的扣点数未接，代码里有 TODO。
- 其余待办见根目录 `README.md` 的待办表。
