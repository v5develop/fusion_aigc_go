// Package notify Go 写站内通知的唯一所属包：写 Java 的通知中心表 afv_notification（V1.0.99，定向通知 target_scope=USER +
// target_user_id）。用户端列表 / 未读数 / 已读（/api/v1/notifications）由 Java 提供，web 现有铃铛直接展示；Go 不注册任何通知路由。
//
// 约定：
//   - 共享表只经本包写（tools/sharedtables 的规则：afv_notification 属主 pkg/notify）。
//   - Send 按 dedupe_key 幂等：dedupe_key / link / biz_type / biz_id 四列与唯一键 uk_dedupe_key 由 Java 迁移补；
//     INSERT … ON DUPLICATE KEY UPDATE id = id，命中唯一键视为「已经发过」（Duplicate），不报错、不覆盖。
//     业务方拼确定的键（如「replicate:<jobId>:<kind>」），这就是「同一对象同一类只发 1 次」的频控。
//   - 一律写 notification_type=SYSTEM（任务类通知归系统消息，EXPERIENCE §3.7.2）、status=1 已发布、publish_time=库时钟 NOW()。
//   - 通知是「锦上添花」：调用方失败只打日志，绝不影响主流程（本包不重试、不阻塞）。
//   - 用户按类型关闭（偏好）本期不做。
package notify

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 列宽上限（afv_notification.title varchar(128)；link / biz_type / biz_id / dedupe_key 见 Java 补列迁移）。
// 超长按字符截断；dedupe_key 超长拒收（截断会把不同的键撞成一个）。content 是 text，按 2000 字截断防误用。
const (
	maxTitle   = 128
	maxContent = 2000
	maxLink    = 512
	maxBizType = 32
	maxBizID   = 64
	maxDedupe  = 128
)

// Message 一条要发的定向通知。
type Message struct {
	UserID    int64
	Kind      string // 业务内的类型（只用于日志 / 去重键拼装，不落库：Java 表只有 SYSTEM/ACTIVITY/MAINTENANCE 三类）
	Title     string // 必填
	Body      string // 正文（content）
	Link      string // 站内跳转（相对路径，如 /video/replicate?job=42）
	BizType   string // 业务对象类型，如 replicate_job
	BizID     string // 业务对象 id
	DedupeKey string // 必填：同一个键只发一次
}

// Outcome Send 的结果。
type Outcome int

const (
	Sent      Outcome = iota // 新写入一条
	Duplicate                // dedupe_key 已存在（之前发过），没有再写
	Skipped                  // 参数不合法，没有写
)

func (o Outcome) String() string {
	switch o {
	case Sent:
		return "sent"
	case Duplicate:
		return "duplicate"
	case Skipped:
		return "skipped"
	}
	return "unknown"
}

// ErrInvalid 参数不合法（缺必填 / dedupe_key 超长）。
var ErrInvalid = errors.New("notify: 参数不合法")

// Service 写 afv_notification。
type Service struct{ conn sqlx.SqlConn }

// New conn 为业务库（与 Java 同库）。conn 为 nil 返回 nil（调用方据此不发）。
func New(conn sqlx.SqlConn) *Service {
	if conn == nil {
		return nil
	}
	return &Service{conn: conn}
}

func trimRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// insertSQL 定向通知（Java 用户端按 target_scope=USER AND target_user_id=? AND status=1 AND publish_time<=NOW() 列出）。
const insertSQL = "INSERT INTO afv_notification (title, content, notification_type, target_scope, target_user_id, publish_time, status, " +
	"link, biz_type, biz_id, dedupe_key) VALUES (?, ?, 'SYSTEM', 'USER', ?, NOW(), 1, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE id = id"

// Send 发一条（按 dedupe_key 幂等）。
func (s *Service) Send(ctx context.Context, m Message) (Outcome, error) {
	m.DedupeKey = strings.TrimSpace(m.DedupeKey)
	if m.UserID <= 0 || strings.TrimSpace(m.Title) == "" || m.DedupeKey == "" || len(m.DedupeKey) > maxDedupe {
		return Skipped, ErrInvalid
	}
	if s == nil || s.conn == nil {
		return Skipped, errors.New("notify: 没有装配数据库")
	}
	res, err := s.conn.ExecCtx(ctx, insertSQL, trimRunes(m.Title, maxTitle), trimRunes(m.Body, maxContent), m.UserID,
		nullable(trimRunes(m.Link, maxLink)), nullable(trimRunes(m.BizType, maxBizType)), nullable(trimRunes(m.BizID, maxBizID)), m.DedupeKey)
	if err != nil {
		return Skipped, err
	}
	// MySQL：新插入 RowsAffected=1；命中唯一键且值不变（id = id）为 0
	if n, _ := res.RowsAffected(); n == 0 {
		return Duplicate, nil
	}
	return Sent, nil
}
