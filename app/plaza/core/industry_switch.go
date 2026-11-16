package core

// 行业功能总开关：数据库 > 配置文件。
//
// afv_system_config.plaza.industry.enabled 存在且合法时以库为准，否则用 Plaza.Industry.Enabled（默认 false）。
// 关着时 /plaza/industries 返回 gateEnabled=false，新 Web 显示「行业模板整理中」。
// 网关与后台各自读库，60 秒内生效；读库失败沿用上一次的值，从没读成功过才用配置文件。
// 写入留痕：remark 记下谁、何时、目标状态与原因（原因必填，≤200 字）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// IndustrySwitchKey 行业功能总开关在 afv_system_config 里的 config_key。
const IndustrySwitchKey = "plaza.industry.enabled"

const industrySwitchTTL = 60 * time.Second

// IndustrySwitch 行业总开关的读写与缓存。
type IndustrySwitch struct {
	db       *sql.DB
	yaml     bool
	now      func() time.Time
	mu       sync.Mutex
	at       time.Time
	loaded   bool
	cur      bool
	warnedAt time.Time
}

func newIndustrySwitch(db *sql.DB, yaml bool) *IndustrySwitch {
	return &IndustrySwitch{db: db, yaml: yaml, now: time.Now}
}

// Enabled 当前是否开启（带 60 秒缓存）。
func (w *IndustrySwitch) Enabled(ctx context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if w.loaded && now.Sub(w.at) < industrySwitchTTL {
		return w.cur
	}
	w.at = now
	v, err := w.read(ctx)
	if err != nil {
		if now.Sub(w.warnedAt) > 10*time.Minute {
			w.warnedAt = now
			logx.WithContext(ctx).Errorf("plaza: 读行业总开关失败，沿用上一次: %v", err)
		}
		if !w.loaded {
			return w.yaml
		}
		return w.cur
	}
	w.loaded = true
	if v.fromDB {
		w.cur = v.enabled
	} else {
		w.cur = w.yaml
	}
	return w.cur
}

// IndustrySwitchView 当前开关的展示信息（后台用）。
type IndustrySwitchView struct {
	Enabled       bool       `json:"enabled"`
	Source        string     `json:"source"` // db / config
	ConfigDefault bool       `json:"configDefault"`
	DBValue       *string    `json:"dbValue,omitempty"`
	Remark        string     `json:"remark,omitempty"`
	UpdatedAt     *time.Time `json:"updatedAt,omitempty"`
	EffectSeconds int        `json:"effectSeconds"`
}

type switchRead struct {
	enabled bool
	fromDB  bool
	raw     string
	remark  string
	updated time.Time
	hasRow  bool
}

func (w *IndustrySwitch) read(ctx context.Context) (switchRead, error) {
	var out switchRead
	var raw, remark sql.NullString
	var updated sql.NullTime
	err := w.db.QueryRowContext(ctx,
		"SELECT config_value, remark, update_time FROM afv_system_config WHERE config_key = ? AND deleted = 0 LIMIT 1",
		IndustrySwitchKey).Scan(&raw, &remark, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.hasRow = true
	out.raw = strings.TrimSpace(raw.String)
	out.remark = remark.String
	out.updated = updated.Time
	if b, ok := parseSwitchBool(out.raw); ok {
		out.fromDB, out.enabled = true, b
	}
	return out, nil
}

// View 当前开关的完整信息。
func (w *IndustrySwitch) View(ctx context.Context) (*IndustrySwitchView, error) {
	r, err := w.read(ctx)
	if err != nil {
		return nil, err
	}
	v := &IndustrySwitchView{Enabled: w.yaml, Source: "config", ConfigDefault: w.yaml, EffectSeconds: int(industrySwitchTTL / time.Second)}
	if r.fromDB {
		v.Enabled, v.Source = r.enabled, "db"
	}
	if r.hasRow {
		raw := r.raw
		v.DBValue, v.Remark = &raw, r.remark
		if !r.updated.IsZero() {
			t := r.updated
			v.UpdatedAt = &t
		}
	}
	return v, nil
}

// SetIndustrySwitch 写库（之后以库为准）。原因必填。
func (s *Service) SetIndustrySwitch(ctx context.Context, op Operator, enabled bool, reason string) (*IndustrySwitchView, error) {
	if s.industry == nil {
		return nil, ErrNotReady()
	}
	reason, err := industrySwitchReason(reason)
	if err != nil {
		return nil, err
	}
	state := map[bool]string{true: "开启", false: "关闭"}[enabled]
	remark := industryRemark(op.ID, s.industry.now(), "设为"+state, reason)
	if _, err := s.industry.db.ExecContext(ctx,
		"INSERT INTO afv_system_config (config_key, config_value, remark) VALUES (?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE config_value = VALUES(config_value), remark = VALUES(remark), deleted = 0",
		IndustrySwitchKey, strconv.FormatBool(enabled), remark); err != nil {
		return nil, err
	}
	s.industry.invalidate()
	logx.WithContext(ctx).Infow("plaza: 后台改行业总开关", logx.Field("admin_id", op.ID), logx.Field("enabled", enabled), logx.Field("reason", reason))
	return s.industry.View(ctx)
}

// ResetIndustrySwitch 恢复跟随配置文件：逻辑删除库里的行。原因必填。
func (s *Service) ResetIndustrySwitch(ctx context.Context, op Operator, reason string) (*IndustrySwitchView, error) {
	if s.industry == nil {
		return nil, ErrNotReady()
	}
	reason, err := industrySwitchReason(reason)
	if err != nil {
		return nil, err
	}
	state := map[bool]string{true: "开", false: "关"}[s.industry.yaml]
	remark := industryRemark(op.ID, s.industry.now(), "恢复跟随配置文件（当前配置文件为"+state+"）", reason)
	if _, err := s.industry.db.ExecContext(ctx,
		"UPDATE afv_system_config SET deleted = 1, remark = ? WHERE config_key = ? AND deleted = 0", remark, IndustrySwitchKey); err != nil {
		return nil, err
	}
	s.industry.invalidate()
	logx.WithContext(ctx).Infow("plaza: 后台恢复行业总开关跟随配置", logx.Field("admin_id", op.ID), logx.Field("reason", reason))
	return s.industry.View(ctx)
}

// IndustrySwitchView 后台读取：完整信息（库值、来源、配置文件默认值、备注）。
func (s *Service) IndustrySwitchView(ctx context.Context) (*IndustrySwitchView, error) {
	if s.industry == nil {
		return &IndustrySwitchView{Enabled: s.cfg.Industry.Enabled, Source: "config", ConfigDefault: s.cfg.Industry.Enabled, EffectSeconds: int(industrySwitchTTL / time.Second)}, nil
	}
	return s.industry.View(ctx)
}

// IndustryEnabled 供 Service 使用：当前行业功能是否开放。
func (s *Service) IndustryEnabled(ctx context.Context) bool {
	if s.industry == nil {
		return s.cfg.Industry.Enabled
	}
	return s.industry.Enabled(ctx)
}

func (w *IndustrySwitch) invalidate() {
	w.mu.Lock()
	w.loaded = false
	w.mu.Unlock()
}

func parseSwitchBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "on", "yes":
		return true, true
	case "false", "0", "off", "no":
		return false, true
	}
	return false, false
}

func industrySwitchReason(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return "", xerr.New(xerr.ParamMissing, "请填写原因")
	}
	if utf8.RuneCountInString(r) > 200 {
		return "", xerr.Param("原因最多 200 字")
	}
	return r, nil
}

func industryRemark(adminID int64, now time.Time, what, reason string) string {
	out := fmt.Sprintf("行业功能总开关：管理员 %d 于 %s %s。原因：%s", adminID, now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04"), what, reason)
	if r := []rune(out); len(r) > 256 {
		out = string(r[:256])
	}
	return out
}
