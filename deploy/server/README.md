# aigc-life 服务器部署（宝塔 / 一键脚本）

> **改配置改哪一份**：改**你运行 `install.sh` 的那个目录**里的 `deploy.env`（比如解压出来的包目录）。
> 脚本优先读它，装完会把它覆盖到 `/opt/aigc-life/deploy.env`——所以改那份装好的会被覆盖掉、看起来"改了没生效"。
> 两份不一致时脚本会停下来提醒。

一个安装包 + 一条命令：`bash install.sh`。它会检查环境、建表、生成配置、注册成系统服务并启动、最后自检。
**可重复执行**，升级也是同一条命令。

**不影响服务器上已有的项目**：
- 只新建 `aigc-life-*` 这几个系统服务，装在单独的目录（默认 `/opt/aigc-life`）。
- 端口默认用 18888 / 18889 / 19201–19204 / 15055，**避开了宝塔面板的 8888**；如果被别的程序占用，脚本会停下来提示，不会抢端口。
- 数据库只**新建**本项目的 `afv_*` 表、插配置数据，**不改现有表结构**；已经建过的自动跳过。
- 不需要装 etcd、Docker、Go，服务之间本机直连。

---

## 一、准备

| 需要 | 说明 |
|---|---|
| 服务器 | Linux x86_64（CentOS 7+/Ubuntu/Debian 都行），root |
| 数据库 | 现有系统的 `fusion_aigc` 库（本机或远程都行），要一个能建表的账号 |
| 出网 | 能访问阿里云百炼（文案、配音）和 OSS |
| 安装包 | `aigc-life-<版本>.tar.gz`（开发机上 `make package` 生成，在 `dist/` 下） |

## 二、安装（第一次）

**1. 上传并解压**

宝塔 →「文件」→ 进到 `/root` → 上传 `aigc-life-<版本>.tar.gz` → 右键「解压」，得到 `/root/aigc-life/`。

（或者命令行：`cd /root && tar xzf aigc-life-*.tar.gz`）

**2. 第一次运行，生成配置文件**

宝塔 →「终端」：

```bash
cd /root/aigc-life
bash install.sh
```

会提示「已生成 deploy.env，请先填写」然后退出。

**3. 填配置**

宝塔「文件」里打开 `/root/aigc-life/deploy.env` 编辑。**一般只需要填数据库**：

```bash
DB_HOST=127.0.0.1      # MySQL 在本机就是 127.0.0.1
DB_PORT=3306
DB_NAME=fusion_aigc
DB_USER=root
DB_PASS=你的密码        # 宝塔「数据库」页能看到 / 重置
```

其余保持默认即可（端口、mock 执行面都已经配好）。

**4. 正式安装**

```bash
bash install.sh
```

看到 6 步都是绿色 ✓、最后出现「部署完成」就好了，大约 20 秒。示例：

```
== 1/6 检查环境          ✓ Linux x86_64，systemd 管理进程
== 2/6 检查数据库        ✓ 已连接 127.0.0.1:3306/fusion_aigc
== 3/6 建表              ✓ V1.0.106__ai_copywriting.sql …（已存在的显示“跳过”）
== 4/6 安装程序与配置    ✓ 端口都可用
== 5/6 启动服务          ✓ gateway-api（端口 18888）…
== 6/6 自检              ✓ App 网关 / 数字人 / 图文笔记 / 海报口播 / OSS 直传凭证 / 运营后台
部署完成
```

**5. 生成系统音色试听（只需一次）**

```bash
yum install -y jq     # Ubuntu/Debian：apt install -y jq
bash /opt/aigc-life/install.sh voices
```

## 三、让外网访问（二选一）

只有两个端口需要对外：`18888`（App 接口，给前端）和 `18889`（运营后台）。

**方式 A：直接放行端口（最快）**

1. 宝塔 →「安全」→ 放行端口 `18888`、`18889`；
2. 云服务器控制台（阿里云/腾讯云）→ 安全组 → 同样放行这两个端口。

前端地址：`http://<服务器公网IP>:18888/api/v1/local-life`

**方式 B：用宝塔建网站做反向代理（有域名、要 HTTPS 时用）**

1. 宝塔 →「网站」→ 添加站点，域名如 `aigc-api.xxx.com`（纯静态即可）；
2. 站点「设置」→「反向代理」→ 添加：目标 URL `http://127.0.0.1:18888`；
3. 站点「设置」→「SSL」申请证书；
4. 反向代理默认的上传大小可能是 1M，站点「配置文件」里加一行 `client_max_body_size 20m;`（本地开发才用网关代传上传，线上是前端直传 OSS，一般用不到）。

前端地址：`https://aigc-api.xxx.com/api/v1/local-life`。运营后台同理再建一个站点代理到 `18889`。

## 四、验证与接口文档

**在线接口文档（Swagger）**，浏览器直接打开，可以在页面上调试：

- App 接口：`http://<服务器公网IP>:18888/swagger`
- 运营后台：`http://<服务器公网IP>:18889/swagger`

调试前先点右上角「Authorize」，填 `X-User-Id`（后台填 `X-Admin-Id`），比如 `1`，之后每个接口「Try it out」→「Execute」就会带上。
文档由代码里的接口定义自动生成，和实际接口一致。按页面逐步对接的说明在安装目录 `docs/` 下（`api-app.md`、`api-voiceover.md`）。
上线前如需关闭文档页：在仓库的 `deploy/etc/gateway-api.yaml`、`admin-api.yaml` 里加一行 `Swagger: false`，重新打包安装。

**命令行验证**：

```bash
curl -s -H 'X-User-Id: 1' http://127.0.0.1:18888/api/v1/local-life/image-creations/options?kind=voiceover
```

返回 `{"code":0,...}` 就正常。前端联调文档在安装目录 `docs/` 下（`api-app.md`、`api-voiceover.md`）。

**关于 mock**：现在没有 AIGC 执行面的正式地址，自带的 `mock-aigc` 会模拟它：提交后约 10 秒「生成完成」，
成品统一是 `deploy.env` 里 `MOCK_SAMPLE_IMAGE` / `MOCK_SAMPLE_VIDEO` 这两个示例文件（会真实转存到 OSS，前端能打开/播放）。
在文案里写 `mock-fail` 可以模拟生成失败。

## 四·五、真实链路回归（切到正式执行面后跑一次）

`e2e-real.sh` 对着已部署的环境，每个模块真实跑一条，确认返回结构与耗时：

```bash
cd /opt/aigc-life
GW=http://127.0.0.1:18888/api/v1/local-life bash scripts/e2e-real.sh          # 只跑便宜的（文案/档案/笔记/海报/长图）
GW=http://127.0.0.1:18888/api/v1/local-life bash scripts/e2e-real.sh video    # 出视频的（口播/动作迁移/探店/短剧/复刻/数字人）
GW=http://127.0.0.1:18888/api/v1/local-life bash scripts/e2e-real.sh all      # 全部
```

- **出视频的模块会真实产生费用**，而且慢（云端 2–10 分钟，本地 GPU 可能十几分钟），脚本最多等 45 分钟。
- 素材会自动下载示例图/视频再上传成我们自己的地址，不用提前准备。
- 海报、长图要运营先配模板，没配会显示「跳过」。数字人要先跑过 `install.sh voices`。
- 测试数据记在固定用户（默认 990909）名下，跑完脚本会打印清理用的 SQL。

## 五、日常操作

```bash
bash /opt/aigc-life/install.sh status            # 各服务状态
bash /opt/aigc-life/install.sh restart           # 重启全部
bash /opt/aigc-life/install.sh stop              # 停止全部
bash /opt/aigc-life/install.sh logs gateway-api  # 看日志（Ctrl+C 退出）
bash /opt/aigc-life/install.sh uninstall         # 停止并注销服务（不删库、不删目录）
```

- 服务名：`gateway-api` `admin-api` `copywriting-rpc` `digitalhuman-rpc` `note-rpc` `imagegen-rpc` `mock-aigc`。
- 已注册成系统服务：**服务器重启后自动启动**，进程挂了 3 秒后自动拉起（`systemctl status aigc-life-gateway-api` 也能看）。
- 日志在 `/opt/aigc-life/logs/`：`<服务>.out` 是启动输出，`<服务>/` 目录下是业务日志，按天切分、保留 7 天。
- 重启不丢任务：生成中的记录都在库里，服务起来后会接着查结果。

## 六、升级

开发机 `make package` 出新包 → 上传解压（可以覆盖 `/root/aigc-life`）→

```bash
cd /root/aigc-life && bash install.sh
```

- 配置自动沿用：新包里没有 `deploy.env` 时，用上次安装留在 `/opt/aigc-life/deploy.env` 的那份。
- 新增的建表脚本会自动执行，已执行过的跳过。
- 会先停旧进程再启动新的，中间有几秒不可用。

## 七、以后切到正式执行面

拿到 AIGC 执行面地址后，改 `/opt/aigc-life/deploy.env`：

```bash
MOCK_ENABLED=0
AIGC_ENDPOINT=http://<执行面地址>:5000
```

再执行 `bash /opt/aigc-life/install.sh`，mock 会被停掉并注销。

## 八、运营数据（否则 App 里对应列表是空的）

公共数字人、海报/长图模板、预置动作视频、数字人口播预设背景：在现有后台「模板」里维护，或用安装包里的 `seed` 工具批量导入：

```bash
/opt/aigc-life/bin/seed -dsn 'root:密码@tcp(127.0.0.1:3306)/fusion_aigc' -kind voiceoverBackground -csv bg.csv -dry  # 先看要写什么
/opt/aigc-life/bin/seed -dsn 'root:密码@tcp(127.0.0.1:3306)/fusion_aigc' -kind voiceoverBackground -csv bg.csv      # 真写
```

`-kind`：`publicModel` / `posterTemplate` / `longImageTemplate` / `actionVideo` / `voiceoverBackground`，CSV 列名见 `seed -h`。

## 九、常见问题

| 现象 | 处理 |
|---|---|
| `端口 xxx 已被占用` | 在 `deploy.env` 里换一个端口，重新 `bash install.sh` |
| `连不上数据库` | 检查 `DB_*`；MySQL 在别的机器上时，宝塔「数据库」里把该用户的访问权限改成允许这台服务器 IP |
| `找不到 mysql 客户端` | `deploy.env` 里填 `MYSQL_BIN=/www/server/mysql/bin/mysql` |
| `afv_system_config 缺少 …` | 连错库了，或现有系统没配 OSS / 文案模型。本项目复用这些配置 |
| `缺少配音模型配置`（黄色） | 只影响数字人配音和音色试听。在 `afv_ai_model` 加一行 `code=qwen-audio-3.1-tts-flash`、`model_type=4`、`ai_provider_id=百炼那条`，再执行一次 `bash install.sh` 会自动补上 `ai_tts_model_id` |
| 自检某项 ✗ | `bash install.sh logs <服务名>` 看报错 |
| 外网访问不了 | 宝塔「安全」和云服务器安全组都要放行，或用反向代理（第三节） |

## 十、现在的限制（上线前要补）

- **用户鉴权**：接口信任请求头 `X-User-Id`，运营后台信任 `X-Admin-Id`，只适合联调。
- **计费**：生成不扣点。
- **执行面**：目前是 mock。
