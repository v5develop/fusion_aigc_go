# 运营后台接口：登录

后台地址：`http://<host>:8889`（服务器部署默认 18889），所有接口前缀 `/api/v1/local-life/admin`。

**在线文档（Swagger，可调试）**：`http://<host>:<端口>/swagger`，先点「Authorize」填登录接口返回的 token。

## 账号

用的是**现有后台的账号表**（`afv_admin_user`），密码也是原来那套（BCrypt）。本服务只做「校验密码 → 发令牌」：

- 不新建账号、不改密码、不重置密码——这些还在原来的后台里做；
- 停用（`status=0`）、删除（`deleted=1`）的账号登不进来；
- 角色取自 `afv_admin_user_role`，目前三种：`super_admin`（超级管理员）、`billing_admin`（计费管理员）、`operator`（运营人员）。

> 本版**只认登录、不做细粒度权限**：登进来的账号能调所有后台接口。角色会在登录时返回，前端可以拿它显示/隐藏菜单。真要按角色限制接口，再说。

## 登录

| 接口 | 说明 |
|---|---|
| `POST /auth/login` | 用户名密码登录 |
| `GET /auth/me` | 当前登录的管理员（带令牌） |

```json
// POST /auth/login  请求
{"username": "keke", "password": "……"}

// 返回
{"code":0,"msg":"ok","data":{
  "token":"eyJhbGciOi…",          // 之后所有接口带 Authorization: Bearer <token>
  "expires_at":1790755825451,      // 过期时间（unix 毫秒）
  "admin":{"id":7,"username":"keke","real_name":"可可","roles":["operator"]}
}}
```

- 令牌有效期 **12 小时**（比 App 的 7 天短）。过期返回 `1002`「登录已过期，请重新登录」，重新登录即可，没有 refresh。
- 后台令牌和 App 令牌**密钥分开**，互相用不了：拿 App 的令牌调后台会被拒，反之亦然。

## 登录失败

| 情况 | 返回 |
|---|---|
| 用户名或密码不对 | `1002`「用户名或密码不正确」 |
| 账号不存在 | 同上——**故意不区分**，免得被拿来试探哪些账号存在 |
| 连错 5 次 | `1004`「连续登录失败次数过多，请 15 分钟后再试」。锁定期间**密码对也不放行** |
| 账号已停用 | `1004`「账号已停用，请联系管理员」 |
| 用户名或密码为空 | `1001` |

登录成功会清零失败计数、记录 `last_login_time` / `last_login_ip`（IP 取 `X-Forwarded-For` 最前面那个，由 nginx 带）。

## 联调模式

配置 `Auth.Mode=dev` 时，没带令牌仍然认请求头 `X-Admin-Id: <管理员ID>`，方便前端先跑通页面。

**上线必须改成 `Auth.Mode=token`**，否则任何人填个 ID 就能进后台。部署脚本的 `deploy.env` 里改 `AUTH_MODE=token` 再跑一次 `install.sh`。
