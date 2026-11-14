package core

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"aigc-local-life/app/plaza/model"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// Repo 读取端：目录快照的数据源，以及对 Java 表（afv_agent_turn / afv_task_latency）的只读查询。
// 本文件对这两张 Java 表没有任何写语句（单测用 sqlmock 断言）。
type Repo struct {
	db bun.IDB
	// requireEnabled 用户端模板口径额外要求 enabled=1（= !Plaza.AdminWriteEnabled，见 tplRec.requireEnabled）。
	requireEnabled bool
}

// NewRepo db 为 *bun.DB 或 bun.Tx。
func NewRepo(db bun.IDB) *Repo { return &Repo{db: db} }

var _ Source = (*Repo)(nil)

func (r *Repo) Industries(ctx context.Context) ([]model.Industry, error) {
	var rows []model.Industry
	err := r.db.NewSelect().Model(&rows).Where("ti.deleted = 0").Order("ti.sort_order ASC", "ti.id ASC").Scan(ctx)
	return rows, err
}

func (r *Repo) Relations(ctx context.Context) ([]model.TemplateIndustry, error) {
	var rows []model.TemplateIndustry
	err := r.db.NewSelect().Model(&rows).Scan(ctx)
	return rows, err
}

func (r *Repo) PublishedTemplates(ctx context.Context) ([]model.Template, error) {
	var rows []model.Template
	q := r.db.NewSelect().Model(&rows).
		Where("t.deleted = 0").Where("t.state = ?", model.StatePublished)
	if r.requireEnabled {
		q = q.Where("t.enabled = 1")
	}
	err := q.Scan(ctx)
	return rows, err
}

func (r *Repo) Channels(ctx context.Context) ([]model.Channel, error) {
	var rows []model.Channel
	err := r.db.NewSelect().Model(&rows).Where("ch.deleted = 0").Order("ch.sort_order ASC", "ch.id ASC").Scan(ctx)
	return rows, err
}

// NonDeletedTemplates 全部未删除的模板（后台预览含草稿用）。
func (r *Repo) NonDeletedTemplates(ctx context.Context) ([]model.Template, error) {
	var rows []model.Template
	err := r.db.NewSelect().Model(&rows).Where("t.deleted = 0").Scan(ctx)
	return rows, err
}

// Uses7d 近 7 天每个模板被「做同款」的轮次数（受理即算，口径同 Java PlazaTemplateUsageStatsService）。
// 只读 afv_agent_turn，走 (plaza_template_id, create_time) 索引。
func (r *Repo) Uses7d(ctx context.Context) (map[int64]int64, error) {
	var rows []struct {
		ID   int64 `bun:"plaza_template_id"`
		Uses int64 `bun:"uses"`
	}
	err := r.db.NewRaw("SELECT plaza_template_id, COUNT(*) AS uses FROM afv_agent_turn "+
		"WHERE plaza_template_id IS NOT NULL AND create_time >= DATE_SUB(NOW(), INTERVAL 7 DAY) "+
		"GROUP BY plaza_template_id").Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(rows))
	for _, x := range rows {
		out[x.ID] = x.Uses
	}
	return out, nil
}

// TurnUse 一个用户对一个模板的使用汇总（「接着做」用）。
type TurnUse struct {
	TemplateID int64
	LastUsedAt time.Time
	UseCount   int
}

// RecentTurns 当前用户最近用过的模板（去重，最近优先）：取最近 200 轮再在内存里去重。
// 走 user_id 前缀；数据量大时可再加近 90 天限制（上线前对该 SQL 跑 EXPLAIN）。
func (r *Repo) RecentTurns(ctx context.Context, userID int64) ([]TurnUse, error) {
	var rows []struct {
		ID         int64     `bun:"plaza_template_id"`
		CreateTime time.Time `bun:"create_time"`
	}
	err := r.db.NewRaw("SELECT plaza_template_id, create_time FROM afv_agent_turn "+
		"WHERE user_id = ? AND plaza_template_id IS NOT NULL ORDER BY id DESC LIMIT 200", userID).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	idx := map[int64]int{}
	var out []TurnUse
	for _, x := range rows {
		if i, ok := idx[x.ID]; ok {
			out[i].UseCount++
			continue
		}
		idx[x.ID] = len(out)
		out = append(out, TurnUse{TemplateID: x.ID, LastUsedAt: x.CreateTime, UseCount: 1})
	}
	return out, nil
}

// LatencySamples 近 7 天已完成、非降级的任务总耗时（毫秒，升序），只读 afv_task_latency（Java V1.0.231）。
// entryCodes 为出图入口码；为空返回 nil（没有可对应的入口就不给预计耗时）。
func (r *Repo) LatencySamples(ctx context.Context, entryCodes []string) ([]int64, error) {
	if len(entryCodes) == 0 {
		return nil, nil
	}
	var ms []int64
	err := r.db.NewRaw("SELECT total_ms FROM afv_task_latency "+
		"WHERE status = 'completed' AND compute_error IS NULL AND gateway_degraded = 0 "+
		"AND submitted_at >= DATE_SUB(NOW(), INTERVAL 7 DAY) AND entry_code IN (?) "+
		"ORDER BY total_ms ASC LIMIT 5000", bun.In(entryCodes)).Scan(ctx, &ms)
	if err != nil {
		return nil, err
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	return ms, nil
}

// isDuplicate 是否唯一键冲突（MySQL 1062）。
func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func noRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
