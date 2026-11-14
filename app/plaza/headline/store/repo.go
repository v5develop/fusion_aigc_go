package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"aigc-local-life/app/plaza/headline/algo"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
)

// Repo 头条的全部数据访问。
type Repo struct {
	DB *bun.DB
	// RequireEnabled 模板可见性是否额外要求 enabled=1（= !Plaza.AdminWriteEnabled：Java 仍是模板写者时为 true）。
	RequireEnabled bool
}

// New 包装 bun 句柄。
func New(db *bun.DB) *Repo { return &Repo{DB: db} }

// IsMissingTable MySQL 1146（表不存在）：依赖的行业 / 审计表还没建（并行开发、先发头条）时降级。
func IsMissingTable(err error) bool { return mysqlCode(err) == 1146 }

// IsMissingColumn MySQL 1054（列不存在）：模板表还没加生命周期列时降级到旧口径。
func IsMissingColumn(err error) bool { return mysqlCode(err) == 1054 }

// IsDuplicate MySQL 1062（唯一键冲突）。
func IsDuplicate(err error) bool { return mysqlCode(err) == 1062 }

func mysqlCode(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// ---------------------------------------------------------------- 人工推荐规则

// ActiveRules 今天（CST）生效窗口内、启用、未删除的规则（范围与成员由算法再过滤）。
func (r *Repo) ActiveRules(ctx context.Context, now time.Time) ([]HeadlineRule, error) {
	today := now.In(algo.CST).Format("2006-01-02")
	var rows []HeadlineRule
	err := r.DB.NewSelect().Model(&rows).
		Where("enabled = 1").Where("deleted = 0").
		Where("start_date <= ?", today).Where("end_date >= ?", today).
		Order("id ASC").Scan(ctx)
	return rows, err
}

// RuleFilter 管理端规则列表的筛选。
type RuleFilter struct {
	Scope      string // 精确范围（含 *）；空=全部
	TemplateID int64
	Phase      string // current | upcoming | expired | 空
	Page, Size int
}

// ListRules 管理端规则列表（未删除）。
func (r *Repo) ListRules(ctx context.Context, f RuleFilter, now time.Time) ([]HeadlineRule, int, error) {
	today := now.In(algo.CST).Format("2006-01-02")
	var rows []HeadlineRule
	q := r.DB.NewSelect().Model(&rows).Where("deleted = 0")
	if f.Scope != "" {
		q = q.Where("scope_code = ?", f.Scope)
	}
	if f.TemplateID > 0 {
		q = q.Where("plaza_template_id = ?", f.TemplateID)
	}
	switch f.Phase {
	case "current":
		q = q.Where("start_date <= ?", today).Where("end_date >= ?", today)
	case "upcoming":
		q = q.Where("start_date > ?", today)
	case "expired":
		q = q.Where("end_date < ?", today)
	}
	if f.Size <= 0 || f.Size > 100 {
		f.Size = 20
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	n, err := q.Order("start_date DESC", "sort_order ASC", "id DESC").Limit(int64(f.Size)).Offset(int64((f.Page - 1) * f.Size)).ScanAndCount(ctx)
	return rows, int(n), err
}

// RulesOfTemplate 某模板全部未删除的规则。
func (r *Repo) RulesOfTemplate(ctx context.Context, tplID int64) ([]HeadlineRule, error) {
	var rows []HeadlineRule
	err := r.DB.NewSelect().Model(&rows).Where("deleted = 0").Where("plaza_template_id = ?", tplID).Order("id ASC").Scan(ctx)
	return rows, err
}

// GetRule 按 id 取未删除的规则；没有返回 nil。
func (r *Repo) GetRule(ctx context.Context, db bun.IDB, id int64) (*HeadlineRule, error) {
	var row HeadlineRule
	err := db.NewSelect().Model(&row).Where("id = ?", id).Where("deleted = 0").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// RulesOfBatch 同一批次未删除的规则。
func (r *Repo) RulesOfBatch(ctx context.Context, db bun.IDB, batchID string) ([]HeadlineRule, error) {
	var rows []HeadlineRule
	err := db.NewSelect().Model(&rows).Where("deleted = 0").Where("batch_id = ?", batchID).Order("id ASC").Scan(ctx)
	return rows, err
}

// InsertRule 新建一条规则（create_time / update_time 走库默认）。
func (r *Repo) InsertRule(ctx context.Context, db bun.IDB, row *HeadlineRule) error {
	res, err := db.NewInsert().Model(row).ExcludeColumn("id").Returning("").Exec(ctx) // create_time / update_time 是 scanonly，走库默认（再 Exclude 会报 can't find column）
	if err != nil {
		return err
	}
	// MySQL 没有 RETURNING：取本条语句的自增 id（同一连接，不用另发 SELECT LAST_INSERT_ID）
	row.ID, err = res.LastInsertId()
	return err
}

// UpdateRule 定向列更新（不整行写回，沿用 Java「禁止整行实体写回」的教训）。
func (r *Repo) UpdateRule(ctx context.Context, db bun.IDB, id int64, set map[string]any) error {
	if len(set) == 0 {
		return nil
	}
	q := db.NewUpdate().Table(TableHeadline)
	for _, k := range sortedKeys(set) {
		q = q.Set("? = ?", bun.Ident(k), set[k])
	}
	_, err := q.Where("id = ?", id).Where("deleted = 0").Exec(ctx)
	return err
}

// DeleteRule 逻辑删除（与 Java 的 deleted 字段同口径）。
func (r *Repo) DeleteRule(ctx context.Context, db bun.IDB, id int64, operator int64) error {
	_, err := db.NewUpdate().Table(TableHeadline).Set("deleted = 1").Set("updated_by = ?", nullID(operator)).
		Where("id = ?", id).Where("deleted = 0").Exec(ctx)
	return err
}

// ---------------------------------------------------------------- 抑制

// ActiveBlocks now 仍生效的抑制（永久或未到期）。
func (r *Repo) ActiveBlocks(ctx context.Context, now time.Time) ([]Block, error) {
	var rows []Block
	err := r.DB.NewSelect().Model(&rows).
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("until_at IS NULL").WhereOr("until_at > ?", CSTString(now))
		}).Order("id ASC").Scan(ctx)
	return rows, err
}

// ListBlocks 管理端抑制列表。
func (r *Repo) ListBlocks(ctx context.Context, scope string, tplID int64, activeOnly bool, now time.Time) ([]Block, error) {
	var rows []Block
	q := r.DB.NewSelect().Model(&rows)
	if scope != "" {
		q = q.Where("scope_code = ?", scope)
	}
	if tplID > 0 {
		q = q.Where("plaza_template_id = ?", tplID)
	}
	if activeOnly {
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.Where("until_at IS NULL").WhereOr("until_at > ?", CSTString(now))
		})
	}
	err := q.Order("id DESC").Limit(500).Scan(ctx)
	return rows, err
}

// GetBlock 按 id 取；没有返回 nil。
func (r *Repo) GetBlock(ctx context.Context, db bun.IDB, id int64) (*Block, error) {
	var row Block
	err := db.NewSelect().Model(&row).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// BlockOf 模板 × 范围的抑制行；没有返回 nil。
func (r *Repo) BlockOf(ctx context.Context, db bun.IDB, tplID int64, scope string) (*Block, error) {
	var row Block
	err := db.NewSelect().Model(&row).Where("plaza_template_id = ?", tplID).Where("scope_code = ?", scope).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// BlocksOfBatch 同一批次的抑制。
func (r *Repo) BlocksOfBatch(ctx context.Context, db bun.IDB, batchID string) ([]Block, error) {
	var rows []Block
	err := db.NewSelect().Model(&rows).Where("batch_id = ?", batchID).Order("id ASC").Scan(ctx)
	return rows, err
}

// UpsertBlock 每模板 × 范围一条，再次操作覆盖（ON DUPLICATE KEY UPDATE）。
func (r *Repo) UpsertBlock(ctx context.Context, db bun.IDB, row *Block) error {
	_, err := db.NewInsert().Model(row).ExcludeColumn("id"). // create_time / update_time 是 scanonly，走库默认

									On("DUPLICATE KEY UPDATE").
									Set("kind = VALUES(kind)").Set("until_at = VALUES(until_at)").Set("reason = VALUES(reason)").
									Set("batch_id = VALUES(batch_id)").Set("created_by = VALUES(created_by)").
									Returning("").Exec(ctx)
	return err
}

// DeleteBlock 解除 = 物理删除。
func (r *Repo) DeleteBlock(ctx context.Context, db bun.IDB, id int64) error {
	_, err := db.NewDelete().Table(TableBlock).Where("id = ?", id).Exec(ctx)
	return err
}

// ---------------------------------------------------------------- 控制行

// Ctl 读控制行（id=1）；行不存在时补建。
func (r *Repo) Ctl(ctx context.Context) (*Ctl, error) {
	var row Ctl
	err := r.DB.NewSelect().Model(&row).
		Column("id", "params_json", "params_version", "updated_by", "ingest_last_id", "calibrated_at").
		Where("id = 1").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := r.DB.NewRaw("INSERT IGNORE INTO " + TableCtl + " (id) VALUES (1)").Exec(ctx); err != nil {
			return nil, err
		}
		return &Ctl{ID: 1}, nil
	}
	return &row, err
}

// SaveParams 保存参数（CAS params_version，防止两个运营同时改互相覆盖）。ok=false 表示版本已变。
func (r *Repo) SaveParams(ctx context.Context, db bun.IDB, raw json.RawMessage, expectVersion int, operator int64) (bool, error) {
	res, err := db.NewUpdate().Table(TableCtl).
		Set("params_json = ?", string(raw)).Set("params_version = params_version + 1").Set("updated_by = ?", nullID(operator)).
		Where("id = 1").Where("params_version = ?", expectVersion).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AdvanceWatermark 推进摄入水位（CAS：只从 from 推到 to）。
func (r *Repo) AdvanceWatermark(ctx context.Context, from, to int64) (bool, error) {
	res, err := r.DB.NewUpdate().Table(TableCtl).Set("ingest_last_id = ?", to).
		Where("id = 1").Where("ingest_last_id = ?", from).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkCalibrated 记录校准完成时刻。
func (r *Repo) MarkCalibrated(ctx context.Context, at time.Time) error {
	_, err := r.DB.NewUpdate().Table(TableCtl).Set("calibrated_at = ?", CSTString(at)).Where("id = 1").Exec(ctx)
	return err
}

// ---------------------------------------------------------------- 快照

// LatestSnapshot 最新版本头；没有返回 nil。
func (r *Repo) LatestSnapshot(ctx context.Context) (*Snapshot, error) {
	var row Snapshot
	err := r.DB.NewSelect().Model(&row).Order("id DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// SnapshotByID 按版本号取；没有返回 nil。
func (r *Repo) SnapshotByID(ctx context.Context, version int64) (*Snapshot, error) {
	var row Snapshot
	err := r.DB.NewSelect().Model(&row).Where("id = ?", version).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// SnapshotAt 时刻 t 生效的版本（computed_at ≤ t 的最新一版）；没有返回 nil。
func (r *Repo) SnapshotAt(ctx context.Context, t time.Time) (*Snapshot, error) {
	var row Snapshot
	err := r.DB.NewSelect().Model(&row).Where("computed_at <= ?", CSTString(t)).Order("computed_at DESC", "id DESC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &row, err
}

// ListSnapshots 版本列表（不含大字段）。
func (r *Repo) ListSnapshots(ctx context.Context, page, size int) ([]Snapshot, int, error) {
	if size <= 0 || size > 100 {
		size = 20
	}
	if page <= 0 {
		page = 1
	}
	var rows []Snapshot
	n, err := r.DB.NewSelect().Model(&rows).
		Column("id", "computed_at", "trigger_type", "operator_id", "params_hash", "signal_source", "window_start", "window_end",
			"candidate_count", "scope_count", "item_count", "duration_ms").
		Order("id DESC").Limit(int64(size)).Offset(int64((page - 1) * size)).ScanAndCount(ctx)
	return rows, int(n), err
}

// Items 某版本的条目；scopes 为空取全部范围。按 (scope, rank) 排序。
func (r *Repo) Items(ctx context.Context, version int64, scopes ...string) ([]SnapshotItem, error) {
	var rows []SnapshotItem
	q := r.DB.NewSelect().Model(&rows).Where("version = ?", version)
	if len(scopes) > 0 {
		q = q.Where("scope_code IN (?)", bun.In(scopes))
	}
	err := q.Order("scope_code ASC", "rank_no ASC").Scan(ctx)
	return rows, err
}

// ItemsOfTemplate 某版本里某模板在各范围的条目。
func (r *Repo) ItemsOfTemplate(ctx context.Context, version, tplID int64) ([]SnapshotItem, error) {
	var rows []SnapshotItem
	err := r.DB.NewSelect().Model(&rows).Where("version = ?", version).Where("plaza_template_id = ?", tplID).Order("scope_code ASC").Scan(ctx)
	return rows, err
}

// ItemsOfTemplates 某版本里一批模板在各范围的条目（后台列表的头条摘要）。
func (r *Repo) ItemsOfTemplates(ctx context.Context, version int64, tplIDs []int64) ([]SnapshotItem, error) {
	var rows []SnapshotItem
	if len(tplIDs) == 0 {
		return rows, nil
	}
	err := r.DB.NewSelect().Model(&rows).Where("version = ?", version).Where("plaza_template_id IN (?)", bun.In(tplIDs)).
		Order("plaza_template_id ASC", "scope_code ASC").Scan(ctx)
	return rows, err
}

// InsertSnapshot 同一事务写快照头与全部条目（分批 INSERT），返回版本号。
func (r *Repo) InsertSnapshot(ctx context.Context, snap *Snapshot, items []algo.Item) (int64, error) {
	err := r.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewInsert().Model(snap).ExcludeColumn("id").Returning("").Exec(ctx)
		if err != nil {
			return err
		}
		if snap.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		const batch = 500
		for i := 0; i < len(items); i += batch {
			end := i + batch
			if end > len(items) {
				end = len(items)
			}
			rows := make([]SnapshotItem, 0, end-i)
			for _, it := range items[i:end] {
				rows = append(rows, ItemFromAlgo(snap.ID, it))
			}
			if _, err := tx.NewInsert().Model(&rows).ExcludeColumn("id").Returning("").Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	return snap.ID, err
}

// SnapshotMetas 保留清理用：全部版本的 (id, computed_at)。
func (r *Repo) SnapshotMetas(ctx context.Context) ([]algo.SnapshotMeta, error) {
	var rows []Snapshot
	if err := r.DB.NewSelect().Model(&rows).Column("id", "computed_at").Order("id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]algo.SnapshotMeta, len(rows))
	for i, s := range rows {
		out[i] = algo.SnapshotMeta{Version: s.ID, ComputedAt: s.ComputedAt.Time}
	}
	return out, nil
}

// DeleteSnapshots 整版删除（条目分批 1000，再删头）。
func (r *Repo) DeleteSnapshots(ctx context.Context, versions []int64) error {
	for _, v := range versions {
		for {
			res, err := r.DB.NewRaw("DELETE FROM "+TableSnapshotItem+" WHERE version = ? LIMIT 1000", v).Exec(ctx)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n < 1000 {
				break
			}
		}
		if _, err := r.DB.NewDelete().Table(TableSnapshot).Where("id = ?", v).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- afv_agent_turn（只读）

type turnRow struct {
	ID              int64   `bun:"id"`
	UserID          int64   `bun:"user_id"`
	PlazaTemplateID int64   `bun:"plaza_template_id"`
	CreateTime      CSTTime `bun:"create_time"`
}

func (t turnRow) algo() algo.Turn {
	return algo.Turn{RowID: t.ID, UserID: t.UserID, TemplateID: t.PlazaTemplateID, CreatedAt: t.CreateTime.Time}
}

// TurnsAfter 水位之后的做同款轮次（主键范围扫描，滞后 lag 避开未提交 / 慢提交的事务）。
func (r *Repo) TurnsAfter(ctx context.Context, watermark int64, lag time.Duration, limit int) ([]algo.Turn, error) {
	var rows []turnRow
	err := r.DB.NewRaw("SELECT id, user_id, plaza_template_id, create_time FROM "+TableAgentTurn+
		" WHERE id > ? AND plaza_template_id IS NOT NULL AND create_time < NOW(3) - INTERVAL ? MICROSECOND ORDER BY id LIMIT ?",
		watermark, lag.Microseconds(), limit).Scan(ctx, &rows)
	out := make([]algo.Turn, len(rows))
	for i, t := range rows {
		out[i] = t.algo()
	}
	return out, err
}

// MaxTurnID 当前最大轮次 id（水位初始化：历史交给校准，不从 0 全表扫）。
func (r *Repo) MaxTurnID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	err := r.DB.NewRaw("SELECT MAX(id) FROM "+TableAgentTurn).Scan(ctx, &id)
	return id.Int64, err
}

// TurnsBetween [from, to) 内的做同款轮次（走 Java 维护的 idx_afv_agent_turn_plaza_time）。
// 校准与 Redis 降级用：在 Go 里按同一套封顶 / 去重规则（algo.CountTurns）聚合，与 Lua 摄入同口径。
// maxID > 0 时只取 id ≤ maxID（校准只覆盖已摄入水位以内的行，水位之后的由摄入补，避免同一行计两次）。
func (r *Repo) TurnsBetween(ctx context.Context, from, to time.Time, maxID int64) ([]algo.Turn, error) {
	var rows []turnRow
	q := "SELECT id, user_id, plaza_template_id, create_time FROM " + TableAgentTurn +
		" WHERE plaza_template_id IS NOT NULL AND create_time >= ? AND create_time < ?"
	args := []any{CSTString(from), CSTString(to)}
	if maxID > 0 {
		q += " AND id <= ?"
		args = append(args, maxID)
	}
	err := r.DB.NewRaw(q, args...).Scan(ctx, &rows)
	out := make([]algo.Turn, len(rows))
	for i, t := range rows {
		out[i] = t.algo()
	}
	return out, err
}

// ---------------------------------------------------------------- 审计

// AuditEntry 一条审计（afv_plaza_audit_log，表由 Go_V1.0.33 建）。
type AuditEntry struct {
	OperatorID   int64
	Action       string // HEADLINE_CREATE / HEADLINE_UPDATE / HEADLINE_DELETE / HEADLINE_SUPPRESS / HEADLINE_UNSUPPRESS / HEADLINE_REFRESH / PARAMS_UPDATE …
	ResourceType string // HEADLINE_RULE | HEADLINE_BLOCK | HEADLINE_PARAMS
	ResourceID   string
	BatchID      string
	Before       any
	After        any
	RequestID    string
	ClientIP     string
}

// Audit 与业务写入同一事务写审计。审计表不存在（行业迁移尚未执行）时只告警不阻断：MySQL 的事务里
// 失败的语句不会让整个事务失效，所以这里吞掉 1146 是安全的；其余错误照常返回让事务回滚。
func (r *Repo) Audit(ctx context.Context, db bun.IDB, e AuditEntry) (missingTable bool, err error) {
	before, after := jsonOrNil(e.Before), jsonOrNil(e.After)
	diff := diffJSON(before, after)
	_, err = db.NewRaw("INSERT INTO "+TableAudit+
		" (operator_id, action, resource_type, resource_id, batch_id, before_json, after_json, diff_json, status, request_id, client_ip)"+
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'SUCCESS', ?, ?)",
		nullID(e.OperatorID), e.Action, e.ResourceType, nullStr(e.ResourceID), nullStr(e.BatchID),
		before, after, diff, nullStr(e.RequestID), nullStr(e.ClientIP)).Exec(ctx)
	if IsMissingTable(err) {
		return true, nil
	}
	return false, err
}

func jsonOrNil(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return string(b)
}

// diffJSON 顶层字段级 diff {field:{from,to}}；大字段（>512 字节）只记长度。
func diffJSON(before, after any) any {
	var b, a map[string]json.RawMessage
	if s, ok := before.(string); ok {
		_ = json.Unmarshal([]byte(s), &b)
	}
	if s, ok := after.(string); ok {
		_ = json.Unmarshal([]byte(s), &a)
	}
	if b == nil && a == nil {
		return nil
	}
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	out := map[string]map[string]any{}
	for k := range keys {
		if string(b[k]) == string(a[k]) {
			continue
		}
		out[k] = map[string]any{"from": shrink(b[k]), "to": shrink(a[k])}
	}
	if len(out) == 0 {
		return nil
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func shrink(v json.RawMessage) any {
	if v == nil {
		return nil
	}
	if len(v) > 512 {
		return map[string]int{"len": len(v)}
	}
	return v
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
