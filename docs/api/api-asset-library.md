# 资产库增强 · 前端接口文档

资产库在现有「素材库」（Java `/api/v1/asset`）之上加了一层：分类（kind）、资产级收藏、标签字典、版本、资产关系与详情。

| 能力 | 接口 |
|---|---|
| 列表 + 筛选 + 分页 | `GET /assets` |
| 分类条计数 | `GET /assets/categories` |
| 详情（版本 / 相关资产 / 生成结果） | `GET /assets/:id` |
| 改标题 / 分类 / 收藏 / 标签 / 元数据 / 授权 / 当前版本 | `PATCH /assets/:id` |
| 追加版本 | `POST /assets/:id/versions` |
| 资产关系 | `POST` / `DELETE /assets/:id/relations` |
| 标签字典 | `GET` / `POST /tags`、`PUT` / `DELETE /tags/:id` |
| 批量打标签 | `POST /assets/batch-tag` |
| 删除资产（单个 / 批量） | `DELETE /assets/:id`、`POST /assets/batch-delete` |

> 本文以 `app/gateway/api/gateway.api` 的 assetlib 组为准，字段名一律下划线。实现与本文不一致时，以接口定义为准，并请通知后端更新本文。
> 资产本身的上传仍走 Java 的 `/api/v1/asset`；删除可走本模块（3.3，逻辑删除，与 Java 删除同口径）。管理后台的接口见最后一章。

---

## 0. 概述

### 0.1 数据从哪来

- 资产就是素材库里的那些行（Java 表 `afv_asset`）：AI 生成的成品、用户上传的图片 / 视频。**本模块只读它们**，唯一的例外是改标题（见 3.1）。
- 分类、收藏、标签、版本、关系存在 Go 的旁表里，按资产 id 挂接。一条资产从没被分类 / 收藏过时，旁表里没有它，读出来就是「未分类、未收藏」。
- 只能看到、改到**自己的**资产（`afv_asset.user_id` = 当前用户、未删除、非草稿）。别人的资产、已删除的资产、不存在的 id，一律返回 `4001` 资产不存在，不区分「无权限」。

### 0.2 与原型 `Asset` 类型的对应

形状以原型 `useAssetStore.ts` 的 `Asset` 为准，字段改下划线：

| 原型字段 | 本接口字段 | 说明 |
|---|---|---|
| `id` | `id` | 数字 |
| `kind` | `kind` | `model` / `scene` / `makeup` / `hair` / `accessory`；**空串 = 未分类** |
| `title` | `title` | 素材库里的名称 |
| `prompt` | `prompt` | 生成时的提示词（上传的为空） |
| `image` | `image` | 展示图：设了当前版本时为该版本的图，否则为原图；视频为封面 |
| `media` | `media` | `image` / `video` |
| `videoUrl` | `video_url` | 仅 `media=video` 时有值 |
| `source` | `source` | `generated`（AI 生成）/ `upload`（用户上传）/ `official`（见 1.5） |
| `tags` | `tags` | 标签名数组，没有为 `[]` |
| `createdAt` | `created_at` | unix 毫秒 |
| `favorite` | `favorite` | 资产级收藏 |
| `license` | `license` | `cleared` / `internal` / `unknown`；空串 = 未设置 |
| `metadata` | `metadata` | `{industry, style, shot, color}`，没有的为空串 |
| `provenance` | `provenance` | `{run_id, workflow_id, node_id, operator, model, params, created_at}`；`params` 是 JSON 文本。AI 生成但没写过溯源的，`run_id` 填来源任务号 |
| `versions` | `versions` | **只在详情里有**（当前页），列表里为 `[]` |
| `currentVersionId` | `current_version_id` | `0` = 原图 |
| `relatedIds` | `related_ids` | **只在详情里有**（相关资产当前页的 id） |
| — | `entry_code` / `width` / `height` | 额外给出：创作入口编码、原图宽高（0 = 未知） |

原型里的两处行为改成了服务端口径：

1. **「未分类」= `kind` 为空**，不是「没有标签」。计数由服务端给（`GET /assets/categories`）。
2. **版本不再整数组 PATCH**：追加用 `POST /assets/:id/versions`，切换当前版本用 `PATCH current_version_id`。
3. **相关资产不再前端启发式**（同 kind + 同 style / industry），只认显式建立的关系（第 5 章）。

---

## 1. 通用约定

- 网关：和其它接口同一个网关域名，前缀 **`/api/v1/asset-library`**（Java 素材库是 `/api/v1/asset`，不是同一个）。下文路径都省略前缀，如 `GET /assets` 即 `GET /api/v1/asset-library/assets`。

### 1.1 鉴权

所有接口带 `Authorization: Bearer <token>`，和 AIGC 服务同一套令牌（见 `api-app.md` 登录一章）。联调环境（`Auth.Mode=dev`）没带 token 时认 `X-User-Id: <用户ID>`，线上会关掉。

未登录 / 令牌过期：**HTTP 401**，`code=2001`。这是唯一不返回 HTTP 200 的情况。

### 1.2 响应信封

```json
{"code": 0, "msg": "ok", "data": { }}
```

失败时多一个 `reason`（和 Java 的 ErrorCode 常量名一致），前端按 `reason` 分支：

```json
{"code": 4001, "reason": "RESOURCE_NOT_FOUND", "msg": "资产不存在", "data": null}
```

**HTTP 状态码一律 200，用 `code` 判断；只有未登录是 HTTP 401。** 业务失败的 `msg` 可以直接展示。

### 1.3 字段、时间、分页

- 请求体、查询参数、响应一律**下划线**命名；时间都是 **unix 毫秒**。
- 列表分页：`page`（从 1 起，默认 1）+ `page_size`（默认 20，上限 100；原型每页 15 就传 15），返回 `items` + `total`。
- 详情里的三组子列表（版本、相关资产、生成结果）各自分页：`version_page` / `related_page` / `result_page`（默认 1），共用 `page_size`（默认 6，上限 50），每组返回 `{items, total}`。

### 1.4 参数限制

| 项 | 限制 |
|---|---|
| 标题 `title` | 去首尾空白后 1–100 字 |
| 标签名 | 去首尾空白后 1–20 字；同一用户内不重名（不分大小写） |
| 每个用户的标签数 | ≤200 |
| 每个资产的标签数 | ≤10 |
| `metadata` 每项 | ≤50 字 |
| 关键字 `q` | ≤100 字，匹配标题与提示词 |
| 版本图片 `image_url` | 必须是 http(s) 地址，≤1024 字符；`blob:` / `data:` 拒绝（先上传再传地址）；还必须是本服务上传接口签发的地址（或配置白名单 `InputURLPrefixes` 内的素材库公有桶地址），外链返回 1003「image_url 不是本服务上传的地址」 |
| 版本提示词 | ≤5000 字 |
| 每个资产的版本数 | ≤100 |
| `params` | 合法 JSON 文本，≤16KB |
| 批量打标签 | 一次 ≤100 个资产、≤10 个标签 |

### 1.5 来源 `source` 与媒体 `media`

| `source` 取值 | 含义 | 素材库里的口径 |
|---|---|---|
| `generated` | AI 生成 | `source_type = 2` |
| `upload` | 用户上传 | `source_type = 1` |
| `official` | 官方 / 系统 | `source_type` 为空或其它值。**个人素材库目前没有官方素材**（官方素材在公共模板里，不进个人素材库），所以基本为 0 |
| `input`（仅筛选） | 原型「我的素材」页签 = `official` + `upload` | |

`media` 与 Java 素材库的「图片 / 视频」页签同一口径（按创作入口的计费类目判定，未登记的入口看名字里有没有 video）。

### 1.6 错误码

| code | reason | 含义 | 前端怎么处理 |
|---|---|---|---|
| 0 | — | 成功 | — |
| 1002 | PARAM_MISSING | 缺必填（标题为空、标签名为空、缺图片地址、批量打标签的 id 为空） | 展示 `msg` |
| 1003 | PARAM_INVALID | 参数不合法（枚举取值不对、超长、超数量、版本不属于该资产） | 展示 `msg` |
| 2001 | UNAUTHORIZED | 未登录或登录过期（**HTTP 401**） | 跳登录 |
| 4001 | RESOURCE_NOT_FOUND | 资产 / 标签 / 关系不存在，或不属于当前用户 | 刷新列表 / 返回上一页 |
| 4002 | RESOURCE_CONFLICT | 标签重名 | 展示 `msg` |
| 5001 | SYSTEM_ERROR | 服务繁忙 | 提示稍后重试 |

---

## 2. 列表与分类

### 2.1 列表 `GET /assets`

| 参数 | 说明 |
|---|---|
| `kind` | 空 = 全部；`uncategorized` = 未分类；或 5 个 kind 之一；或视频二级分类键之一（见下，只读筛选） |
| `kind_group` | 空 = 不限；`image` = 5 个图片 kind（`kind IN (model, scene, makeup, hair, accessory)`）；`video` = 视频二级分类键。与 `kind` 同传时两者同时生效 |
| `entry_codes` | 空 = 不限；逗号分隔的创作入口码，按素材的 `entry_code IN (...)` 筛选（列表与 `total` 同口径）。每项须匹配 `[A-Za-z0-9_.-]{1,64}`，最多 200 个；空段忽略、重复去重；格式不对或超量返回参数错误。前端分类条传所选分类节点的入口码（见 2.3） |
| `source` | 空 = 全部；`input` / `generated` / `official` / `upload` |
| `media` | 空 = 全部；`image` / `video` |
| `aspect_ratio` | 空 = 不限；比例 `宽:高`（各 1~2 位数字，如 `1:1` / `3:4` / `4:3` / `9:16` / `16:9`），按素材的 `aspect_ratio` 精确匹配；格式不对返回参数错误 |
| `tag` | 标签名（精确匹配） |
| `untagged` | `true` 只看没挂任何标签的资产（「未分类」，口径同 `GET /tags` 的 `untagged_count`）；不能与 `tag` 同传（`1003`） |
| `favorite_only` | `true` 只看收藏 |
| `q` | 关键字（标题 / 提示词，模糊） |
| `page` / `page_size` | 见 1.3 |

按创建时间倒序。

视频二级分类键（`kind_group=video` 包含的全部 kind，前端「AI生视频」二级分类按这个顺序展示）：
`viral_video` 爆款视频、`video_remake` 视频复刻、`promo_video` 带货视频、`digital_human` 数字人、`digital_human_talk` 数字人口播、`store_visit` 探店视频、`promo_drama` 带货短剧、`motion_transfer` 动作迁移、`canvas_video` 画布视频、`video_other` 其他。

> **待定**：视频资产如何归入这些二级分类（数据来源）尚未确定，这些键目前不能写入资产的 `kind`，`/assets/categories` 也不返回它们。定下来之前 `kind_group=video` 与这些 `kind` 都查不到资产。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "items": [{
      "id": 1351, "kind": "model", "title": "夏季连衣裙-1", "prompt": "…",
      "image": "https://cdn.example.com/a.png", "media": "image", "video_url": "",
      "source": "generated", "tags": ["夏季"], "created_at": 1761840000000,
      "favorite": true, "license": "",
      "metadata": {"industry": "服装", "style": "日系", "shot": "", "color": ""},
      "provenance": {"run_id": "tyo_…", "workflow_id": "", "node_id": "", "operator": "", "model": "", "params": "", "created_at": 1761840000000},
      "versions": [], "current_version_id": 0, "related_ids": [],
      "entry_code": "studio_tryon", "width": 1024, "height": 1365
    }],
    "total": 274
  }
}
```

### 2.2 分类条计数 `GET /assets/categories`

参数：`source`、`media`（同列表）。计数**随这两个条件变化**，固定返回 7 项、固定顺序：

```json
{"code": 0, "msg": "ok", "data": {"items": [
  {"key": "all", "label": "全部", "count": 274},
  {"key": "model", "label": "模特", "count": 3},
  {"key": "scene", "label": "场景", "count": 0},
  {"key": "makeup", "label": "妆容", "count": 0},
  {"key": "hair", "label": "发型", "count": 1},
  {"key": "accessory", "label": "配饰", "count": 0},
  {"key": "uncategorized", "label": "未分类", "count": 270}
]}}
```

点某个分类时，把 `key` 作为列表的 `kind` 传（`all` 不传 `kind`），两边计数一致。

前端分类条按大类分两级：一级「全部 / 创作素材（`kind_group=image`）/ AI生视频（`kind_group=video`）/ 其他分类（`kind=uncategorized`）」，选中大类且未选二级时列表传 `kind_group`；选中二级时传该二级的 `kind`。大类计数由前端把二级计数相加（本接口不变；`aspect_ratio` 不影响分类计数）。

### 2.3 我的资产分类条（Creaft v1）

分类树来自 Java `GET /api/v1/asset/categories`（需登录 token），返回 `List<AssetCategoryRespVO>`：
一级节点 `{id, categoryCode, categoryName, sortOrder, entryCodes, children, count, entries}`，二级节点同结构、`children` 恒为 `[]`，`entries` 为 `[{entryCode, entryName}]`（只二级有）。
二级里编码以 `__other` 结尾的「其他」是后端合成的虚拟分类（`id` 为 null，渲染 key 用 `categoryCode`）。

前端：首项「全部」（不传 `entry_codes`），其后每个一级一个 chip（`categoryName` + `count`）；选中有二级的一级时显示其二级 chip，
「全部」= 该组各二级 `entryCodes`（缺省时取一级 `entryCodes`）之并；选中二级或无二级的一级时传该节点 `entryCodes`。
入口码一律以 `entry_codes` 传给本列表接口；分类树取不到或为空时分类条只剩「全部」，列表照常加载。

> 口径差异：Java `count` 按「可访问范围」计数，本列表只查本人素材，两边条数可能不一致。

---

## 3. 详情与修改

### 3.1 详情 `GET /assets/:id`

参数：`version_page`、`related_page`、`result_page`、`page_size`（见 1.3）。

```json
{
  "code": 0, "msg": "ok",
  "data": {
    "asset": { /* 同列表项，另带 versions（版本当前页）与 related_ids（相关资产当前页 id） */ },
    "versions": {"items": [{"id": 12, "version_no": 2, "image": "https://…/v2.png", "prompt": "…", "model": "Banana", "params": "", "created_at": 1761840000000}], "total": 2},
    "related": {"items": [ /* Asset */ ], "total": 1},
    "generated_results": {"items": [ /* Asset */ ], "total": 0}
  }
}
```

- `versions` 新的在前。原图不算版本：没有版本时 `total=0`，展示原图。
- `related`：和它有 `reference` 关系的资产（两个方向都算）+ 它 `generated_from` 的来源资产。
- `generated_results`：`generated_from` 指向它的资产（由它生成的结果）。
- 两组里只出现当前用户自己、未删除的资产。

### 3.2 修改 `PATCH /assets/:id`

**只改带了的字段**，没带的不动。返回修改后的完整 `Asset`。

| 字段 | 说明 |
|---|---|
| `title` | 1–100 字。写回素材库名称，Java 素材库里也会看到新名字 |
| `kind` | 5 个 kind 之一；**传空串 = 改回未分类** |
| `favorite` | `true` / `false` |
| `tags` | 标签名数组，**整组替换**（传 `[]` 清空）。字典里没有的名字会自动建标签（受每用户 200 个上限约束），≤10 个，重名去重 |
| `metadata` | `{industry, style, shot, color}`，**整体替换**（没带的子字段视为空） |
| `license` | `cleared` / `internal` / `unknown`，空串 = 清除 |
| `current_version_id` | 该资产的某个版本 id；`0` = 回到原图。别的资产的版本 → `1003` |

```http
PATCH /api/v1/asset-library/assets/1351
{"kind": "model", "favorite": true, "tags": ["夏季", "通勤"], "metadata": {"style": "日系"}}
```

原型 v1.0 里 PATCH 只改名 / 收藏，其余字段按需接入即可。

---

### 3.3 删除 `DELETE /assets/:id`、`POST /assets/batch-delete`

- 只删**本人的上传 / AI 生成**资产（`source` 为 `upload` / `generated`）；**官方素材不可删除**。
- 逻辑删除（`afv_asset.deleted = 1`，与 Java 删除同口径），同一事务解除该资产与本人标签的关联；**不删存储对象**、不动任务结果（免费档原图 / 水印副本挂在任务与存储上，不受影响）、不删版本 / 关系 / 分类旁表（读侧只认未删除资产）。删除后列表、各计数、详情都看不到它。
- 单删：不存在 / 不是自己的 → `4001`；官方素材 → `2005`「官方素材不可删除」。
- 批量 body `{"asset_ids": [1351, 1352]}`（≤100）：官方素材与不存在的 id **静默跳过**并分别计数，不算错误。

两者返回同形：

```json
{"deleted": 2, "official_skipped": 1, "missing": 0, "deleted_ids": [1351, 1352]}
```

## 4. 版本

### 4.1 追加版本 `POST /assets/:id/versions`

```json
{"image_url": "https://cdn.example.com/a-v3.png", "prompt": "…", "model": "GPT", "params": "{\"seed\":1}", "image_rid": ""}
```

- `image_url` 必填，其余可选；`params` 是 JSON 文本（前端原样存取）。
- `version_no` 由服务端按「该资产现有最大版本号 + 1」分配，从 1 开始。
- **追加的版本自动成为当前版本**（`current_version_id` 指向它，`image` 随之变化）；要回到旧版本或原图用 `PATCH current_version_id`。
- 返回新版本 `{id, version_no, image, prompt, model, params, created_at}`。
- 暂不提供删除版本。

---

## 5. 资产关系

| 方法 | 路径 | 参数 |
|---|---|---|
| POST | `/assets/:id/relations` | body：`{"related_asset_id": 1352, "relation_type": "reference"}` |
| DELETE | `/assets/:id/relations` | 查询参数：`?related_asset_id=1352&relation_type=reference` |

`relation_type`：

| 取值 | 含义 | 详情里怎么展示 |
|---|---|---|
| `reference` | 互为参考 | 两边详情的 `related` 里都有对方；从任一端删除都会删掉 |
| `generated_from` | `:id` 是由 `related_asset_id` 生成的 | `related_asset_id` 的 `generated_results` 里有 `:id`；`:id` 的 `related` 里有 `related_asset_id` |
| `version_of` | `:id` 是另一资产的另一版本 | 只记录，不在详情里展示（版本以第 4 章为准） |

- 两端都必须是自己的资产，否则 `4001`；不能和自己建关系（`1003`）。
- 重复建同一关系不报错；删除不存在的关系返回 `4001`。

---

## 6. 标签

### 6.1 我的标签 `GET /tags`

```json
{"code": 0, "msg": "ok", "data": {"items": [{"id": 5, "name": "夏季", "asset_count": 12, "created_at": 1761840000000}], "untagged_count": 30}}
```

按名字排序；`asset_count` 只数未删除的资产。
`untagged_count`：本人没挂任何标签的资产数（标签弹层「未分类」的计数），归属 / 删除 / 草稿口径同 `GET /assets`。

### 6.2 新建 / 重命名 / 删除

| 方法 | 路径 | body | 说明 |
|---|---|---|---|
| POST | `/tags` | `{"name": "夏季"}` | 返回新标签。重名 `4002`；超过 200 个 `1003` |
| PUT | `/tags/:id` | `{"name": "盛夏"}` | 返回改名后的标签。资产上的标签名随之变化 |
| DELETE | `/tags/:id` | — | 同时从所有资产上摘掉这个标签 |

### 6.3 批量打标签 `POST /assets/batch-tag`

```json
{"asset_ids": [1351, 1352], "tag_ids": [5, 6]}
```

- **追加**，不删资产已有的标签；已经有的跳过。返回 `{"added": 3}`（新增的关联数）。
- 任一资产不是自己的 → `4001`；任一标签不是自己的 → `4001`；任一资产追加后会超过 10 个标签 → `1003`，整批不生效。
- 要从多个资产上摘掉标签：逐个 `PATCH tags`，或直接删标签。

---

## 7. 管理侧接口简表（给后台同事）

前缀 `/api/v1/asset-library/admin`（由 admin-api 处理，nginx 单独转发），鉴权为后台账号登录态，信封和错误码同第 1 章。权限码：`GET` 要 `asset_library:view`，其它要 `asset_library:update`（迁移 `Go_V1.0.24`；super_admin 两个都有，operator 只有 view）。字段定义见 `app/admin/api/admin.api` 的 assetlib 组。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/tags` | 全部用户的标签字典。查询参数：`page`、`page_size`、`user_id`（0 = 全部）、`q`（标签名模糊）。返回 `list` + `total`，每项 `id`、`user_id`、`name`、`asset_count`、`created_at`，新建的在前 |
| GET | `/kind-stats` | 分类统计。查询参数：`user_id`（0 = 全部用户）、`media`（`image` / `video`）。返回 `rows`（kind × source 的计数，kind 为 `uncategorized` 表示未分类）、`kinds`（按 kind 汇总，含 `all`，固定 7 项）、`sources`（按来源汇总：`upload` / `generated` / `official`）。只排除已删除的资产 |
