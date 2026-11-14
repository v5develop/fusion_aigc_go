# 可替换位 / 入参 schema v1（广场模板 `slots_json`）

每个模板的入参可以不同，前端按模板动态渲染。实现：`app/plaza/core/slots.go`（Go 类型 + `ParseSlotSchema` + `ValidateInputs`），单测 `slots_test.go`。

## 存储与下发

- 线上形状与存库形状一致：`{"schemaVersion":1,"items":[Slot…]}`（与管理端、用户端前端同口径）。
  读取兼容上线前的旧键 `{"schemaVersion":1,"slots":[…]}` 与裸数组（视为 v1）；`items` 与旧键 `slots` 同时出现拒收。保存时一律规整成 `items` 并补 `schemaVersion`；空 / `[]` / `null` / `items:[]` 存 NULL。
- 校验失败：业务码 `1003`，`reason=SLOTS_INVALID`，`msg` 列出前 5 处（如 `items[2].options[1].value：value 重复：x`），
  `data = {field: 第一处路径, errors: [{field, rule, message}…]}`；路径相对 slots 对象（`items[n].field`、顶层 `schemaVersion` / `items`）。
  同时写日志（`判据=… 路径=… 原因=…`）。**未知字段一律拒收**（拼错字段名不会被悄悄吞掉）。
- 后台：
  - `GET /api/v1/plaza/admin/templates/{id}` 回显 `slots`（规整后的 `{schemaVersion, items}`；没有为 `null`；库里是旧键时转成 `items` 回显；库里是坏数据时原样回显并打日志）。
  - `PUT /api/v1/plaza/admin/templates/{id}/slots` **只改入参**：body `{"slots":{"schemaVersion":1,"items":[…]},"updatedAt":"<详情里的 updateTime>"}`；
    成功返回 `{id, slots, updatedAt}`（新的 updateTime，下次保存带上）。`updatedAt` 与库里不一致 → `4002 · reason=CONFLICT`「他人已修改，刷新后再试」；
    缺省不做并发校验。`slots` 为 null / `items:[]` = 清空。受 `Plaza.AdminWriteEnabled` 保护（同其它模板写接口）。权限 `creaft_catalog:edit`。
  - 整体编辑 `PUT /api/v1/plaza/admin/templates/{id}`：**不带 `slots` 键 = 保留原入参**；显式 `null` / `[]` / `items:[]` = 清空；带了就整体替换并按同一套规则校验。
    新建 `POST /templates` 不带 = 没有入参。
- 用户端：`GET /api/v1/plaza/templates/{id}`（模板详情）下发 `slots`（同上形状）；**浏览列表 / 最近用过 / 头条不下发**。
- 做同款准备：`POST /api/v1/plaza/templates/{id}/prepare`，body `{"inputs":{key:值}}`，按同一份 schema 校验并返回规整后的 `{"inputs":{…}}`。错误带路径 `inputs.<key>`（`reason=PARAM_INVALID`）。
- v1 兼容路径与 v1 payload 不动（slots 不进 `data_json`）。

## 通用字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `key` | string | 必填，模板内唯一，`^[a-z][a-z0-9_]{0,31}$` |
| `type` | string | 必填：`image` `video` `text` `textarea` `price` `number` `select` `multiselect` `switch` `color` |
| `label` | string | 必填，≤32 字 |
| `help` / `placeholder` | string | 可选，≤200 / ≤100 字 |
| `required` | bool | 默认 false |
| `default` | 随类型 | 可选；按类型校验；`image` / `video` 不支持 |
| `order` | int | 0~9999，保存时按它稳定排序 |
| `group` | string | 可选分组名，≤16 字 |
| `visibleWhen` | `{key, equals}` | 可选，单层条件显示：`key` 必填，必须指向本模板内**另一个已存在**的入参（`select` / `switch` / `text`，且自己没有 `visibleWhen`，不能是自己；可以是排在后面的项）；`equals` 必填，类型须与被引用项一致：`switch` → `true` / `false`，`select` → 它的某个合法选项值，`text` → 字符串（不能是 null / 对象 / 数组）。被引用项的取值（未填取其 default）等于 `equals` 才显示 |

## 类型专属字段（用在别的类型上会被拒收）

| 类型 | 字段 | 约束与默认 |
|---|---|---|
| `image` / `video` | `minCount` `maxCount` | maxCount 1~9，默认 1；minCount 默认 `required?1:0`；必填时 minCount≥1；minCount≤maxCount |
| | `guidance` `exampleUrl` `acceptRatio` | 拍摄指引 ≤200 字；示例图 http(s)；比例形如 `正整数:正整数`（如 `9:16`、`1080:1920`；不允许 0、小数、前导 0、空格，每边 ≤5 位） |
| `video` | `maxDurationSec` | 1~600，默认 60 |
| `text` / `textarea` | `maxLength` | 1~2000，默认 100 / 500 |
| | `layout` | **仅 `text`**（textarea 带 layout 拒收）：文字叠加版式，见下「layout v1」；本规格之外的键原样保存 |
| `number` / `price` | `min` `max` `step` `unit` | min≤max，step>0，unit ≤8 字；`price` 的 min 缺省为 0 |
| `select` / `multiselect` | `options[{value,label}]` | 非空，≤50，value 唯一（≤64 字符），label ≤32 字；`multiselect` 可带 `maxCount`（1~选项数） |
| `switch` | — | default 为布尔 |
| `color` | — | default 为 `#RRGGBB` |

### layout v1（`type=text`，模板 `text_overlay=1` 时出图后按它叠字；全是相对画面的比例）

| 字段 | 约束 |
|---|---|
| `box{x,y,w,h}` | **必填**；x,y∈[0,1]，w,h∈(0,1]，x+w≤1，y+h≤1 |
| `size` | 字号，相对画面高度 [0,0.5]；缺省/0 = 按框高自适应 |
| `color` | `#RGB` `#RGBA` `#RRGGBB` `#RRGGBBAA`，缺省白 |
| `align` / `valign` | `left\|center\|right`（缺省 center）/ `top\|middle\|bottom`（缺省 middle） |
| `stroke{color?,width}` | width 相对画面高度 [0,0.05] |
| `maxLines` / `lineHeight` | 1~10 / 0.8~3 |
| `fontRid` | 媒体服务字体 rid（`xxxxxx-(user\|work\|deliver\|asset)-<26 位 base32>`）；缺省用 `Plaza.Overlay.DefaultFontRid` |
| `defaultText` | ≤100 字，样例 / 默认文字 |

保存时逐项校验，错误路径形如 `items[0].layout.box.x`、规则码 `layout.box_range` 等；`textOverlay` 为 0 时 layout 仍可存（不生效）。

一个模板最多 20 个入参。

### 保存时的 default 校验（与管理端前端同口径）

`default` 必须与 `type` 匹配，否则 `items[n].default` 拒收：`select` 的 default 必须是某个选项的 value；`multiselect` 必须是选项值数组的子集（不重复、≤maxCount）；
`number` / `price` 必须是 JSON 数字且在 `min`~`max` 内（`price` 的 min 缺省为 0；有 `step` 时还须是 step 整倍数）；`switch` 必须是布尔；`color` 必须是 `#RRGGBB`；
`text` / `textarea` 必须是文字且 ≤maxLength；`image` / `video` 不支持 default。必填的 `image` / `video` 位 `minCount` 必须 ≥1（缺省自动补 1）。

## 提交校验规则（`ValidateInputs`）

未知 key 拒收；`visibleWhen` 不可见的项**忽略**（丢弃其值、不要求必填）；缺省值取 `default`；必填项为空（缺省 / `null` / 空串 / 空数组；`switch` 只有缺省与 `null` 算空，`false` 是有效取值）拒收；入参键数超过 40（`maxSlots×2`）直接拒收，错误最多返回 20 条；
image / video：值为地址或地址数组，数量在 `minCount~maxCount`，必须是 http(s)；文字：去首尾空白、≤maxLength；
number / price：必须是 JSON 数字，在 min~max 内，按 step 整倍数（从 min 起算）；select：必须是选项值；multiselect：选项值数组、不重复、≤maxCount；switch：布尔（`false` 是有效值）；color：`#RRGGBB`（规整为大写）。
多处错误一次说清（最多列 5 处）。

## 示例 1：足浴门头图模板（1 张门头照 + 店名）

```json
{"schemaVersion":1,"items":[
 {"key":"storefront","type":"image","label":"门头照片","required":true,"order":1,"maxCount":1,
  "guidance":"正对门头，白天拍，别逆光","acceptRatio":"4:3"},
 {"key":"shop_name","type":"text","label":"店名","required":true,"order":2,"maxLength":12,"placeholder":"例如：悦足堂"}]}
```

## 示例 2：服装上新模板（1–3 张商品图 + 价格 + 风格 select）

```json
{"schemaVersion":1,"items":[
 {"key":"products","type":"image","label":"商品图","required":true,"order":1,"minCount":1,"maxCount":3,"group":"商品"},
 {"key":"price","type":"price","label":"到手价","required":true,"order":2,"min":0.01,"max":99999,"step":0.01,"unit":"元","group":"商品"},
 {"key":"style","type":"select","label":"风格","order":3,"default":"fresh",
  "options":[{"value":"fresh","label":"清新"},{"value":"luxury","label":"轻奢"},{"value":"street","label":"街头"}]},
 {"key":"show_price","type":"switch","label":"海报上显示价格","order":4,"default":true},
 {"key":"price_color","type":"color","label":"价格颜色","order":5,"default":"#ff3b30","visibleWhen":{"key":"show_price","equals":true}}]}
```

两个示例同时是 `slots_test.go` 的测试数据（`footSchema` / `apparelSchema`）。
