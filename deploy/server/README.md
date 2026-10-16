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

其余保持默认即可（端口已经配好）。

**4. 正式安装**

```bash
bash install.sh
```

看到 6 步都是绿色 ✓、最后出现「部署完成」就好了，大约 20 秒。示例：

```
== 1/6 检查环境          ✓ Linux x86_64，systemd 管理进程
== 2/6 检查数据库        ✓ 已连接 127.0.0.1:3306/fusion_aigc
== 3/6 建表              ✓ V1.0.0__ai_copywriting.sql …（已存在的显示“跳过”）
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

## 三、对外入口（只经 nginx）

公网只开 80 / 443，经宝塔站点反向代理到本机网关。`18888`（网关）、`18889`（后台）、各 rpc 端口**都只监听 127.0.0.1**，不要在安全组或宝塔「安全」里放行。

1. 在 `deploy.env` 填 `PUBLIC_DOMAIN`（如 `aigc-api.xxx.com`）和 `JAVA_UPSTREAM`（Java 服务地址，刷新令牌用）；
2. 安装完成后，`$INSTALL_DIR/nginx/aigc-life.conf` 已经渲染好，内容只放行两类路径：
   - `/api/v1/local-life/` → 本机网关（App 接口）；
   - `/api/v1/auth/refresh` → Java（与 AIGC 共用会话，刷新在那边做）；
   - 其它路径一律 404（后台、接口文档、指标都不走公网）；
3. 宝塔 →「网站」→ 添加站点 `PUBLIC_DOMAIN`（纯静态即可）→「SSL」申请证书；
4. 站点「配置文件」里把 `aigc-life.conf` 的 `upstream` 和 `server` 内容加进去（或 include 进来），保存后重载 nginx；
5. 前端地址：`https://<PUBLIC_DOMAIN>/api/v1/local-life`。

**运营后台不对公网开放**：需要时用 ssh 隧道访问本机端口：

```bash
ssh -L 18889:127.0.0.1:18889 root@<服务器>
# 然后浏览器打开 http://127.0.0.1:18889/admin/v1/...
```

**会话与 Java 共用**：`SESSION_MAX_CONCURRENT`、`TOKEN_ACCESS_HOURS`、`TOKEN_REFRESH_DAYS` 必须和 Java 生产环境的 `max-concurrent-sessions`、`TOKEN_ACCESS_HOURS`、`TOKEN_REFRESH_DAYS` 完全一致，否则两边对同一个会话的上限与过期规则不一样。

## 四、验证与接口文档

**在线接口文档（Swagger）**，默认关闭（正式环境不可见）。需要联调时在 `deploy.env` 设 `SWAGGER_ENABLED=1` 后重装，开启后可在浏览器直接打开调试：

- App 接口（仅本机）：`http://127.0.0.1:18888/swagger`
- 运营后台（仅本机，ssh 隧道）：`http://127.0.0.1:18889/swagger`

调试前先点右上角「Authorize」，填 `X-User-Id`（后台填 `X-Admin-Id`），比如 `1`，之后每个接口「Try it out」→「Execute」就会带上。
文档由代码里的接口定义自动生成，和实际接口一致。按页面逐步对接的说明在安装目录 `docs/api/` 下（`api-app.md`、`api-voiceover.md`）。
文档页由安装时的 `SWAGGER_ENABLED` 决定，不需要改 yaml。

**命令行验证**：

```bash
curl -s -H 'X-User-Id: 1' http://127.0.0.1:18888/api/v1/local-life/image-creations/options?kind=voiceover
```

返回 `{"code":0,...}` 就正常。前端联调文档在安装目录 `docs/api/` 下（`api-app.md`、`api-voiceover.md`）。

## 四·五、真实链路回归（切到正式执行面后跑一次）


```bash
cd /opt/aigc-life
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

- 服务名：`gateway-api` `admin-api` `copywriting-rpc` `digitalhuman-rpc` `note-rpc` `imagegen-rpc`。
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
AIGC_ENDPOINT=http://<执行面地址>:5000
```

再执行 `bash /opt/aigc-life/install.sh`，早期开发版遗留的 mock 服务会被注销。

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
| 外网访问不了 | 确认 nginx 站点已加载 `aigc-life.conf`、证书已配；网关端口不对外是正常的（第三节） |

## 十、现在的限制（上线前要补）

- **用户鉴权**：接口信任请求头 `X-User-Id`，运营后台信任 `X-Admin-Id`，只适合联调。
- **计费**：生成不扣点。
- **执行面**：由 `AIGC_ENDPOINT` 指定（正式地址）。
