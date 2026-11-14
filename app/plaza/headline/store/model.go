// Package store 「本周头条」的数据访问（bun）：头条规则 / 抑制 / 控制行 / 快照（Go 独写），
// afv_agent_turn 与模板 / 行业目录（只读）。
//
// 时间列：库是 Asia/Shanghai 的无时区 DATETIME，而 bundb.New 的方言按 UTC 内联 time.Time，
// 所以本包写入时间一律经 CSTTime（Valuer 输出 CST 字符串），查询条件里的时间用 CSTString 格式化后传参。
package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"aigc-local-life/app/plaza/headline/algo"

	"github.com/uptrace/bun"
)

// 表名。
const (
	TableHeadline     = "afv_plaza_headline"
	TableBlock        = "afv_plaza_headline_block"
	TableCtl          = "afv_plaza_headline_ctl"
	TableSnapshot     = "afv_plaza_headline_snapshot"
	TableSnapshotItem = "afv_plaza_headline_snapshot_item"
	TableAgentTurn    = "afv_agent_turn" // Java 独写，只读
	TableAudit        = "afv_plaza_audit_log"
)

const cstLayout = "2006-01-02 15:04:05.000"

// CSTString 查询参数用的 CST 时间字符串。
func CSTString(t time.Time) string { return t.In(algo.CST).Format(cstLayout) }

// CSTTime 以 CST 字符串写入、从驱动的 time.Time / 字节读出的时间；零值写 NULL。
type CSTTime struct{ time.Time }

// Value 实现 driver.Valuer。
func (t CSTTime) Value() (driver.Value, error) {
	if t.IsZero() {
		return nil, nil
	}
	return CSTString(t.Time), nil
}

// Scan 实现 sql.Scanner。
func (t *CSTTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		t.Time = time.Time{}
	case time.Time:
		// 驱动按 DSN loc 解析；DSN 已是 Asia/Shanghai 时就是正确的时刻，否则按 CST 重新解释墙上时间
		if v.Location() == time.UTC {
			v = time.Date(v.Year(), v.Month(), v.Day(), v.Hour(), v.Minute(), v.Second(), v.Nanosecond(), algo.CST)
		}
		t.Time = v
	case []byte:
		return t.parse(string(v))
	case string:
		return t.parse(v)
	default:
		return fmt.Errorf("CSTTime: 不支持的类型 %T", src)
	}
	return nil
}

func (t *CSTTime) parse(s string) error {
	for _, layout := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02"} {
		if v, err := time.ParseInLocation(layout, s, algo.CST); err == nil {
			t.Time = v
			return nil
		}
	}
	return fmt.Errorf("CSTTime: 无法解析 %q", s)
}

// Ptr 零值返回 nil。
func (t CSTTime) Ptr() *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t.Time
	return &v
}

// HeadlineRule afv_plaza_headline：人工推荐规则（Java 创建、Go 接管）。
type HeadlineRule struct {
	bun.BaseModel `bun:"table:afv_plaza_headline"`

	ID              int64   `bun:"id,pk,autoincrement" json:"id"`
	PlazaTemplateID int64   `bun:"plaza_template_id" json:"plazaTemplateId"`
	ScopeCode       string  `bun:"scope_code" json:"scope"`
	BatchID         *string `bun:"batch_id" json:"batchId"`
	StartDate       CSTTime `bun:"start_date,type:date" json:"-"`
	EndDate         CSTTime `bun:"end_date,type:date" json:"-"`
	Why             string  `bun:"why" json:"why"`
	SortOrder       int     `bun:"sort_order" json:"sortOrder"`
	Enabled         int     `bun:"enabled" json:"enabled"`
	PinPosition     *int    `bun:"pin_position" json:"pinPosition"`
	AdoptedFromAuto int     `bun:"adopted_from_auto" json:"adoptedFromAuto"`
	CreatedBy       *int64  `bun:"created_by" json:"createdBy"`
	UpdatedBy       *int64  `bun:"updated_by" json:"updatedBy"`
	CreateTime      CSTTime `bun:"create_time,nullzero,scanonly" json:"-"`
	UpdateTime      CSTTime `bun:"update_time,nullzero,scanonly" json:"-"`
	Deleted         int     `bun:"deleted" json:"-"`
}

// Algo 转成算法输入。
func (r HeadlineRule) Algo() algo.ManualRule {
	pin := 0
	if r.PinPosition != nil {
		pin = *r.PinPosition
	}
	return algo.ManualRule{ID: r.ID, TemplateID: r.PlazaTemplateID, Scope: r.ScopeCode, StartDate: r.StartDate.Time, EndDate: r.EndDate.Time,
		Why: r.Why, SortOrder: r.SortOrder, Enabled: r.Enabled == 1 && r.Deleted == 0, PinPosition: pin, Adopted: r.AdoptedFromAuto == 1}
}

// Block afv_plaza_headline_block：取消 / 屏蔽。
type Block struct {
	bun.BaseModel `bun:"table:afv_plaza_headline_block"`

	ID              int64   `bun:"id,pk,autoincrement" json:"id"`
	PlazaTemplateID int64   `bun:"plaza_template_id" json:"plazaTemplateId"`
	ScopeCode       string  `bun:"scope_code" json:"scope"`
	Kind            string  `bun:"kind" json:"kind"`
	UntilAt         CSTTime `bun:"until_at" json:"-"`
	Reason          string  `bun:"reason" json:"reason"`
	BatchID         *string `bun:"batch_id" json:"batchId"`
	CreatedBy       *int64  `bun:"created_by" json:"createdBy"`
	CreateTime      CSTTime `bun:"create_time,nullzero,scanonly" json:"-"`
	UpdateTime      CSTTime `bun:"update_time,nullzero,scanonly" json:"-"`
}

// Algo 转成算法输入。
func (b Block) Algo() algo.Block {
	return algo.Block{ID: b.ID, TemplateID: b.PlazaTemplateID, Scope: b.ScopeCode, Kind: b.Kind, Until: b.UntilAt.Ptr()}
}

// Ctl afv_plaza_headline_ctl（单行 id=1）。
type Ctl struct {
	bun.BaseModel `bun:"table:afv_plaza_headline_ctl"`

	ID            int64           `bun:"id,pk"`
	ParamsJSON    json.RawMessage `bun:"params_json,type:json,nullzero"`
	ParamsVersion int             `bun:"params_version"`
	UpdatedBy     *int64          `bun:"updated_by"`
	IngestLastID  int64           `bun:"ingest_last_id"`
	CalibratedAt  CSTTime         `bun:"calibrated_at"`
}

// Snapshot afv_plaza_headline_snapshot：一次刷新一版。
type Snapshot struct {
	bun.BaseModel `bun:"table:afv_plaza_headline_snapshot"`

	ID             int64           `bun:"id,pk,autoincrement"`
	ComputedAt     CSTTime         `bun:"computed_at"`
	TriggerType    string          `bun:"trigger_type"`
	OperatorID     *int64          `bun:"operator_id"`
	ParamsHash     string          `bun:"params_hash"`
	ParamsJSON     json.RawMessage `bun:"params_json,type:json"`
	SignalSource   string          `bun:"signal_source"`
	WindowStart    CSTTime         `bun:"window_start"`
	WindowEnd      CSTTime         `bun:"window_end"`
	CandidateCount int             `bun:"candidate_count"`
	ScopeCount     int             `bun:"scope_count"`
	ItemCount      int             `bun:"item_count"`
	ScopeStats     json.RawMessage `bun:"scope_stats,type:json,nullzero"`
	DurationMs     *int            `bun:"duration_ms"`
}

// SnapshotItem afv_plaza_headline_snapshot_item：某版某范围的一条。
type SnapshotItem struct {
	bun.BaseModel `bun:"table:afv_plaza_headline_snapshot_item"`

	ID              int64           `bun:"id,pk,autoincrement"`
	Version         int64           `bun:"version"`
	ScopeCode       string          `bun:"scope_code"`
	PlazaTemplateID int64           `bun:"plaza_template_id"`
	RankNo          int             `bun:"rank_no"`
	Source          string          `bun:"source"`
	HeadlineID      *int64          `bun:"headline_id"`
	Score           *float64        `bun:"score"`
	ScoreDetail     json.RawMessage `bun:"score_detail,type:json,nullzero"`
	Why             string          `bun:"why"`
	Uses7d          int             `bun:"uses7d"`
	PrevUses        int             `bun:"prev_uses"`
	Trend           *int            `bun:"trend"`
	IsNew           int             `bun:"is_new"`
	UniqueUsers     *int            `bun:"unique_users"`
	EnteredAt       CSTTime         `bun:"entered_at"`
}

// ItemFromAlgo 算法条目 → 快照行。
func ItemFromAlgo(version int64, it algo.Item) SnapshotItem {
	row := SnapshotItem{Version: version, ScopeCode: it.Scope, PlazaTemplateID: it.TemplateID, RankNo: it.Rank, Source: it.Source,
		Score: it.Score, Why: truncRunes(it.Why, 60), Uses7d: it.Uses7d, PrevUses: it.PrevUses, Trend: it.Trend, UniqueUsers: it.UniqueUsers,
		EnteredAt: CSTTime{it.EnteredAt}}
	if it.HeadlineID != 0 {
		id := it.HeadlineID
		row.HeadlineID = &id
	}
	if it.IsNew {
		row.IsNew = 1
	}
	if it.Detail != nil {
		row.ScoreDetail, _ = json.Marshal(it.Detail)
	}
	return row
}

// Algo 快照行 → 算法条目（score_detail 原样解析）。
func (s SnapshotItem) Algo() algo.Item {
	it := algo.Item{Scope: s.ScopeCode, TemplateID: s.PlazaTemplateID, Rank: s.RankNo, Source: s.Source, Score: s.Score, Why: s.Why,
		Uses7d: s.Uses7d, PrevUses: s.PrevUses, Trend: s.Trend, IsNew: s.IsNew == 1, UniqueUsers: s.UniqueUsers, EnteredAt: s.EnteredAt.Time}
	if s.HeadlineID != nil {
		it.HeadlineID = *s.HeadlineID
	}
	if len(s.ScoreDetail) > 0 {
		var d algo.ScoreDetail
		if json.Unmarshal(s.ScoreDetail, &d) == nil {
			it.Detail = &d
		}
	}
	return it
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
