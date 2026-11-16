package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Switch 视频做同款总开关的后台读写（admin-api /api/v1/replicate/admin/switch）。
//
// 数据库 > 文件配置：afv_system_config.replicate.enabled 存在且合法时以库为准，否则用部署配置 Replicate.Enabled
// （admin-api 读不到网关的配置，由部署工具把同一个值写进 ReplicateAdmin.EnabledDefault）。
// 改动 60 秒内生效（网关运行参数缓存 runtimeTTL）。关着只拦新提交，历史 / 我的方案 / 在途任务照常。
// 审计：每次改动把「谁、何时、改成什么、原因」写进该行 remark，并打一条日志；恢复跟随部署配置 = 逻辑删除该行。
type Switch struct {
	conn          sqlx.SqlConn
	configDefault bool
	now           func() time.Time
}

// NewSwitch conn 为 nil 返回 nil（接口回 7003）。configDefault = 部署配置里的默认值。
func NewSwitch(conn sqlx.SqlConn, configDefault bool) *Switch {
	if conn == nil {
		return nil
	}
	return &Switch{conn: conn, configDefault: configDefault, now: time.Now}
}

// 开关来源
const (
	SwitchSourceDB     = "db"     // 数据库 afv_system_config.replicate.enabled
	SwitchSourceConfig = "config" // 部署配置 Replicate.Enabled（库里没有这一行 / 值非法）
)

// SwitchView 当前开关。
type SwitchView struct {
	// Enabled 当前是否接新提交（网关最迟 EffectSeconds 秒后与此一致）
	Enabled bool `json:"enabled"`
	// Source db = 以数据库为准；config = 跟随部署配置
	Source string `json:"source"`
	// ConfigDefault 部署配置里的默认值（Source=config 时生效）
	ConfigDefault bool `json:"configDefault"`
	// DBValue 库里的原值（库里有这一行才有；值非法时 Source 仍是 config）
	DBValue *string `json:"dbValue,omitempty"`
	// Remark 最近一次改动说明（谁、何时、原因）
	Remark string `json:"remark,omitempty"`
	// UpdatedAt 库里这一行最近更新时间
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	// EffectSeconds 改动多久内在网关生效
	EffectSeconds int `json:"effectSeconds"`
}

// SwitchReq 改开关。
type SwitchReq struct {
	AdminID int64
	Enabled bool
	Reason  string
}

const switchReasonMax = 200

type switchRow struct {
	Value      string    `db:"config_value"`
	Remark     string    `db:"remark"`
	UpdateTime time.Time `db:"update_time"`
}

// Get 读当前开关。
func (s *Switch) Get(ctx context.Context) (*SwitchView, error) {
	v := &SwitchView{Enabled: s.configDefault, Source: SwitchSourceConfig, ConfigDefault: s.configDefault, EffectSeconds: int(runtimeTTL / time.Second)}
	var row switchRow
	q := "SELECT COALESCE(config_value, '') AS config_value, COALESCE(remark, '') AS remark, update_time FROM afv_system_config " +
		"WHERE config_key = ? AND deleted = 0 LIMIT 1"
	err := s.conn.QueryRowCtx(ctx, &row, q, KeyEnabled)
	switch {
	case errors.Is(err, sqlx.ErrNotFound), errors.Is(err, sql.ErrNoRows):
		return v, nil
	case err != nil:
		return nil, err
	}
	raw := strings.TrimSpace(row.Value)
	v.DBValue, v.Remark = &raw, row.Remark
	t := row.UpdateTime
	v.UpdatedAt = &t
	if validBool(raw) {
		v.Source = SwitchSourceDB
		v.Enabled = parseEnabled(raw)
	}
	return v, nil
}

// Set 写库（数据库 > 文件配置，从此以库为准）。原因必填。
func (s *Switch) Set(ctx context.Context, req SwitchReq) (*SwitchView, error) {
	reason, err := switchReason(req.Reason)
	if err != nil {
		return nil, err
	}
	state := map[bool]string{true: "开启（接新提交）", false: "暂停新提交"}[req.Enabled]
	remark := s.remark(req.AdminID, "设为"+state, reason)
	q := "INSERT INTO afv_system_config (config_key, config_value, remark) VALUES (?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE config_value = VALUES(config_value), remark = VALUES(remark), deleted = 0"
	if _, err := s.conn.ExecCtx(ctx, q, KeyEnabled, strconv.FormatBool(req.Enabled), remark); err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infow("replicate: 后台改总开关", logx.Field("admin_id", req.AdminID), logx.Field("enabled", req.Enabled),
		logx.Field("source", SwitchSourceDB), logx.Field("reason", reason))
	return s.Get(ctx)
}

// Reset 恢复跟随部署配置：逻辑删除库里这一行（网关的运行参数只读 deleted=0）。原因必填。
func (s *Switch) Reset(ctx context.Context, adminID int64, rawReason string) (*SwitchView, error) {
	reason, err := switchReason(rawReason)
	if err != nil {
		return nil, err
	}
	state := map[bool]string{true: "开", false: "关"}[s.configDefault]
	remark := s.remark(adminID, "恢复跟随部署配置（当前部署配置为"+state+"）", reason)
	q := "UPDATE afv_system_config SET deleted = 1, remark = ? WHERE config_key = ? AND deleted = 0"
	if _, err := s.conn.ExecCtx(ctx, q, remark, KeyEnabled); err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infow("replicate: 后台改总开关", logx.Field("admin_id", adminID), logx.Field("enabled", s.configDefault),
		logx.Field("source", SwitchSourceConfig), logx.Field("reason", reason))
	return s.Get(ctx)
}

func switchReason(raw string) (string, error) {
	r := strings.TrimSpace(raw)
	if r == "" {
		return "", xerr.New(xerr.ParamMissing, "请填写原因")
	}
	if utf8.RuneCountInString(r) > switchReasonMax {
		return "", xerr.Param(fmt.Sprintf("原因最多 %d 字", switchReasonMax))
	}
	return r, nil
}

// remark 写进 afv_system_config.remark（varchar(256)，按字符截断）。
func (s *Switch) remark(adminID int64, what, reason string) string {
	out := fmt.Sprintf("视频做同款总开关：管理员 %d 于 %s %s。原因：%s", adminID, s.now().In(cstZone).Format("2006-01-02 15:04"), what, reason)
	if r := []rune(out); len(r) > 256 {
		out = string(r[:256])
	}
	return out
}

var cstZone = time.FixedZone("CST", 8*3600)

// parseEnabled 已确认 validBool 的值转布尔。
func parseEnabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "on", "yes":
		return true
	}
	return false
}
