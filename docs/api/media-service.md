# 共享媒体服务契约（app/media）

封面、水印、合成三种无状态的视频处理。服务**不碰业务表和存储**：输入是公网地址，输出是字节流，调用方自己落库、自己转存。

## 部署（容器，推荐）

FFmpeg 与 ffprobe 只装在这个容器里，宿主机和其它服务不需要装。

```bash
docker build -f deploy/docker/Dockerfile.media -t <仓库>/aigc-media:v1.0.0 .
cd deploy/docker
cp media.env.example media.env && chmod 600 media.env   # 填 AIGC_MEDIA_IMAGE、MEDIA_SERVICE_TOKEN
# 水印：把 PNG 放到 MEDIA_LOGO_FILE 指定的路径（默认 ./logo.png），不需要水印就不放
docker compose -f docker-compose.media.yml --env-file media.env up -d
```

- 镜像基于 debian bookworm，装的是发行版的 ffmpeg 5.1（自带 ffprobe），以非 root 用户运行。
- 网络是 host 模式，只监听 `127.0.0.1:18090`。调用方在同一台机器上直接访问。
- 配置是 `deploy/docker/media/media-svc.yaml`，其中 `Token`、`Listen` 来自环境变量（`conf.UseEnv`），**token 为空时容器拒绝启动**。
- 容器自带健康检查（`/healthz`）。
- Java 侧配置：`MEDIA_SERVICE_BASE_URL=http://127.0.0.1:18090`，`MEDIA_SERVICE_TOKEN` 与容器一致。
- 合成要把多段视频落到容器临时目录，内存与磁盘按 `MaxInputBytes × 片段数` 估算。

## 实例发现与注册

注册中心是 [r-nacos](https://github.com/nacos-group/r-nacos)（兼容 Nacos 协议，Apache-2.0）。

- **注册**：配置了 `Registry.ServerAddr` 的实例启动后，以**临时实例**（ephemeral）注册到 `Registry.ServiceName`（默认 `aigc-media`，分组 `Registry.Group`，默认 `DEFAULT_GROUP`）。实例保活由 Nacos SDK 通过 gRPC 连接完成：进程退出或断线后，注册中心自动摘除（实测 kill -9 后 5 秒内消失）。收到 SIGTERM 时主动注销，调用方立刻停止路由到本实例。注册中心连不上不影响服务启动，后台退避重连。
- **地址要求**：`Advertise` 必须是 **IP** 加端口（如 `http://10.0.0.5:18090`），不能是域名，注册中心不接受域名。
- **发现**：调用方订阅同一个服务名，只使用**健康且启用**的实例。注册中心没有健康实例时使用静态回退地址（`BaseURL` / `base-urls`）。调用方通过实例的 `scheme` 元数据拼出调用地址，缺省 `http`。
- **路由**：请求按视频 URL（或文件内容的 SHA-256）做 rendezvous 哈希排序，同一个视频固定打到同一实例，让它的输入缓存生效；实例增减时只有映射到变动实例的视频会换节点。首选实例连不上或返回 503 时转到下一个实例；整轮都是 503 时按 `Retry-After` 等待后重试。
- **缓存**：下载的输入视频按 URL 缓存在本实例，最后一次使用后保留 `InputCacheMinutes`（默认 5 分钟）再清理，使用中的文件不会被清。缓存只在本实例内有效，实例之间不共享。
- **客户端版本**：Go 使用 `nacos-sdk-go/v2` v2.2.5，Java 使用 `com.alibaba.nacos:nacos-client` 2.1.0，均为 r-nacos 验证过的版本。

## 鉴权与部署

- 每个请求带头 `X-Media-Token: <token>`，缺失或错误返回 `401`。服务未配置 token 时拒绝启动。
- 只应部署在内网。服务会下载调用方给的地址，默认拒绝回环、内网、链路本地等非公网地址（防 SSRF），线上不得开启 `AllowPrivateHosts`。
- 依赖 `ffmpeg` / `ffprobe`（容器镜像里已装；裸机运行时需自行安装）。水印需要配置 `LogoPath`。

## 接口

| 方法 | 路径 | 请求体 | 成功响应 |
|---|---|---|---|
| GET | `/healthz` | - | `200 ok` |
| GET | `/v1/capabilities` | - | `200 {"cover": true, "compose": true, "watermark": <logo 是否已配置>}` |
| POST | `/v1/cover` | 见下面「视频输入」 | `200 image/jpeg`，单张代表帧 |
| POST | `/v1/watermark` | 见下面「视频输入」 | `200 video/mp4`，右下角叠加 logo |
| POST | `/v1/compose` | `{"video_urls": ["...", "..."]}` | `200 video/mp4`，按顺序拼接 |

成功时 `Content-Length` 有值；调用方应读完整个响应体后再使用。
`/healthz` 与 `/v1/capabilities` 之外的接口都需要鉴权。

### 视频输入（cover、watermark）

二选一：

- **公网地址**：`Content-Type: application/json`，体为 `{"video_url": "https://..."}`，服务端下载（经 SSRF 检查）。
- **文件本体**：`Content-Type` 为其它值（如 `video/mp4`），请求体就是视频文件。调用方手上只有本地临时文件时用这种，不必为它另外发布地址。

体积上限、超时由服务端配置决定（`MaxInputBytes`）。

## 错误

失败时返回 JSON：`{"code": "...", "message": "..."}`，`message` 是可以给用户看的分类说明，不含 ffmpeg 原始输出。

| HTTP | code | 含义 | 调用方处理 |
|---|---|---|---|
| 401 | `unauthorized` | token 错误 | 永久失败，检查配置 |
| 409 | `logo_unavailable` | 服务端没配水印 logo | 跳过水印，不算失败 |
| 422 | `bad_input` | 地址不合规、体积超限、源站 4xx、ffmpeg 解不开 | 永久失败，不重试 |
| 503 | `unavailable` | 超时、源站 5xx、并发满（带 `Retry-After: 5`） | 暂时失败，可以重试 |
| 500 | `internal` | 服务自身问题（缺 ffmpeg、写不了临时目录） | 告警，按暂时失败处理 |

网络层面连不上服务，调用方应按 503 处理。

## 语义约定

- **封面不影响交付。** 调用方抽封面失败只能记录跳过，不能把视频标记为失败。
- **水印跳过不算失败。** 409 `logo_unavailable` 时视频原样交付。
- 合成先尝试 concat demuxer 零转码，失败再回退到 filter_complex 重新编码；输入无音轨时回退为仅视频。
- 单个输入体积上限、合成片段数上限、并发数、超时都在服务配置里（`deploy/etc/media-svc.yaml`）。

## 调用方

- Go：`pkg/mediasvc`，目前只封装了 `Cover`（复刻成片封面）；水印、合成需要时按同样方式补充。
- Java：`com.pengqiwl.fusion.service.media.MediaServiceClient`（fusion-aigc）。封面、水印走文件本体，合成走公网地址。未配置 `app.media-service.base-url` 时回退到本机 ffmpeg。
