# 站内通知（Go 侧写入）· pkg/notify

状态：已实现。**Go 不提供任何通知接口**：用户端列表 / 未读数 / 已读（`/api/v1/notifications`）由 Java 通知中心提供（`afv_notification` + `afv_notification_read`，Java `V1.0.99`），web 现有铃铛直接展示。
Go 侧只有一件事：业务（目前是视频做同款）给某个用户发一条定向通知，经 `pkg/notify` 写进 `afv_notification`。

## 1 归属

- `afv_notification` 是 Java 共享表；Go 里**只有 `pkg/notify` 可写**（`tools/sharedtables` 规则：`^afv_notification$` → `pkg/notify`）。`afv_notification_read`（已读事实）Go 不写。
- 依赖 Java 迁移给 `afv_notification` 补四列与唯一键（Go 写入时带上）：`link varchar(512) NULL`、`biz_type varchar(32) NULL`、`biz_id varchar(64) NULL`、`dedupe_key varchar(128) NULL` + `UNIQUE uk_dedupe_key (dedupe_key)`。**Java 迁移上线前 Go 写入会失败**（只打日志，不影响业务）。

## 2 写入口径

```go
n := notify.New(conn) // conn 为业务库 sqlx 连接；nil 返回 nil
out, err := n.Send(ctx, notify.Message{
    UserID: 7, Title: "视频做同款已完成", Body: "3 段全部成功，点击查看",
    Link: "/video/replicate?job=42", BizType: "replicate_job", BizID: "42",
    DedupeKey: "replicate:42:replicate.done",
})
// out: notify.Sent（新写一条）/ notify.Duplicate（同去重键已发过）/ notify.Skipped（参数不合法，err=ErrInvalid）
```

| 列 | 值 |
|---|---|
| `target_scope` / `target_user_id` | `USER` / `Message.UserID` |
| `notification_type` | 恒 `SYSTEM`（任务类通知归系统消息；Java 只认 SYSTEM / ACTIVITY / MAINTENANCE） |
| `title` / `content` | `Title`（≤128 字截断，必填）/ `Body`（≤2000 字截断） |
| `publish_time` / `status` | 库时钟 `NOW()` / `1` 已发布（Java 用户端按 `status=1 AND publish_time<=NOW()` 列出） |
| `link` / `biz_type` / `biz_id` | 站内跳转（相对路径）/ 业务对象类型 / id |
| `dedupe_key` | 必填（≤128 字节，超长拒收）。`INSERT … ON DUPLICATE KEY UPDATE id = id`：命中唯一键视为已发（幂等，不覆盖）。并发同键只写一条 |

约定：正文只写「任务 + 状态 + 下一步」，不含提示词 / 素材内容；通知失败只打日志、不重试、不影响主流程。

## 3 当前发送方

视频做同款（`app/replicate/core/notify.go`）：`replicate.first_ready` / `replicate.done` / `replicate.seg_failed` 三类，去重键 `replicate:<job>:<类>`，详见 `docs/api/api-replicate.md` §8.5。

## 4 未做

- 用户按类型关闭（偏好）：本期不做。
- 微信服务号模板消息 / 短信：需要 go-user 能力，列 P1。
