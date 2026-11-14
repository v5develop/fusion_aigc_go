// Package repo 画布各表（afv_canvas / afv_canvas_node_run / afv_canvas_template / afv_prompt_library_item / afv_canvas_copy_job）的 bun 数据访问。
//
// 约定（与 app/studio/rpc/internal/repo 一致）：
//   - 全部方法带 ctx；查单条找不到返回 (nil, nil)。
//   - 状态迁移一律 CAS（WHERE id=? AND status=?），ok=false 表示没抢到。
//   - 时间条件用 FROM_UNIXTIME(ms/1000) 交给库按会话时区换算。
//   - 只写 afv_canvas* / afv_prompt_library_item；计费 / 任务中心 / 素材库仍走 pkg/billing、pkg/taskcenter、pkg/asset。
package repo

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/pkg/bundb"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// 表名。
const (
	TableCanvas   = "afv_canvas"
	TableRun      = "afv_canvas_node_run"
	TableTemplate = "afv_canvas_template"
	TablePrompt   = "afv_prompt_library_item"
)

// Repo 数据访问入口。
type Repo struct {
	db       *bun.DB
	Canvas   *CanvasRepo
	Run      *RunRepo
	Template *TemplateRepo
	Prompt   *PromptRepo
	CopyJob  *CopyJobRepo
	// AgentCall 智能体工具调用审计 / 幂等（Go_V1.0.38）
	AgentCall *AgentCallRepo
	// Revision 画布快照历史（Go_V1.0.40，三方合并 base）
	Revision *RevisionRepo
	// McpToken 画布 MCP 访问令牌（Go_V1.0.42）
	McpToken *McpTokenRepo
}

// New 根 Repo。
func New(db *bun.DB) *Repo {
	r := bind(db)
	r.db = db
	return r
}

func bind(db bun.IDB) *Repo {
	return &Repo{Canvas: &CanvasRepo{db: db}, Run: &RunRepo{db: db}, Template: &TemplateRepo{db: db}, Prompt: &PromptRepo{db: db},
		CopyJob: &CopyJobRepo{db: db}, AgentCall: &AgentCallRepo{db: db}, Revision: &RevisionRepo{db: db},
		McpToken: &McpTokenRepo{db: db}}
}

// RunInTx 在事务里执行 fn。
func (r *Repo) RunInTx(ctx context.Context, fn func(ctx context.Context, tx *Repo) error) error {
	if r.db == nil {
		return errors.New("repo: 事务内不能再开事务")
	}
	return bundb.RunInTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		return fn(ctx, bind(tx))
	})
}

// IsDuplicate 是否唯一键冲突（MySQL 1062）。
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func noRows[T any](v *T, err error) (*T, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func affected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// msExpr 毫秒时间戳 → DATETIME(3) 表达式。
const msExpr = "FROM_UNIXTIME(? / 1000)"

// Trunc 按字符截断。
func Trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Page 页码 → offset / limit（page 从 1 起；size 默认 def、上限 max）。
func Page(page, size int64, def, max int64) (offset, limit int64) {
	if size <= 0 {
		size = def
	}
	if size > max {
		size = max
	}
	if page < 1 {
		page = 1
	}
	return (page - 1) * size, size
}
