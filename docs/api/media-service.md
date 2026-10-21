# 共享媒体服务契约（app/media）

媒体服务统一负责视频和音频的读取与加工：下载来源、完整解码校验、探测、截剪、规整、抽帧、封面、水印、合成、变速、删除中间产物，以及按用途落桶。**调用方只发指令，不碰字节。** 契约以 `app/media/api/media.api`（go-zero .api）为准，本文档是它的说明。

当前状态：落桶已接入 OSS（`app/media/store_oss.go`），未启用 OSS 时退回本地目录，只用于开发。对象按 **rid** 定位：rid 形如 `<所属实例 6 位>-<类别>-<26 位编码>`（例如 `tst001-work-…`），桶内路径为 `v1/<类别>/<对象键>`，对象键由 rid 经 HMAC 派生，不含业务域、用户、任务信息。类别为 `user`、`work`、`deliver`、`asset`，与存储的对应见下文「资源寻址（rid）」。删除只允许 `work`、`deliver`、`asset`，`user` 类别（用户输入）与历史 `sha256/` 路径一律拒绝（403 `forbidden_purpose`）。

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
| `MEDIA_FETCH_TIMEOUT_SECONDS` | 1800 | 单个输入下载的总时长上限（大视频下载可能很慢） |
| `MEDIA_OP_TIMEOUT_SECONDS` | 1800 | 单次请求（下载之外的处理，含 ffmpeg 与校验）的时长上限，也是请求 `deadline_ms` 的上限 |

调用方（Java 的 `app.media-service.timeout-seconds`，环境变量 `MEDIA_SERVICE_TIMEOUT_SECONDS`，默认 1800）的 HTTP 超时必须**大于** `MEDIA_OP_TIMEOUT_SECONDS`，否则会先于媒体服务超时。

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
- **缓存**：下载的输入按 URL 缓存在本实例，最后一次使用后保留 `InputCacheMinutes`（默认 5 分钟）再清理，使用中的文件不会被清。缓存只在本实例内有效，实例之间不共享。
- **客户端版本**：Go 使用 `nacos-sdk-go/v2` v2.2.5，Java 使用 `com.alibaba.nacos:nacos-client` 2.1.0，均为 r-nacos 验证过的版本。

## 鉴权与安全

- 每个请求带调用方令牌，二选一：`Authorization: Bearer <token>`（服务间推荐），或 `X-Media-Token: <token>`（兼容旧调用方）。缺失或错误返回 `401`。服务未配置 token 时拒绝启动。两种写法用的是同一个密钥。
- 只应部署在内网。服务会下载调用方给的地址，默认拒绝回环、内网、链路本地等非公网地址（防 SSRF），线上不得开启 `AllowPrivateHosts`。私有桶的域名要进 `trusted-private-hosts`。
- 依赖 `ffmpeg` / `ffprobe`（容器镜像里已装；裸机运行时需自行安装）。水印需要配置 `LogoPath`。

## 请求约定

- 基础路径：`/v1/mediasvc`，`Content-Type: application/json`。
- `/healthz` 不加前缀，不需要鉴权。
- **写操作**（`ingest`、`cover`、`watermark`、`video/trim`、`video/conform`、`image/conform`、`image/resize`、`compose`、`audio/extract`、`audio/timeline`、`video/replace-audio`、`video/burn-subtitles`、`video/adjust-duration`、`delete`）必须带 `Idempotency-Key` 头，缺失返回 400。幂等规则（按 路由 + 键 索引）：
  - 已成功完成、且请求体一致：原样回放上次的状态码和响应体，响应带 `Idempotent-Replayed: true`，不重复处理；
  - 同一个键但请求体不同：400 `invalid_request`（check=`request.idempotency_key_reused`）；
  - 同一个键正在处理中：503，`Retry-After: 5`，用同一个键稍后重试；
  - 处理过程中 panic：返回 500 `internal`（`retryable: true`），不缓存，同一个键可以重试；
  - 只缓存 2xx 结果，错误不缓存，用同一个键重试会重新执行。
  - 结果保留 `IdempotencyMinutes`（默认 120 分钟）。缓存只在本实例内有效，多实例之间不共享。
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
{"key": "v1/work/xxxxxxxxxxxxxxxxxxxxxxxxxx", "store": "private"}
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
- **地址（读侧现签）**：`POST /v1/mediasvc/resources/urls`，请求体 `{"rid", "store", "mode", "ttl_seconds"}`。`class` 可以省略；如果传了，必须与 rid 一致，否则 `400 class_mismatch`。`store` 必须与类别配对：`asset` → `asset_library`；`user`、`work` → `private`；`deliver` → `private` 或 `third_party`。地址每次现签，调用方不保存。
- **删除（软删除）**：`{"rid": …}` 或 `{"object": {…}}`。只登记待删除标记，字节保留到宽限期（默认 7 天）过后由清理任务物理删除。宽限期内可用 `resources/restore` 撤销。`user` 类别与历史 `sha256/` 路径返回 `403 forbidden_purpose`，且永不删除。
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
| POST | `/v1/mediasvc/frames` | 抽帧，返回每帧的对象 | `FramesResp` |
| POST | `/v1/mediasvc/cover` | 代表帧封面 | `MediaResult` |
| POST | `/v1/mediasvc/watermark` | 右下角叠加 logo | `MediaResult` |
| POST | `/v1/mediasvc/video/trim` | 截到前 `seconds` 秒（重编码，切点精确） | `MediaResult` |
| POST | `/v1/mediasvc/image/resize` | 图片精确尺寸缩放：输出严格为 `width×height`，不保比例。`format` 为 `png` 或 `jpeg`。PNG 仅在源图有 alpha 小于 255 的像素时保留透明通道，否则写 RGB；JPEG 先铺白底。`width`、`height` 各 1..16384，总像素不超过服务端 `MaxSourcePixels`（超过是 `422 source_rejected`，`check=image.target_pixels_exceeded`）。源必须是 PNG/JPEG/WebP，否则 `422`（`check=image.not_image`）；解码失败也是 `422`。必须带 `Idempotency-Key` | `MediaResult` |
| POST | `/v1/mediasvc/image/conform` | 图片规整：长边按区间等比缩放；限定格式时转码（PNG 保留透明）；不需要处理时原样落桶 | `MediaResult` |
| POST | `/v1/mediasvc/video/conform` | 规整：去音轨、帧率、尺寸、比例；可选截剪与过短拒收 | `MediaResult` |
| POST | `/v1/mediasvc/compose` | 按顺序拼接 | `MediaResult` |
| POST | `/v1/mediasvc/audio/extract` | 抽音轨 | `MediaResult` |
| POST | `/v1/mediasvc/video/adjust-duration` | 变速 | `MediaResult` |
| POST | `/v1/mediasvc/delete` | 软删除：登记待删除标记，宽限期后由清理物理删除。请求体 `{"rid": "..."}`（首选）或 `{"object": {...}}`；`user` 类别与历史 `sha256/` 路径一律拒绝（403）。必须带 `Idempotency-Key` | `DeleteResp` |
| POST | `/v1/mediasvc/resources/restore` | 宽限期内撤销待删除，对象恢复可用。请求体 `{"rid": "..."}`；不在待删除状态时返回 `restored: false`（幂等）；宽限期已过返回 `409 grace_expired` | `RestoreResp` |
| POST | `/v1/mediasvc/resources/upload-target` | 申请直传目标（只允许用户输入）：返回 rid 与签名 PUT 地址；目标对象已存在时返回 `409 conflict`，不覆盖已登记的用户对象 | `UploadTargetResp` |
| POST | `/v1/mediasvc/resources/register` | 直传完成后凭 rid 确认：完整解码校验，返回 `mime`、大小、摘要与媒体属性；内容不合法返回 `422 source_rejected`（对象保留，由调用方决定如何处理），对象不存在返回 404 | `MediaResult` |
| POST | `/v1/mediasvc/audio/timeline` | 配音时间线：多段音频按起点拼成整条音轨（m4a） | `MediaResult` |
| POST | `/v1/mediasvc/video/replace-audio` | 换音轨：视频流拷贝，按短的一方截断 | `MediaResult` |
| POST | `/v1/mediasvc/video/burn-subtitles` | 烧录字幕（固定样式，请求体带 srt 文本） | `MediaResult` |
| POST | `/v1/mediasvc/resources/urls` | 凭 rid 现签访问地址（`mode=signed|public`）；不传 class | `ResourceURL` |

参数细节以 `media.api` 为准。例如 `seconds` 的范围是 `(0, 600]`，`factor` 的范围是 `(0, 100]`。

**显式复制（`resources/copy`）**：`POST /v1/mediasvc/resources/copy`，请求体 `{"rid": "<源 rid>", "purpose": "asset_library" | "third_party"}`，必须带 `Idempotency-Key`。源已在私有桶而服务商要公有地址时，Java 显式发复制指令：源被复制到 `asset`（素材库桶）或 `deliver`（第三方桶）类别下的新 rid，新 rid 由幂等键派生，同键重放得到同一个 rid；返回的 `MediaResult` 中 `parents` 为 `["<源 rid>"]`，用于溯源。源不存在返回 `404 not_found`；`purpose` 为 `user_input` 或 `intermediate`（即目标为 user/work 类别）返回 `400 invalid_request`。

### 用途与落桶（purpose）

| purpose | 含义 | 落在哪（store） | 返回的地址 |
|---|---|---|---|
| `user_input` | 用户上传的输入 | 私有桶 | 签名地址（有效期由调用方指定，上限 24 小时） |
| `intermediate` | 中间产物（截剪、规整、抽帧结果） | 私有桶 | 签名地址 |
| `asset_library` | 素材库产物 | 素材库桶（store=`asset_library`，公有） | 公有地址（优先自定义域名） |
| `third_party` | 交付给第三方的产物 | 第三方桶（store=`third_party`，公有） | 公有地址 |

- 具体桶名、对象键前缀由服务端配置决定，调用方不感知。
- 用途对应的类别：`user_input` → `user`，`intermediate` → `work`，`asset_library` → `asset`，`third_party` → `deliver`。
- **用户输入永不删**：`delete` 对 `user` 类别（`user_input` 产生的对象）一律拒绝（`403 forbidden_purpose`）。
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

## 错误

失败时返回：

```json
{"error": {"code": "source_unavailable", "class": "source", "retryable": false, "check": "source.fetch_status", "message": "素材地址无法访问"}}
```

| code | 含义 | Java 工作流对应的失败 | retryable |
|---|---|---|---|
| `source_unavailable` | 源站返回 404 / 410 或其它 4xx（403、408、429 除外）、对象不存在 | 素材不可用 | false |
| `source_rejected` | 不是合法的媒体、解码被拒、超过大小、格式不支持、比例越界 | 素材被拒 | false |
| `source_too_short` | 参考视频比请求的 `seconds` 短（仅 `video/conform`） | 素材被拒 | false |
| `forbidden_purpose` | 不允许的操作（如删除 `user` 类别、删除历史 `sha256/` 路径） | 内部错误 | false |
| `invalid_request` | 参数不合法（含 `class_mismatch`、`store` 与类别不配对） | 内部错误 | false |
| `not_found` | 对象不存在（如 `resources/urls` 时对象尚未落桶） | 待确认 | false |
| `no_public_location` | 资源没有公有地址（`mode=public` 但在私有桶），HTTP 409 | 待确认 | false |
| `resource_deleted` | 对象处于待删除状态（软删除宽限期内），读取与签发地址被拒，HTTP 410 | 素材不可用 | false |
| `grace_expired` | 宽限期已过，不能再撤销（清理任务即将或已经删除它），HTTP 409 | 素材不可用 | false |
| `transient` | 资源紧张、超时、下游暂时不可用；源站 5xx、429、408、403（签名过期、CDN 拦截常见）、连接中断、DNS 临时失败 | 暂时失败 | true |
| `internal` | 未知错误 | 暂时失败 | true |

- `check` 字段给出判据（如 `source.fetch_status=403`、`media.decode_rejected`），便于排障。
- `message` 不含地址、路径、凭证。
- 调用方按 `retryable` 决定是否重试，不再自己推断。

网络层面连不上服务，调用方应按 503 处理。

**删除响应**（`DeleteResp`）：`{"deleted": bool, "state": "pending_delete" | "absent", "delete_after": "<RFC3339>"}`。`state=pending_delete` 表示已进入待删除，`delete_after` 为物理删除的最早时间；对象不存在时 `state=absent`、`deleted=false`，不登记标记。`deleted=true` 表示对象已不可再读（待删除或已删除）。**注意**：旧版本的 `deleted=true` 表示"已物理删除"，现在改为"已进入待删除"，调用方如需物理删除时间以 `delete_after` 为准。

**配置**（`app/media/config.go`）：`DeleteGraceMinutes`（默认 10080，即 7 天）、`SweepIntervalMinutes`（默认 60）。清理任务在每个实例内周期执行，只删宽限期已过的待删除对象，清理是幂等的，多实例并跑无害。

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
