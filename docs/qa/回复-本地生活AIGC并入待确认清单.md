# 回复：本地生活 AIGC 并入 AIGC 后端 · 待确认事项

> 回复对象：App 端 ｜ 对应清单：`local_life_merge_questions.md`（2026-10-17，2026-10-18 更新）
> 口径：**Go 是 Java 的延伸，契约以 Java 为准。** 错误码、信封、登录行为与 Java 一致；Go 只补充 Java 没有的部分，且新码必须先登记在 Java `ErrorCode` 里。
> 状态说明：**已完成** = 代码已改，本地测试通过，未部署；**待部署** = 依赖发版；**待定** = 需要你们确认。

---

## 先更正两点

1. **两个后端没有合成一个代码库。** Go 仍是独立进程，与 Java 通过同一套令牌（同一个 Redis 会话、同一张用户表 `afv_app_user`）互通。响应信封、错误码、登录行为对齐的是**契约**，不是代码。
2. **用户 ID 没有分裂。** 本地生活登录和 AIGC 登录写的是同一张用户表，`userId` 相同，**原来用本地生活令牌产生的数据，换成 AIGC 令牌后仍然能查到**（见第 5 条）。

---

## 1. 令牌失效的判定 · 已完成（待部署）

- 本地生活接口令牌失效：**HTTP 401 + `code 2001` + `reason UNAUTHORIZED`**。
- 原来的 `1002` 在本地生活接口里**不再表示未登录**，`1002` 现在只表示「缺少必填参数」（`PARAM_MISSING`），与 AIGC 一致。
- App 可以只判断 `HTTP 401` 或 `code 2001`，二者任一即刷新令牌、重放一次。

## 2. 「需同意协议」的业务码与接口 · 已完成（待部署）

- **协议接口由本地生活提供**，AIGC 不新增。路径 `GET /api/v1/local-life/agreements/aigc`、`POST /api/v1/local-life/agreements/aigc/accept` **不变**。
- 出入参沿用本地生活现有定义：出参为 `{code, version, accepted}`。AIGC 没有对应接口，所以不存在需要逐字段核对的对象，**以此为准**。
- 业务码：**`3011`**，`reason` 为 **`AGREEMENT_REQUIRED`**。已登记在 AIGC 的 `ErrorCode`（3xxx 业务规则）。
- App 请按 **`reason`** 分支，不要按数字。

## 3. 幂等请求头 · 已完成（待部署）

- 幂等键放在 **`Idempotency-Key`**（UUID，重试沿用同一个值），与 AIGC 口径一致。App 切换到这个头即可。
- **`X-Request-Id` 只用于链路追踪**，不参与幂等。旧版本只带它时，服务端会用它补成幂等键，所以旧版本的重试仍然幂等。
- 两个头都带且值不同：**不报错**，以 `Idempotency-Key` 为准做幂等，`X-Request-Id` 原样保留用于链路追踪。
- `X-Request-Id` 作为旧幂等键来源的过渡期截止日期**待定**，不阻塞当前发版，定下来后告诉我们。

## 4. 其他业务码与信封 · 已完成（待部署）

**信封**与 AIGC 的 `CommonResult` 一致：

```json
成功：{"code":0,"msg":"ok","data":{...}}
失败：{"code":2001,"reason":"UNAUTHORIZED","msg":"登录已过期","data":null}
```

- **成功只认 `code == 0`**。
- 失败时 `reason` 是 AIGC `ErrorCode` 的常量名，App 请按它分支。
- 未登录返回 HTTP 401，其它业务失败保持 HTTP 200（不按 HTTP 状态判业务）。

**业务码对照（本地生活旧码 → 统一码）：**

| 旧码 | 含义 | 统一码 | reason | 说明 |
|---|---|---|---|---|
| 0 | 成功 | 0 | SUCCESS | 不变 |
| 1000 | 系统错误 | 5001 | SYSTEM_ERROR | |
| 1001 | 参数错误 | 1003 | PARAM_INVALID | |
| 1002 | 未登录 | **2001** | UNAUTHORIZED | **码变了**，`1002` 现在是缺参数 |
| 1002 | 缺必填参数 | 1002 | PARAM_MISSING | 新增语义，原值不变 |
| 1003 | 不存在 | **4001** | RESOURCE_NOT_FOUND | **码变了**，`1003` 现在是参数无效 |
| 1004 | 无权限 | **2005** | ACCESS_DENIED | 码变了 |
| 1005 | 冲突 | **4002** | RESOURCE_CONFLICT | 码变了 |
| 1006 | 暂未开放 | **7003** | FEATURE_NOT_READY | 码变了 |
| 1007 | 需同意协议 | **3011** | AGREEMENT_REQUIRED | 码变了 |
| 1008 | 点数不足 | **3003** | INSUFFICIENT_POINTS | 码变了 |

> 注意：`1003` 和 `1002` 在旧版本里的含义与现在**不同**，App 里如果有按数字写死的判断，请全部改成按 `reason` 判断。

## 5. 登录 · 请改用 AIGC 的登录接口（已完成，待部署）

**推荐做法：App 直接调用 AIGC 的登录接口，不再调用本地生活的登录接口。**

- 接口：`POST /api/v1/auth/union-token-login`，无需登录即可调用。
- 请求：`{"token": "<联盟临时 token>"}`。临时 token 的获取方式不变（前端仍先向联盟换取）。
- 响应：`LoginRespVO`，字段为 `accessToken`、`refreshToken`、`expiresIn`（秒）、`refreshExpiresIn`（秒）、`userId`、`username`、`nickname`。
- 错误码与本地生活登录一致：临时 token 无效或已用过 → 1003（文案为联盟返回的内容）；联盟不可用 → 6001；账号禁用 → 2007。
- 刷新令牌走 AIGC 的 `POST /api/v1/auth/refresh`。

**本地生活的登录接口（`POST /api/v1/local-life/auth/login`）已弃用**，只为旧版本保留，行为与上面一致（请求字段 `token` 与旧字段 `temp_token` 都接受，响应同时包含旧字段与 AIGC 字段）。新版本不要再调用它。

- 会话上限：Go 与 Java 都按「超出上限踢掉最久未刷新的会话」执行。Java 读取的配置项是 `app.security.token.max-concurrent-sessions`，对应环境变量 **`APP_SECURITY_TOKEN_MAX_CONCURRENT_SESSIONS`**（默认 0，即不限）。**变量名在代码层面已核对，Java 生产环境是否已设置、取值是多少，需要你们在生产环境确认。**

## 6. 用户老数据 · 已确认无需迁移（待部署）

- 本地生活登录与 AIGC 登录写入的是同一张用户表 `afv_app_user`，**同一个人的 `userId` 一致**。
- 因此：**原来用本地生活令牌产生的数据，换成 AIGC 令牌后，仍然属于同一个用户，能正常查到。**
- 之前担心的「两套用户 ID」在代码核对后不成立，特此更正。

## 7. 旧接口下线时间 · 待定

- 本地生活登录接口已弃用，**下线时间未定**。
- 请先确认 App 版本分布，再定下线日期。在此之前，旧版本仍可使用该接口。
- 定下来后请告诉我们目标日期。

---

## 需要你们确认的

1. 第 3 条：`X-Request-Id` 作为旧幂等键来源的截止日期（不阻塞当前发版；它在链路追踪中仍然保留）。
2. 第 5 条：Java 生产环境 `APP_SECURITY_TOKEN_MAX_CONCURRENT_SESSIONS` 的实际取值（需要在生产核对）。
3. 第 7 条：旧登录接口的下线时间。

## 部署依赖

- **Java 侧**：`ErrorCode` 新增 `AGREEMENT_REQUIRED(3011)` 与 `FEATURE_NOT_READY(7003)`，需要随 AIGC 发版。App 登录改用 Java 接口后，**不依赖 Go 发版**。
- **Go 侧**：本地生活网关与后台随本次发版上线，包括幂等头中间件（同时接受 `Idempotency-Key` 与 `X-Request-Id`）和统一错误码。登录接口仅为旧版本保留。
