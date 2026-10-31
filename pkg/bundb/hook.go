package bundb

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// maxSQLLen 日志里 SQL 的最大长度（按字符计），超出截断。
// 不做参数脱敏：bun 已把参数内联进 SQL，这里只截断，避免超长 INSERT 撑爆日志。
const maxSQLLen = 1000

// hook 把 bun 的 SQL、耗时、错误接到 logx。
type hook struct {
	slow   time.Duration
	logSQL bool
}

func newHook(slow time.Duration, logSQL bool) *hook {
	return &hook{slow: slow, logSQL: logSQL}
}

var _ bun.QueryHook = (*hook)(nil)

func (h *hook) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

// AfterQuery 优先级：错误 > 慢查询 > 逐条 debug。sql.ErrNoRows 不算错误。
func (h *hook) AfterQuery(ctx context.Context, ev *bun.QueryEvent) {
	dur := time.Since(ev.StartTime)
	query := truncate(ev.Query, maxSQLLen)
	l := logx.WithContext(ctx)

	switch {
	case ev.Err != nil && !errors.Is(ev.Err, sql.ErrNoRows):
		l.Errorw("bun query error",
			logx.Field("sql", query), logx.Field("duration", dur.String()), logx.Field("err", ev.Err.Error()))
	case dur >= h.slow:
		l.Sloww("bun slow query",
			logx.Field("sql", query), logx.Field("duration", dur.String()))
	case h.logSQL:
		l.Debugw("bun query",
			logx.Field("sql", query), logx.Field("duration", dur.String()))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "...(truncated)"
}
