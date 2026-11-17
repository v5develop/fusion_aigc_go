# 共享媒体服务契约（app/media）

媒体服务统一负责视频和音频的读取与加工：下载来源、完整解码校验、探测、截剪、规整、抽帧、封面、水印、合成、变速、删除中间产物，以及按用途落桶。**调用方只发指令，不碰字节。** 契约以 `app/media/api/media.api`（go-zero .api）为准，本文档是它的说明。

当前状态：落桶已接入 OSS（`app/media/store_oss.go`），未启用 OSS 时退回本地目录，只用于开发。对象按 **rid** 定位：rid 形如 `<所属实例 6 位>-<类别>-<26 位编码>`（例如 `tst001-work-…`），桶内路径为 `v1/<类别>/<yyyy>/<mm>/<对象键>[.<后缀>]`（日期取首次写入时间，后缀由内容类型决定；直传目标的后缀由声明的 `mime` 决定），rid → 路径由私有桶索引对象 `idx/<对象键>.json` 给出。对象键由 rid 经 HMAC 派生，不含业务域、用户、任务信息。类别为 `user`、`work`、`deliver`、`asset`，与存储的对应见下文「资源寻址（rid）」。通用删除只允许 `work`、`deliver`、`asset`，`user` 类别（用户输入）与历史 `sha256/` 路径一律拒绝（403 `forbidden_purpose`）；用户输入只能经保留期删除接口 `resources/retention/delete` 软删除，宽限期过后由清理任务物理删除（见「用户输入保留期删除」）。

新增的音频与字幕接口已落地（`audio/timeline`、`video/replace-audio`、`video/burn-subtitles`），与既有接口一样走 `Idempotency-Key`。

## 对象引用与运维定位

- **业务响应只返回 `rid`**（以及类别、存储位置、摘要等元数据）。业务响应不含对象键（`key`、`object_key`）与桶信息：调用方（Java、网关）只保存 rid，桶与对象键不由调用方持有或推算。
- **运维定位**：`POST /v1/mediasvc/ops/locate`，请求体 `{"rid": "..."}`，鉴权用运维令牌（`MEDIA_OPS_TOKEN`，与业务令牌分开）。返回 `class`、`store`、`bucket`、`object_key`、`path`，只用于排障、对账与人工恢复。运维令牌未配置时该接口不开放（404）；用业务令牌访问返回 401。
- **溯源与重放**：rid 由类别与幂等键确定性派生，用同一个幂等键重新执行同一次写入，得到同一个 rid 与同一个对象（覆盖写，结果一致）。因此只要保存了来源（调用方的 `sourceKind` / `sourceRef`），rid 失效后可以按原来源重新拉取并用原幂等键重放。

## 请求头下载的主机名单

`source.headers`（如 `Authorization`）只会发送给配置项 `MEDIA_INGEST_HEADER_HOSTS` 列出的主机（逗号分隔，只列素材站自己的主机）。名单为空（默认）时，所有带 `headers` 的 ingest 都返回 `403 forbidden_purpose`（check=`source.headers_host_not_allowed`），不向任何主机发送凭证。带请求头的下载不进共享缓存，每次都直接从源站取。

## 输入与超时配置

以下配置都经环境变量注入（compose 里有默认值，见 `deploy/docker/docker-compose.media.yml`）：

| 变量 | 默认 | 含义 |
|---|---|---|
| `MEDIA_MAX_INPUT_BYTES` | 1073741824（1 GiB） | 单个输入（下载、字节上传 `purpose=user_input`）的体积上限，超过返回 422 |
| `MEDIA_STORE_LOCAL_MAX_BYTES` | 2147483648（2 GiB） | 我方产物字节上传（`purpose` 非 `user_input`，如 Java 本地加工的文件）的体积上限，与 Java 的 `GATEWAY_RESULT_MAX_BYTES` 对齐；超过返回 400 `invalid_request`（check=`request.body_too_large`），不判为素材被拒 |
| `MEDIA_FETCH_TIMEOUT_SECONDS` | 1800 | 单个输入下载的总时长上限（大视频下载可能很慢）。必须为正数，留空或 0 启动即拒绝 |
| `MEDIA_OP_TIMEOUT_SECONDS` | 1800 | 单次请求（下载之外的处理，含 ffmpeg 与校验）的时长上限，也是请求 `deadline_ms` 的上限。必须为正数，留空或 0 启动即拒绝 |

调用方（Java 的 `app.media-service.timeout-seconds`，环境变量 `MEDIA_SERVICE_TIMEOUT_SECONDS`，默认 3600）的 HTTP 超时必须**大于** `MEDIA_OP_TIMEOUT_SECONDS`（fusion-aigc 的 `deploy.sh` 据此拒绝倒置的配置），否则会先于媒体服务超时、媒体侧仍在处理，产生重复提交。推荐至少为 2 倍：排队等名额也计在内。

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
- 合成要把多段视频落到容器临时目录，内存与磁盘按 `MaxInputBytes × 片段数` 估算。

## 实例发现与注册

注册中心是 [r-nacos](https://github.com/nacos-group/r-nacos)（兼容 Nacos 协议，Apache-2.0）。

- **注册**：配置了 `Registry.ServerAddr` 的实例启动后，以**临时实例**（ephemeral）注册到 `Registry.ServiceName`（默认 `aigc-media`，分组 `Registry.Group`，默认 `DEFAULT_GROUP`）。
- **地址要求**：`Advertise` 必须是 **IP** 加端口（如 `http://10.0.0.5:18090`），不能是域名，注册中心不接受域名。
- **发现**：调用方订阅同一个服务名，只使用**健康且启用**的实例。注册中心没有健康实例时使用静态回退地址（`BaseURL` / `base-urls`）。调用方通过实例的 `scheme` 元数据拼出调用地址，缺省 `http`。
- **实例 ID**：每个实例的注册元数据带 `instance_id`（部署时持久化的 6 位 ID，见 `InstanceID`）。启动前注册中心里若有存活实例使用同一 ID 且地址不同，实例**拒绝启动**；墓碑列表（配置项 `media-instance-tombstones`）里的 ID 同样拒绝启动。下线用 `media-svc -f <配置> -retire <id>`，它把 ID 写入墓碑列表，可重复执行。
- **路由**：请求按视频 URL（或内容 SHA-256）做 rendezvous 哈希排序，同一个视频固定打到同一实例，让它的输入缓存生效；实例增减时只有映射到变动实例的视频会换节点。资源 ID（rid）键先路由到它的**所属实例**（rid 里的实例 ID，在注册元数据里找地址）；所属实例不在线时按哈希顺序兜底，结果不变（共享桶保证可读）。首选实例连不上或返回 503 时转到下一个实例；整轮都是 503 时按 `Retry-After` 等待后重试。
- **所属实例的选择**：新资源的所属实例由服务端按幂等键在存活实例的 ID 集合上做 rendezvous 哈希选出（与路由同一算法）。成员不变时重试得到同一个 rid；成员增减的窗口内重试可能得到新 rid，见设计文档 §5。
- **缓存**：下载的输入按 URL 缓存在本实例，最后一次使用后保留 `InputCacheMinutes`（默认 5 分钟）再清理，使用中的文件不会被清。缓存只在本实例内有效，实例之间不共享。按 `rid` 读取已落桶的对象（`rid` 输入、`register`、`resources/copy`）另有对象缓存：本实例写入或读过的对象留在本地（上限 `ObjectCacheMaxBytes`，默认 10 GiB 且不超过磁盘可用空间 20%；空闲保留 `ObjectCacheMinutes`，默认 30 分钟），每次使用前都核对待删除标记（410 不变）与存储里的对象版本，不一致或已删除即回落从存储下载，所以结果与不缓存时相同。
- **客户端版本**：Go 使用 `nacos-sdk-go/v2` v2.2.5，Java 使用 `com.alibaba.nacos:nacos-client` 2.1.0，均为 r-nacos 验证过的版本。

## 鉴权与安全

- 每个请求带调用方令牌，二选一：`Authorization: Bearer <token>`（服务间推荐），或 `X-Media-Token: <token>`（兼容旧调用方）。缺失或错误返回 `401`。服务未配置 token 时拒绝启动。两种写法用的是同一个密钥。
- 只应部署在内网。服务会下载调用方给的地址，默认拒绝回环、内网、链路本地等非公网地址（防 SSRF），线上不得开启 `AllowPrivateHosts`。私有桶的域名要进 `trusted-private-hosts`。
- 依赖 `ffmpeg` / `ffprobe`（容器镜像里已装；裸机运行时需自行安装）。水印需要配置 `LogoPath`。

## 请求约定

- 基础路径：`/v1/mediasvc`，`Content-Type: application/json`。
- `/healthz` 不加前缀，不需要鉴权。
- **写操作**（`ingest`、`cover`、`watermark`、`video/trim`、`video/conform`、`image/conform`、`image/resize`、`image/watermark`、`image/grid`、`image/crop`、`image/mark`、`compose`、`audio/extract`、`audio/timeline`、`video/replace-audio`、`video/burn-subtitles`、`video/adjust-duration`、`delete`）必须带 `Idempotency-Key` 头，缺失返回 400。幂等规则（按 路由 + 键 索引）：
  - 已成功完成、且请求体一致：原样回放上次的状态码、响应头和响应体，响应带 `Idempotent-Replayed: true`，不重复处理；
  - 同一个键但请求体不同：400 `invalid_request`（check=`request.idempotency_key_reused`）；
  - 同一个键正在处理中：503，`Retry-After: 5`，用同一个键稍后重试；
  - 处理过程中 panic：返回 500 `internal`（`retryable: true`），不缓存，同一个键可以重试；
  - 只缓存 2xx 结果，错误不缓存，用同一个键重试会重新执行。
  - 结果保留 `IdempotencyMinutes`（默认 120 分钟）。记录存在 `Idempotency.Store`：生产为 `redis`（默认），所有实例共享，路由失效、重启或换实例时同一个键不会重算；`memory` 只在本实例内有效，仅用于单机开发（启动时打印警告）。
  - 幂等存储（Redis）不可用：503 `transient`（check=`idem.store_unavailable`），请求不执行、不降级到内存，用同一个键稍后重试。
- 可选的 `deadline_ms`（请求体字段，毫秒）：本次请求最多处理这么久。超过服务端上限（`OpTimeoutSeconds`）时截到上限。超时后处理被取消，返回 503 `transient`，带 `Retry-After`，调用方用同一个 `Idempotency-Key` 重试。不带时用服务端上限。
- 长操作超过服务端超时时同样返回 503，`Retry-After` 给出等待秒数。

## 字节流上传（`ingest/upload`）

字体同样接受：按文件头（TTF `00 01 00 00` / `true`、OTF `OTTO`、TTC `ttcf`、WOFF `wOFF`、WOFF2 `wOF2`）识别，mime 为 `font/ttf`、`font/otf`、`font/collection`、`font/woff`、`font/woff2`，不解码、不探测宽高，体积仍受上限约束；其余内容按媒体做完整解码校验。

适用于没有 URL 的素材：用户提交的字节、网关返回的字节产物、Java 本地生成的文件。与 `ingest` 的区别是媒体字节直接放在请求体里，不经过下载。

```
POST /v1/mediasvc/ingest/upload?purpose=<用途>
Authorization: Bearer <token>
Idempotency-Key: <必填>
X-Deadline-Ms: <可选，毫秒>
Content-Type: <可选，仅作提示>

<原始字节>
```

- 服务端流式写入临时文件并计算 sha256，不把整个请求体放进内存。体积上限按用途区分：`purpose=user_input` 用 `MaxInputBytes`，超过返回 `422 source_rejected`（check=`source.size_exceeded`）；其它用途（我方产物）用 `MEDIA_STORE_LOCAL_MAX_BYTES`，超过返回 `400 invalid_request`（check=`request.body_too_large`）。
- 通过完整解码校验后按用途落桶，返回与 `ingest` 相同的 `MediaResult`（含 `rid`、`class`、`sha256`）。
- 幂等：同一个 `Idempotency-Key` + 同一份字节 = 重放（`Idempotent-Replayed: true`）；同键不同字节返回 `400 invalid_request`（`request.idempotency_key_reused`）。失败不缓存，同一个键可以重试。
- 不是合法媒体：`422 source_rejected`。
- 这个路由没有在 `media.api` 里登记：go-zero 的 `.api` 描述不了原始请求体，契约以本节为准。

## 来源（source）

`rid`、`url`、`key` 三选一（系统内首选 `rid`）：

```json
{"rid": "tst001-user-xxxxxxxxxxxxxxxxxxxxxxxxxx"}
```
```json
{"url": "https://example.com/a.mp4"}
```
```json
{"key": "v1/work/2026/10/xxxxxxxxxxxxxxxxxxxxxxxxxx.png", "store": "private"}
```

- `rid`：系统内资源。类别从 rid 推导，调用方不传 class。`store` 只对 `deliver` 类别有意义（`third_party` 读第三方桶，缺省读私有桶），其它类别忽略。
- `url`：服务端下载，经 SSRF 检查，限大小、限超时、限重定向。
- `key`：历史兼容，服务自己桶里的对象路径，只读，需要同时给 `store`。
- `headers`（可选，仅 `ingest` 的 `url` 来源）：下载请求头，对象形式 `{"Authorization": "...", "User-Agent": "..."}`。
  - 白名单：`Authorization`、`User-Agent`、`Accept`、`Referer`、`Accept-Language`（大小写不敏感）。白名单外的头、重复的头、值里含换行的头，返回 `400 invalid_request`。其它接口带 `headers` 也是 `400`，`rid` / `key` 来源带头也是 `400`。
  - 头只随请求发给 `url` 的来源。跳转到别的来源（协议或主机、端口不同）时，头全部剥掉；同来源跳转保留。
  - 头的值不写日志、不出现在错误信息里，也不参与 `rid` 派生（`rid` 只由 `Idempotency-Key` 决定）。输入缓存的键包含头的摘要，带头下载的内容不会被不带头的请求命中。
  - 安全提醒：`Authorization` 等凭证会经由媒体服务转发给素材站。调用方应只传该素材站自己的凭证，不要传 Java 的内部凭证。

## 资源寻址（rid）

- **写（产出）**：所有写操作在 `object` 里返回 `rid`、`class`、`key`（`key` 仅内部与运维使用）。rid 由类别与幂等键派生：`rid = <所属实例 6 位> - <类别> - HMAC(密钥, "rid|"+类别+"|"+幂等键) 的 26 位编码`。同一幂等键的重试得到同一 rid 与同一对象。所属实例由服务端按幂等键在注册中心的存活实例 ID 集合上做 rendezvous 哈希选出（见「实例发现与注册」）。没有幂等键的操作（如抽帧）以内容 sha256 代替幂等键。
- **读（输入）**：`source` 传 `{"rid": …}`，类别与存储由 rid 推导，不传 class。
- **地址（读侧现签）**：`POST /v1/mediasvc/resources/urls`，请求体 `{"rid", "store", "mode", "ttl_seconds", "process"}`。`process` 可选，为 OSS 命名样式（named style），只允许 `mode=signed`：其它模式返回 `400 request.process_mode`；字符超出 `[A-Za-z0-9_./-]` 返回 `400 request.process_style`。传入时地址带 `x-oss-process=<样式>`，且样式进入签名（OSS 把 `x-oss-process` 当子资源签名）；不传或为空时地址与原来一致。本地开发存储忽略样式。`class` 可以省略；如果传了，必须与 rid 一致，否则 `400 class_mismatch`。`store` 必须与类别配对：`asset` → `asset_library`；`user`、`work` → `private`；`deliver` → `private` 或 `third_party`。地址每次现签，调用方不保存。
- **删除（软删除）**：`{"rid": …}` 或 `{"object": {…}}`。只登记待删除标记，字节保留到宽限期（默认 7 天）过后由清理任务物理删除。宽限期内可用 `resources/restore` 撤销。`user` 类别与历史 `sha256/` 路径返回 `403 forbidden_purpose`，此接口不删除它们（用户输入的保留期删除走 `resources/retention/delete`）。
- **待删除对象的读取**：处于待删除状态的对象，`source`（rid 或 key）输入、`resources/urls` 签发一律返回 `410 resource_deleted`（永久失败，不可重试；撤销后才可用）。未标记的对象照常。
- **类别与存储**：`user`、`work` → 私有桶；`deliver` → 私有或第三方桶；`asset` → 素材库桶。

### mr:// 引用约定

- **网关的输入引用**：`mr://{rid}`，**不带扩展名**。网关用它向媒体服务现签地址，请求里不传 class。
- **Java 内部约定**：私有交付回填的引用为 `mr://{rid}.{扩展名}`。扩展名只用于判断产物类型，**寻址只看 rid**。读侧经 `resources/urls` 现签，有效期 600 秒。
- 这是 Java 与网关之间的约定，**不是媒体服务契约的字段**。媒体服务不解析 `mr://` 字符串，只接收 rid。

## 接口

| 方法 | 路径 | 说明 | 成功响应 |
|---|---|---|---|
| GET | `/healthz` | 健康检查 | `200 ok` |
| GET | `/v1/mediasvc/capabilities` | 能力声明（是否配置了水印 logo 等） | `200 JSON` |
| POST | `/v1/mediasvc/ingest` | 读取来源、按用途落桶：字体（TTF/OTF/TTC/WOFF/WOFF2）按文件头识别后直接落桶；其余完整解码校验。`url` 来源可带 `headers`（见"来源"） | `MediaResult` |
| POST | `/v1/mediasvc/probe` | 取元数据，不落桶 | `MediaMeta` |
| POST | `/v1/mediasvc/video/inspect` | 坏片检测（只读，不落桶，不需要 `Idempotency-Key`，同 probe）：整段 ffmpeg blackdetect + freezedetect + 完整解码，返回 `VideoInspect`：`{decodable, duration_s, black_ratio, freeze_ratio, black_seconds, freeze_seconds}`。占比 = 命中区间合并去重后的秒数 / 视频时长（0~1；片尾仍未结束的区间按到片尾算）。参数 `blackdetect=d=0.2:pix_th=0.10:pic_th=0.98`、`freezedetect=n=-55dB:d=0.5`。**解不开（容器损坏 / 无视频流 / 解码失败）不是错误**，返回 `decodable=false`、占比 0；来源取不到 / 被中断 / 超时才走常规错误码（`503 unavailable` 可重试等），调用方应按「检测不可用」处理。`source` 同 probe（rid / url / key，不接受下载头）。能力声明 `video_inspect: true` | `VideoInspect` |
| POST | `/v1/mediasvc/frames` | 抽帧，返回每帧的对象 | `FramesResp` |
| POST | `/v1/mediasvc/cover` | 代表帧封面 | `MediaResult` |
| POST | `/v1/mediasvc/watermark` | 右下角叠加 logo | `MediaResult` |
| POST | `/v1/mediasvc/video/trim` | 截到前 `seconds` 秒（重编码，切点精确） | `MediaResult` |
| POST | `/v1/mediasvc/image/resize` | 图片精确尺寸缩放：输出严格为 `width×height`，不保比例。`format` 为 `png` 或 `jpeg`。PNG 仅在源图有 alpha 小于 255 的像素时保留透明通道，否则写 RGB；JPEG 先铺白底。`width`、`height` 各 1..16384，总像素不超过服务端 `MaxSourcePixels`（超过是 `422 source_rejected`，`check=image.target_pixels_exceeded`）。源必须是 PNG/JPEG/WebP，否则 `422`（`check=image.not_image`）；解码失败也是 `422`。必须带 `Idempotency-Key` | `MediaResult` |
| POST | `/v1/mediasvc/image/watermark` | 图片展示水印：右下角一组（logo 贴右下、文字在其左侧），版式同 Java `ImageWatermarkService`（见下"图像四件套"） | `MediaResult` |
| POST | `/v1/mediasvc/image/grid` | 1..9 张图合成自适应宫格图（PNG） | `MediaResult` |
| POST | `/v1/mediasvc/image/crop` | 按像素矩形或行列均分裁出多张 PNG | `ImageCropResp` |
| POST | `/v1/mediasvc/image/mark` | 在图上画红色标记（圆环 / 实心点 / 矩形 / 折线） | `MediaResult` |
| POST | `/v1/mediasvc/image/conform` | 图片规整：长边按区间等比缩放；限定格式时转码（PNG 保留透明）；不需要处理时原样落桶 | `MediaResult` |
| POST | `/v1/mediasvc/video/conform` | 规整：去音轨、帧率、尺寸、比例；可选截剪与过短拒收 | `MediaResult` |
| POST | `/v1/mediasvc/compose` | 按顺序拼接 | `MediaResult` |
| POST | `/v1/mediasvc/audio/extract` | 抽音轨 | `MediaResult` |
| POST | `/v1/mediasvc/video/adjust-duration` | 变速 | `MediaResult` |
| POST | `/v1/mediasvc/delete` | 软删除：登记待删除标记，宽限期后由清理物理删除。请求体 `{"rid": "..."}`（首选）或 `{"object": {...}}`；`user` 类别与历史 `sha256/` 路径一律拒绝（403）。必须带 `Idempotency-Key` | `DeleteResp` |
| POST | `/v1/mediasvc/resources/retention/delete` | 用户输入保留期删除：只收 `user` 类别 rid，软删除语义与 `delete` 相同（见「用户输入保留期删除」）。请求体 `{"rid": "..."}`；必须带 `Idempotency-Key` | `DeleteResp` |
| POST | `/v1/mediasvc/resources/restore` | 宽限期内撤销待删除，对象恢复可用。请求体 `{"rid": "..."}`；不在待删除状态时返回 `restored: false`（幂等）；宽限期已过返回 `409 grace_expired` | `RestoreResp` |
| POST | `/v1/mediasvc/resources/upload-target` | 申请直传目标（只允许用户输入）：返回 rid 与签名 PUT 地址；路径后缀由声明的 `mime` 决定；目标对象已存在、或同一幂等键改了声明类型时返回 `409 conflict`，不覆盖已登记的用户对象 | `UploadTargetResp` |
| POST | `/v1/mediasvc/resources/register` | 直传完成后凭 rid 确认：完整解码校验，返回 `mime`、大小、摘要与媒体属性；内容不合法，或实际内容类型与声明类型不一致（`upload.type_mismatch`），返回 `422 source_rejected`（对象保留，由调用方决定如何处理），对象不存在返回 404 | `MediaResult` |
| POST | `/v1/mediasvc/audio/timeline` | 配音时间线：多段音频按起点拼成整条音轨（m4a） | `MediaResult` |
| POST | `/v1/mediasvc/video/replace-audio` | 换音轨：视频流拷贝，按短的一方截断 | `MediaResult` |
| POST | `/v1/mediasvc/video/burn-subtitles` | 烧录字幕（固定样式，请求体带 srt 文本） | `MediaResult` |
| POST | `/v1/mediasvc/resources/urls` | 凭 rid 现签访问地址（`mode=signed|public`）；可选 `process`（OSS 样式，仅 signed）；不传 class | `ResourceURL` |

参数细节以 `media.api` 为准。例如 `seconds` 的范围是 `(0, 600]`，`factor` 的范围是 `(0, 100]`。

**显式复制（`resources/copy`）**：`POST /v1/mediasvc/resources/copy`，请求体 `{"rid": "<源 rid>", "purpose": "asset_library" | "third_party"}`，必须带 `Idempotency-Key`。源已在私有桶而服务商要公有地址时，Java 显式发复制指令：源被复制到 `asset`（素材库桶）或 `deliver`（第三方桶）类别下的新 rid，新 rid 由幂等键派生，同键重放得到同一个 rid；返回的 `MediaResult` 中 `parents` 为 `["<源 rid>"]`，用于溯源。源不存在返回 `404 not_found`；`purpose` 为 `user_input` 或 `intermediate`（即目标为 user/work 类别）返回 `400 invalid_request`。源在本实例的对象缓存里且版本与存储一致时直接用本地字节，不再从存储下载（目标桶仍要写入一次）；源进入待删除返回 `410 resource_deleted`。要进素材库的产物优先在生产时就用 `purpose=asset_library` 直接落素材库桶，不必事后复制。

**服务端复制（`resources/copy` 快路径，2026-11 起）**：源与目标都在 OSS 且同地域时，复制由 OSS 在服务端完成（`CopyObject`；源超过 1 GiB 用 `InitiateMultipartUpload` + 并发 `UploadPartCopy`（256 MiB/片，4 并发）+ `CompleteMultipartUpload`，失败即 `Abort`），字节不经过媒体服务所在机器，不产生下载与上传流量。契约（请求、响应字段、错误码、幂等）不变：

- **结果一致**：`sha256`、`size`、`mime`、`duration_s`、`width`、`height`、`fps`、`has_audio` 与走「下载 + 上传」时逐字段相同。媒体服务没有资源表，这些值来自源对象落桶时写入的对象元数据 `x-oss-meta-media-facts`（`JSON 的 base64url` + `.` + `HMAC-SHA256 的 base64url`，含 sha256、字节数、CRC64 与 probe 元数据；每次 PUT 整体替换，只描述这次写入的字节）。所有经 probe 的落桶（ingest、ingest/upload、各加工接口、回退复制）都会写；字体与客户端直传不写。
- **事实只信本服务签过的**：对象元数据谁能写对象谁就能写（Java、发给客户端的 STS 直传凭证、运维工具；公有桶的元数据还能被匿名 HEAD 读到），所以事实用 `IdentitySecret` 派生的 HMAC 签名（只有媒体服务持有；换密钥后旧事实全部失效、自动回退，不影响正确性）。读回时依次核对：签名（不对 → `source_facts_untrusted`）；字节数与 `Content-Type` 与本次 HEAD 一致（不符 → `source_facts_stale`）；存储类型为标准型（低频 / 归档 → `source_storage_class`）；事实里的 CRC64 与 HEAD 返回的 `x-oss-hash-crc64ecma`（OSS 对当前字节算的 CRC-64/ECMA）相等（事实不带 CRC64 → `source_facts_no_crc64`，存储不返回该头 → `source_crc64_missing`，不等 → `source_crc64_mismatch`，均回退原路径）。复制请求带 `x-oss-copy-source-if-match: <HEAD 时的 ETag>`（分片复制每片都带），只复制验过事实的那一版字节；HEAD 之后源被改写时 OSS 返回 412，回退原路径（`source_changed`）；复制完成后核对目标的 CRC64（复制响应头，没有则 HEAD 目标）等于源，不等或取不到时回退原路径覆盖目标（`dest_crc64_mismatch` / `dest_crc64_missing`）。**事实与字节的绑定**：CRC64 在落桶时与 sha256 同一遍对本地字节算出（字节上传在读请求体时算出，不额外整读），签进事实；PUT 后再与响应的 `x-oss-hash-crc64ecma` 核对；**不一致绝不让写入失败**：限流打 `media ERROR check=media.crc64_mismatch …`（进程内首次立即打，之后每 10 分钟最多一条并带期间累计次数），对象保留、写入照常返回——此时事实里的 CRC64 与对象当前字节不符，读回核对必然判 `source_crc64_mismatch`，等同于事实不带 CRC64，之后对它的复制一律回退原路径。CRC64 口径为 CRC-64/XZ（ECMA-182 多项式反射、初值与结果异或全 1，即 Go `hash/crc64` ECMA 表与阿里云 OSS Go SDK 的实现；`"123456789"` → `0x995DC9BBDF1939FA`）；进程启动时用已知向量自检，不过则打 `media ERROR check=media.crc64_selftest_failed` 并整体关闭绑定（事实不带 CRC64、PUT 不核对、复制全部回退原路径）。分片复制的目标 CRC64 取 `CompleteMultipartUpload` 响应头，没有则 HEAD 目标核对。因此把真签名的事实原样抄到另一份等长、同类型的字节上会因 CRC64 不符而回退。缺 CRC64（绑定上线前写入的事实、不返回该头的非 OSS 存储）一律回退原路径，回退写出的新对象带 CRC64，之后可走服务端复制；旧事实格式不变（`crc64` 为可选字段），原签名照常验过。残余风险：CRC64 不是密码学摘要，能刻意构造同 CRC64、同长度、同类型字节并写入该路径的一方本来就能直接篡改该路径内容，不是服务端复制新引入的面。
- **元数据与权限**：单次复制用 `x-oss-metadata-directive: COPY`，内容类型与全部 `x-oss-meta-*` 原样带到目标；分片复制在初始化时显式写入内容类型与 `x-oss-meta-*`。与 `Put` 一样不带对象 ACL（目标对象继承目标桶权限：素材库桶 / 第三方桶公开，私有桶私有）、不带 `x-oss-forbid-overwrite`（同路径覆盖写，与 `Put` 相同）、不带 `x-oss-storage-class` / `x-oss-server-side-encryption`。复制出来的对象同样带事实，复制的复制也走服务端。**上线核对**：目标桶默认存储类型应为标准型；若桶配置了默认服务端加密，需在实际环境确认 CopyObject 产物同样被加密（OSS 文档对未带加密头的 CopyObject 写的是「目标不加密」），不一致时设 `DisableServerSideCopy: true`。
- **幂等**：新 rid 的派生（幂等键 → rid）、索引、待删除标记的处理与原路径完全相同；同一幂等键无论走哪条路径都得到同一个 rid 与同一个桶内路径。
- **条件（全部满足才走服务端复制）**：存储为 OSS（本地存储不支持）；未设置 `OSS.DisableServerSideCopy: true`；源桶与目标桶的 endpoint 同地域（从 `oss-<地域>[-internal].aliyuncs.com` / `vpc<N>-oss-<地域>.aliyuncs.com` 取地域，内网与公网同地域，协议头、端口、尾斜杠不影响；传输加速 `oss-accelerate*` 与认不出的 aliyuncs 域名判断不了地域，不做服务端复制，原因 `region_unknown`；非阿里云主机只有主机名完全相同才算同一处）；源对象带有效的落桶事实（见上）；这对桶最近 10 分钟内没有被拒过。请求用目标桶的密钥签名：两个桶同一密钥天然可读；密钥不同时目标密钥需对源桶有读权限。
- **回退**：不满足条件，或复制返回 403（权限）、400 / 404（非 `NoSuchKey`）/ 405 / 501（不支持、跨地域）、409、412（源在 HEAD 之后变了）时，自动回退原来的「下载 + 上传」，结果相同。403 / 400 / 404 / 405 / 501 表示这对桶整体做不了，之后 10 分钟内该桶对直接回退（按源→目标存储名记录，加锁访问）；409 / 412 只与这个对象有关，只回退这一次。分片复制任一片失败或请求被取消即 `Abort`（独立短超时上下文），不留半截分片。源在复制前被删（`NoSuchKey`）按原语义返回；5xx 等暂时故障按原语义返回（`retryable`），由调用方用同一幂等键重试。
- **日志**：成功 `media copy trace_id=… check=media.copy_server_side src_rid=… rid=… src_store=… dst_store=… size=… mode=single|multipart dur_ms=…`；回退 `media copy trace_id=… check=media.copy_fallback reason=<原因> src_rid=… src_store=… dst_store=…[ err=…]`。`reason` 取值：`store_unsupported`（本地存储）、`disabled`、`cross_region`、`region_unknown`、`denied_recently`、`invalid_store`、`source_head_failed`、`source_facts_missing`（上线前写入的对象、直传对象、字体）、`source_facts_untrusted`、`source_facts_stale`、`source_storage_class`、`source_facts_no_crc64`、`source_crc64_missing`、`source_crc64_mismatch`、`dest_crc64_mismatch`、`dest_crc64_missing`、`permission`、`unsupported`、`source_changed`。日志与错误不含对象键与地址（网络错误去掉请求 URL，只留方法与原因）。
- **阶段耗时**：服务端复制的 `timings` 为 `copy_src_head`、`copy_src_facts`（HEAD 源对象取落桶事实）、`place_object`、`copy_server`（存储服务端复制）、`url`，不出现 `copy_read` / `probe` / `hash` / `put`；回退时在 `copy_src_facts`（及被拒时的 `copy_server`）之后照旧出现 `copy_read`、`put` 等。
- **配置**：`OSS.DisableServerSideCopy`（bool，默认 `false` 即开启）；部署 yaml 不必出现该键（不写即开启，已有测试覆盖），需要关闭时加 `DisableServerSideCopy: true`。不要写成 `${ENV}` 占位：go-zero 把空环境变量展开成空串后 bool 解析失败，服务起不来。
- **收益**：同地域服务端复制只发 3 次 HEAD（定位源、待删除检查、取事实）+ 1 次复制请求（分片为 1 + N + 1 次），耗时与本机出口带宽无关（同地域 OSS 内部复制通常数十毫秒到数秒），不产生公网 / 跨网下行与上行流量。网关产物（`mr://{rid}`，经媒体服务落桶）转存业务桶同样走 `resources/copy`，自动受益，调用方（`pkg/mediasvc.CopyToLibrary`）无需改动。

### 图像四件套（`image/watermark`、`image/grid`、`image/crop`、`image/mark`）

把 Java 自研 Java2D 图像计算迁到媒体服务。四个都是写操作：必须带 `Idempotency-Key`（幂等语义见"请求约定"，同键同体回放同一结果，同键异体 400），输入 `source` 为 `rid` / `url`（`key` 历史写法也可），输出经 `purpose` 落桶（`intermediate` → work 类私有桶，`third_party` → deliver 类，`asset_library` → asset 类，与 `image/resize` 相同），响应里是 `rid` 与地址。解码、缩放、合成由 ffmpeg 完成，输入支持 PNG / JPEG / WebP（`image/watermark` 只支持 PNG / JPEG，同 Java）。**所有输入按编码像素处理，不应用 EXIF 方向**（Java `ImageIO` 同样不读 EXIF），坐标与排版以此为准。

通用错误：不是图片 / 解不开 / 超过 `MaxSourcePixels` → `422 source_rejected`（`check=image.not_image` 等，素材类、永久失败）；参数问题 → `400 invalid_request`；处理超时、被终止、排队过久 → `503 transient`（同键可重试）。

**`image/watermark`**——请求：

```json
{"source": {"rid": "mr://..."}, "logo": {"rid": "mr://..."}, "text": "示例水印",
 "logo_scale_percent": 15, "margin": 40, "text_size": 56, "text_opacity_percent": 85, "purpose": "intermediate"}
```

- `logo`（来源，可省）与 `text`（可省）至少一个，都没有 → `400 request.watermark_empty`。logo、文字由请求给出，服务不内置业务文案。
- 版式（默认值同 Java `WatermarkProperties`，括号内为可覆盖的字段）：logo 画在左上角 `(margin, margin)`，宽 = `max(1, 图宽 × logo_scale_percent / 100)`，高 = `max(1, logo 高 × 宽 / logo 宽)`（整数除法，双线性缩放）；文字为无衬线粗体，白色，画在右下角，右、下各留 `margin`（`x = max(margin, 图宽 − 文字宽 − margin)`，基线 `= 图高 − margin`），带向右下偏移 2 像素、不透明度 0.6 的黑色阴影；整层不透明度为 `text_opacity_percent`（阴影实际为 0.6 × 该值）。先画 logo，再画文字。默认：`logo_scale_percent=15`、`margin=40`、`text_size=56`（像素）、`text_opacity_percent=85`。范围：`logo_scale_percent` 0..100、`margin` 0..4096、`text_size` 0..512（0 取默认）、`text_opacity_percent` 0..100（0 即不画文字）、`text` 至多 200 字符且不含换行。
- 输出格式跟随输入：PNG 进 PNG 出（RGBA），JPEG 进 JPEG 出（先铺白底，质量约 q:v 3）。尺寸不变。
- 字体由 fontconfig 的 `Sans:bold` 决定，镜像已装 `fonts-noto-cjk`，中文可用；换镜像须保留中文字体。
- 错误：源或 logo 不是图片 → `422 image.not_image`；源是 WebP 等非 PNG / JPEG → `422 source_rejected`（`check=image.format_unsupported`）。logo 取不到 / 被拒同样是素材类 422（Java 是静默只画文字，这里显式失败，由调用方决定是否去掉 logo 重试）。

**`image/grid`**——请求：

```json
{"sources": [{"rid": "mr://a"}, {"rid": "mr://b"}], "gap": 20, "row_height": 960, "max_side": 1920, "purpose": "intermediate"}
```

- `sources` 1..9 张（超过 → `400 request.sources_count`），顺序即排版顺序。`gap`（0..512，默认 20）、`row_height`（16..4096，默认 960）、`max_side`（64..16384，默认 1920）可省，默认值同 Java `GridImageService`。
- 排版（逐项同 Java `merge`，整数取整一致）：各图保持比例缩放到 `row_height` 高；1 张直出（不拼格，只限制最长边）；2 张一行两列；3 张上 1 张居中、下 2 张并排居中；4 张 2+2；5..9 张每行至多 3 张（5→3+2，7→3+3+1，9→3+3+3），每行整体居中；白底（RGB，PNG 输出）；画布最长边超过 `max_side` 时整体等比缩小（边 = `int(边 × max_side / 最长边)`）。
- 画布总像素超过 `MaxSourcePixels`（极端长宽比的输入）→ `422 image.target_pixels_exceeded`。任一张取不到或不是图片 → 整体失败（Java 是跳过该张），避免产出与输入不对应的宫格。

**`image/crop`**——请求（`boxes` 与 `grid` 二选一）：

```json
{"source": {"rid": "mr://..."}, "boxes": [{"x": 0, "y": 0, "width": 100, "height": 50}], "purpose": "intermediate"}
{"source": {"rid": "mr://..."}, "grid": {"rows": 2, "cols": 2}, "purpose": "intermediate"}
```

- 坐标语义选**像素整数**（不用归一化）：原点在左上角，`x`/`y` 为裁剪框左上角，`width`/`height` 为宽高，必须完整落在图内（越界 → `400 request.crop_out_of_bounds`，宽高 < 1 → `request.crop_box`）。像素与 Java `getSubimage` 一致，不引入浮点取整歧义。至多 64 个框。
- `grid`：`rows × cols` 均分（≤ 64 格），行优先；单格 = `floor(W / cols) × floor(H / rows)`，余数像素丢弃（同 `HitVideoCropHandler` 的 `W/2`、`H/2`：四宫格 = `rows=2, cols=2`，16s 的二宫格 = `rows=1, cols=2`（竖版）或 `rows=2, cols=1`（16:9））。单格不足 1 像素 → `400 request.crop_grid_too_fine`。
- 响应 `{"crops": [MediaResult, ...]}`，与请求顺序（`grid` 为行优先）一一对应，输出 PNG。每张的 `rid` 由 `Idempotency-Key` 加序号派生：同键重放得到同一组 rid，不同张互不相同。

**`image/mark`**——请求：

```json
{"source": {"rid": "mr://..."}, "marks": [{"x": 100, "y": 100, "radius": 30, "color": "#ff0000"}],
 "shape": "auto", "color": "#ff0000", "line_width": 0, "format": "jpeg", "purpose": "intermediate"}
```

- `marks` 1..64 个点，**像素坐标**（原点左上角），超出图像的点夹到 `[0, 宽-1] × [0, 高-1]`（同 Java 的 clamp）。Java 传的是 0..1 比例，迁移后由调用方先换算：`clamp(round(ratio × (边 − 1)), 0, 边 − 1)`。
- `shape`：`auto`（默认，同 Java：1 点画圆环、2 点画轴对齐矩形、≥3 点画闭合折线）、`circle`（每点一个圆环）、`dot`（每点一个实心圆）、`rect`（须正好 2 点）、`polygon`（≥3 点）。圆 / 点的 `radius` 默认 `max(20, int(短边 × 0.03))`，圆环线宽默认 `max(3, int(radius × 0.12))`；矩形 / 折线线宽默认 `max(3, int(短边 × 0.005))`；`line_width`（1..256）可统一覆盖。`color` 为 `#RGB` / `#RRGGBB`，默认纯红；单点 `color` 优先于请求级 `color`（矩形 / 折线只用请求级 `color` 与第一个点的 `color`，不建议混用）。
- `format`：`jpeg`（默认，质量 95，铺白底，同 Java）或 `png`（保留透明）。边缘按抗锯齿绘制。
- 错误：`400`（`request.marks_count` / `request.shape` / `request.color` / `request.radius` / `request.line_width` / `request.format` / `request.rect_needs_two_marks` / `request.polygon_needs_three_marks`）。

### 用途与落桶（purpose）

| purpose | 含义 | 落在哪（store） | 返回的地址 |
|---|---|---|---|
| `user_input` | 用户上传的输入 | 私有桶 | 签名地址（有效期由调用方指定，上限 24 小时） |
| `intermediate` | 中间产物（截剪、规整、抽帧结果） | 私有桶 | 签名地址 |
| `asset_library` | 素材库产物 | 素材库桶（store=`asset_library`，公有） | 公有地址（优先自定义域名） |
| `third_party` | 交付给第三方的产物 | 第三方桶（store=`third_party`，公有） | 公有地址 |

- 具体桶名、对象键前缀由服务端配置决定，调用方不感知。
- 用途对应的类别：`user_input` → `user`，`intermediate` → `work`，`asset_library` → `asset`，`third_party` → `deliver`。
- **用户输入默认不删**：`delete` 对 `user` 类别（`user_input` 产生的对象）一律拒绝（`403 forbidden_purpose`）。唯一例外是调用方显式发起的保留期删除（`resources/retention/delete`，见下）：只有经它到期的用户输入会在宽限期过后被物理删除；不调用它，用户输入永久保留。

### 用户输入保留期删除

产品规则：用户输入可以永久保留，也可以在可配置的保留期后删除。**删不删、保留多少天由调用方（Java）决定**，媒体服务不保存开关与天数，只提供受门禁、可审计的软删除原语。默认（不调用）即永不删除。

- **请求**：`POST /v1/mediasvc/resources/retention/delete`，鉴权同其它资源接口；必须带 `Idempotency-Key`（规则同 `delete`）；请求体 `{"rid": "<user 类别 rid>"}`，没有其它字段（与其它接口一致，未知字段忽略不报错）。
- **门禁**：
  - 缺 `Idempotency-Key`：`400 invalid_request`（`check=request.idempotency_key_missing`）。
  - rid 解析失败（空、格式不对、历史 `sha256/` 路径）：`400 invalid_request`（`check=request.rid`）。
  - rid 类别不是 `user`（`work`、`asset`、`deliver`）：`403 forbidden_purpose`（`check=purpose.not_user_class`），什么也不写。本接口永远不删 `work`、`asset`、`deliver`。
- **行为**：与 `delete` 共用同一个软删除实现：登记待删除标记，宽限期（配置项 `DeleteGraceMinutes`，默认 10080 分钟即 7 天）内字节保留，可用 `resources/restore` 撤销；待删除期间读取（`source` 输入、`resources/urls`）返回 `410 resource_deleted`。已有标记时幂等，沿用原到期时间；对象不存在返回 `absent`。
- **响应**：`DeleteResp`，与 `delete` 相同：`{"deleted": true, "state": "pending_delete", "delete_after": "<RFC3339>"}`；对象不存在时 `{"deleted": false, "state": "absent"}`。同一 `Idempotency-Key` + 同一请求体回放首次结果（`Idempotent-Replayed: true`），不再登记；同键异体 `400 request.idempotency_key_reused`。
- **审计**：每次执行（含 `absent`）打一行 `media retention delete trace_id=… check=retention.delete rid=… state=… delete_after=…`，不记对象内容与地址。
- **标记原因**：本接口写的待删除标记带原因 `retention`（本地存储：标记文件第二行 `reason=retention`；OSS：标记对象的元数据 `x-oss-meta-delete-reason: retention`）。通用 `delete` 写的标记不带原因（视为 manual）。已有标记时不覆盖，原因也不改。
- **宽限期之后**：清理任务（`sweep.go`）对 `user` 类别只物理删除原因为 `retention` 的标记：宽限期已过，且物理删除前复读标记确认它仍在、仍已到期、原因仍是 `retention`，然后删对象（存储层专用入口 `DeleteRetentionExpired`，只收 `user` 类别）、删索引、清标记。宽限期内、或列举后被 `restore`/重新入库清掉的标记一律跳过。
- **仍然拒绝的情况**：通用 `delete` 对 `user` 类别返回 `403`；不带 `retention` 原因的 `user` 标记（手工写入或其它路径写入）到期后不删，计为失败并打日志 `check=sweep.user_marker_not_retention count=…`；存储层 `Delete` 仍拒绝 `user` 类别与历史路径。
- 每个 store 对应一个桶配置（`OSS.Private`、`OSS.AssetLibrary`、`OSS.ThirdParty`）。`asset_library` 与 `third_party` 可以配成同一个桶，之后分开时只改配置，代码不动。
- 调用方（如 imagegen 的封面）直接用媒体服务返回的地址，不下载、不再转存：存桶只发生在媒体服务里。

### 结果

写操作成功返回 `MediaResult`：

```json
{
  "object": {
    "rid": "tst001-work-xxxxxxxxxxxxxxxxxxxxxxxxxx",
    "class": "work",
    "store": "private",
    "key": "v1/work/xxxxxxxxxxxxxxxxxxxxxxxxxx",
    "sha256": "ab12...",
    "size": 1234567,
    "mime": "video/mp4",
    "duration_s": 6.0,
    "width": 720,
    "height": 1280,
    "fps": 30.0,
    "has_audio": false
  },
  "url": {"kind": "signed", "value": "https://...", "expires_at": "2026-10-21T12:00:00+08:00"}
}
```

`probe` 返回 `MediaMeta`（同 `object` 的元数据字段，不含 `key`）；`frames` 返回 `frames[]`，每帧是 `timestamp_s` 加一个对象。

**阶段耗时（可选，只增字段）**：`ingest`、`ingest/upload`、`resources/copy` 本次实际执行的成功响应在 `MediaResult` 上多一个 `"timings": [{"stage": "probe", "dur_ms": 22}, ...]`，按发生顺序列出服务端各阶段耗时（`read_body`、`idem_begin`、`validate`、`fetch`（从源地址下载到本地盘，含 DNS / TLS / 传输）、`copy_src_head`（复制：定位源桶 + 待删除检查）、`copy_read`（复制：取源字节，本机缓存命中近 0）、`probe`、`hash`、`place_object`、`put`（写对象存储）、`url`（签发 / 拼出访问地址）、`ingest_local`、`idem_finish`，按路由取其中实际经过的）。同名阶段同时打一行日志 `media stage trace_id=… stage=… dur_ms=…`，转存慢在供应商源站（`fetch`）还是我方存储（`put` / `copy_read`）据此区分。幂等重放（`Idempotent-Replayed: true`）与其它路由不带该字段；调用方不认识可以忽略。

## 错误

失败时返回：

```json
{"error": {"code": "source_unavailable", "class": "source", "retryable": false, "check": "source.fetch_status", "message": "素材地址无法访问"}}
```

| code | 含义 | Java 工作流对应的失败 | retryable |
|---|---|---|---|
| `source_unavailable` | 源站返回 404 / 410 或其它 4xx（403、408、429 除外）、对象不存在；重定向次数过多（check=`source.redirect_too_many`）、重定向到被禁止的地址（非 http/https 协议、内网 / 保留地址，check=`source.redirect_forbidden`） | 素材不可用 | false |
| `source_rejected` | 不是合法的媒体、解码被拒、超过大小、格式不支持、比例越界 | 素材被拒 | false |
| `source_too_short` | 参考视频比请求的 `seconds` 短（仅 `video/conform`） | 素材被拒 | false |
| `forbidden_purpose` | 不允许的操作（如删除 `user` 类别、删除历史 `sha256/` 路径） | 内部错误 | false |
| `invalid_request` | 参数不合法（含 `class_mismatch`、`store` 与类别不配对） | 内部错误 | false |
| `not_found` | 对象不存在（如 `resources/urls` 时对象尚未落桶） | 待确认 | false |
| `no_public_location` | 资源没有公有地址（`mode=public` 但在私有桶），HTTP 409 | 待确认 | false |
| `resource_deleted` | 对象处于待删除状态（软删除宽限期内），读取与签发地址被拒，HTTP 410 | 素材不可用 | false |
| `grace_expired` | 宽限期已过，不能再撤销（清理任务即将或已经删除它），HTTP 409 | 素材不可用 | false |
| `transient` | 资源紧张、超时、下游暂时不可用；源站 5xx、429、408、403（签名过期、CDN 拦截常见）、连接中断、DNS 临时失败 | 暂时失败 | true |

503 `transient` 都带 `Retry-After`（整数秒）：源站给了 `Retry-After`（秒数或 HTTP 日期）时换算成秒数透传，最少 1 秒、最多 120 秒（源站不受信任，过大的值会把调用方任务挂住，超出部分交给调用方自己的退避）；没有则为 5 秒。
| `internal` | 未知错误 | 暂时失败 | true |

- `check` 字段给出判据（如 `source.fetch_status=403`、`media.decode_rejected`），便于排障。
- `message` 不含地址、路径、凭证。
- 调用方按 `retryable` 决定是否重试，不再自己推断。

网络层面连不上服务，调用方应按 503 处理。

**删除响应**（`DeleteResp`）：`{"deleted": bool, "state": "pending_delete" | "absent", "delete_after": "<RFC3339>"}`。`state=pending_delete` 表示已进入待删除，`delete_after` 为物理删除的最早时间；对象不存在时 `state=absent`、`deleted=false`，不登记标记。`deleted=true` 表示对象已不可再读（待删除或已删除）。**注意**：旧版本的 `deleted=true` 表示"已物理删除"，现在改为"已进入待删除"，调用方如需物理删除时间以 `delete_after` 为准。

**配置**（`app/media/config.go`）：`DeleteGraceMinutes`（默认 10080，即 7 天）、`SweepIntervalMinutes`（默认 60）。清理任务在每个实例内周期执行，只删宽限期已过的待删除对象，清理是幂等的，多实例并跑无害。待删除标记按类别分前缀列举，单轮预算分开计：`work`、`deliver`、`asset` 合用 5000 条（`sweepMaxPerRun`），`user` 类别单独 1000 条（`sweepMaxUserPerRun`），超出的留给下一轮；用户输入标记的积压不会挤占其它类别的清理。

## 语义约定

- **封面不影响交付。** 抽封面失败只能记录跳过，不能把视频标记为失败。
- **水印跳过不算失败。** 服务端没配 logo 时视频原样交付。
- 合成先尝试 concat demuxer 零转码，失败再回退到 filter_complex 重新编码；输入无音轨时回退为仅视频。
- 截剪只按请求的 `seconds` 截短，不做延长。参考视频比它短时，`video/conform` 返回 `source_too_short`，由调用方决定是拒绝还是换素材。
- 单个输入体积上限（`MaxInputBytes`）、合成片段数上限、并发数、超时都在服务配置里。超时从请求进入开始计时，覆盖等资源、下载和处理。
- 并发分两类：计算名额（`MaxConcurrent`，只给 ffmpeg / ffprobe / 图像运算占用）与输入名额（`MaxConcurrent × 4`，给下载与请求体读取占用）。下载和上传不占计算名额。
- 排队等待名额的上限是 `min(OpTimeoutSeconds, 120s)`，超过返回 503 `transient`（`Retry-After: 5`），不会把请求挂满整个处理时长。

## 调用方

- Java：`com.pengqiwl.fusion.service.media.MediaServiceClient`（fusion-aigc，未提交）。输入已改为 rid 或 URL（`MediaResource.Source.ofRid` / `ofUrl`），不再传 key（`ofLegacyKey` 仅保留给历史数据），路径为 `/v1/mediasvc`。
  - `resources/urls` 当前调用仍在请求体里传 `class`。服务端一致时接受，不一致返回 400。按本契约应改为只传 rid，**待改**。
  - 视频翻译、视频生成、视频增强、商品、管理端封面的加工：**进行中**，正在按 rid 改造，尚未全部完成。
  - 私有交付回填使用 `mr://{rid}.{扩展名}`（见「mr:// 引用约定」），读侧经 `resources/urls` 现签（600 秒）。
- Go：`pkg/mediasvc` 的 `CoverURL` 按本契约调用（路由 key 为视频 URL）。
- AI 网关：输入引用为 `mr://{rid}`，网关向媒体服务现签，不传 class。旧 `aig://` 只读兼容，网关侧的现状以网关仓库为准（待确认）。

## 未定事项

- 字节产物与字节输入：契约尚无上传入口，依赖 b64 的供应商暂不可用。
- 旧桶保留期与 `aig_key` 别名的删除时间：待定。
- `media.api` 尚未收录 `resources/urls` 的请求与响应，`delete` 也只写了 `object` 形式、没有 `rid` 形式。以代码为准，待同步到 `media.api`。
