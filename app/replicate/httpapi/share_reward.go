package httpapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	radmin "aigc-local-life/app/replicate/admin"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ShareRewardSettings 分享奖励规则的后台读写（admin-api /api/v1/replicate/admin/share-reward-settings，CONTRACT §U）。
//
// 数据库 > 配置文件，逐项判定：afv_system_config 里该项存在且合法（点数 1–1000、月上限 0–100、开关为布尔）时以库为准，
// 非法 / 缺失的项告警后用部署配置 ReplicateAdmin.ShareReward（yaml 值本身非法再退内置默认 true/30/3）。
// 生效方式：每次请求现读库（read-through，不缓存；一次主键范围小查询），所以改动立即生效，比契约要求的 ≤60 秒更严；
// 读库失败时回落部署配置并打错误日志（不让奖励流程因此整体不可用）。
// 审计：PUT / DELETE 的原因写进三行的 remark 并打日志；恢复跟随配置文件 = 逻辑删除这三行。
type ShareRewardSettings struct {
	conn          sqlx.SqlConn
	configDefault radmin.RewardSettings
	now           func() time.Time
}

// 配置键（afv_system_config.config_key）
const (
	KeyShareRewardEnabled      = "replicate.share_reward.enabled"
	KeyShareRewardPoints       = "replicate.share_reward.points"
	KeyShareRewardMonthlyLimit = "replicate.share_reward.monthly_limit"
)

var shareRewardKeys = []string{KeyShareRewardEnabled, KeyShareRewardPoints, KeyShareRewardMonthlyLimit}

// NewShareRewardSettings conn 为 nil 返回 nil（接口回 7003）。cfg = 部署配置值，非法则整体退内置默认。
func NewShareRewardSettings(conn sqlx.SqlConn, cfg radmin.RewardSettings) *ShareRewardSettings {
	if conn == nil {
		return nil
	}
	if cfg.Validate() != nil {
		logx.Errorf("replicate: ReplicateAdmin.ShareReward 配置非法（points=%d monthlyLimit=%d），改用内置默认", cfg.Points, cfg.MonthlyLimit)
		cfg = radmin.DefaultRewardSettings()
	}
	return &ShareRewardSettings{conn: conn, configDefault: cfg, now: time.Now}
}

var _ radmin.RewardSettingsSource = (*ShareRewardSettings)(nil)

// ShareRewardView GET / PUT / DELETE 的返回。
type ShareRewardView struct {
	radmin.RewardSettings
	// Source db = 至少有一项以数据库为准；config = 全部跟随部署配置
	Source string `json:"source"`
	// ConfigDefault 部署配置值（恢复跟随后生效的值）
	ConfigDefault radmin.RewardSettings `json:"configDefault"`
	Remark        string                `json:"remark,omitempty"`
	UpdatedAt     *time.Time            `json:"updatedAt,omitempty"`
}

// ShareRewardReq 改规则。
type ShareRewardReq struct {
	AdminID int64
	radmin.RewardSettings
	Reason string
}

type shareRewardRow struct {
	Key        string    `db:"config_key"`
	Value      string    `db:"config_value"`
	Remark     string    `db:"remark"`
	UpdateTime time.Time `db:"update_time"`
}

const shareRewardSelect = "SELECT config_key, COALESCE(config_value, '') AS config_value, COALESCE(remark, '') AS remark, update_time " +
	"FROM afv_system_config WHERE config_key IN (?, ?, ?) AND deleted = 0"

func (s *ShareRewardSettings) query(ctx context.Context) ([]shareRewardRow, error) {
	var rows []shareRewardRow
	err := s.conn.QueryRowsCtx(ctx, &rows, shareRewardSelect, KeyShareRewardEnabled, KeyShareRewardPoints, KeyShareRewardMonthlyLimit)
	return rows, err
}

// resolve 逐项合并：库里合法的项覆盖部署配置。返回是否有项来自库，以及最近更新的 remark / 时间。
func (s *ShareRewardSettings) resolve(ctx context.Context, rows []shareRewardRow) (out radmin.RewardSettings, fromDB bool, remark string, at *time.Time) {
	out = s.configDefault
	for _, r := range rows {
		raw := strings.TrimSpace(r.Value)
		ok := false
		switch r.Key {
		case KeyShareRewardEnabled:
			if validBool(raw) {
				out.Enabled, ok = parseEnabled(raw), true
			}
		case KeyShareRewardPoints:
			if n, err := strconv.Atoi(raw); err == nil && radmin.ValidPoints(n) {
				out.Points, ok = n, true
			}
		case KeyShareRewardMonthlyLimit:
			if n, err := strconv.Atoi(raw); err == nil && radmin.ValidMonthlyLimit(n) {
				out.MonthlyLimit, ok = n, true
			}
		default:
			continue
		}
		if !ok {
			logx.WithContext(ctx).Errorf("replicate: 分享奖励配置值非法，改用部署配置 判据=share_reward_setting_invalid key=%s value=%q", r.Key, raw)
			continue
		}
		fromDB = true
		if at == nil || r.UpdateTime.After(*at) {
			t := r.UpdateTime
			at, remark = &t, r.Remark
		}
	}
	return out, fromDB, remark, at
}

// Load 实现 radmin.RewardSettingsSource：永不失败。
func (s *ShareRewardSettings) Load(ctx context.Context) radmin.RewardSettings {
	rows, err := s.query(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 读分享奖励配置失败，回落部署配置 判据=share_reward_setting_read_failed: %v", err)
		return s.configDefault
	}
	out, _, _, _ := s.resolve(ctx, rows)
	return out
}

// Get 当前生效规则与来源（读库失败返回错误，不静默回落，免得管理员看到假象）。
func (s *ShareRewardSettings) Get(ctx context.Context) (*ShareRewardView, error) {
	rows, err := s.query(ctx)
	if err != nil {
		return nil, err
	}
	out, fromDB, remark, at := s.resolve(ctx, rows)
	v := &ShareRewardView{RewardSettings: out, Source: SwitchSourceConfig, ConfigDefault: s.configDefault, Remark: remark, UpdatedAt: at}
	if fromDB {
		v.Source = SwitchSourceDB
	}
	return v, nil
}

// Set 三项一起写库（同一事务），从此以库为准。原因必填。
func (s *ShareRewardSettings) Set(ctx context.Context, req ShareRewardReq) (*ShareRewardView, error) {
	reason, err := switchReason(req.Reason)
	if err != nil {
		return nil, err
	}
	if err := req.RewardSettings.Validate(); err != nil {
		return nil, err
	}
	limit := "不限"
	if req.MonthlyLimit > 0 {
		limit = strconv.Itoa(req.MonthlyLimit) + " 次"
	}
	state := map[bool]string{true: "开启", false: "关闭"}[req.Enabled]
	remark := s.remark(req.AdminID, fmt.Sprintf("设为%s、每条 %d 点、每人每月 %s", state, req.Points, limit), reason)
	vals := []string{strconv.FormatBool(req.Enabled), strconv.Itoa(req.Points), strconv.Itoa(req.MonthlyLimit)}
	const q = "INSERT INTO afv_system_config (config_key, config_value, remark) VALUES (?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE config_value = VALUES(config_value), remark = VALUES(remark), deleted = 0"
	err = s.conn.TransactCtx(ctx, func(ctx context.Context, tx sqlx.Session) error {
		for i, k := range shareRewardKeys {
			if _, err := tx.ExecCtx(ctx, q, k, vals[i], remark); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infow("replicate: 后台改分享奖励规则", logx.Field("admin_id", req.AdminID), logx.Field("enabled", req.Enabled),
		logx.Field("points", req.Points), logx.Field("monthly_limit", req.MonthlyLimit), logx.Field("source", SwitchSourceDB), logx.Field("reason", reason))
	return s.Get(ctx)
}

// Reset 恢复跟随部署配置：逻辑删除这三行。原因必填。
func (s *ShareRewardSettings) Reset(ctx context.Context, adminID int64, rawReason string) (*ShareRewardView, error) {
	reason, err := switchReason(rawReason)
	if err != nil {
		return nil, err
	}
	remark := s.remark(adminID, "恢复跟随部署配置", reason)
	if _, err := s.conn.ExecCtx(ctx, "UPDATE afv_system_config SET deleted = 1, remark = ? WHERE config_key IN (?, ?, ?) AND deleted = 0",
		remark, KeyShareRewardEnabled, KeyShareRewardPoints, KeyShareRewardMonthlyLimit); err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infow("replicate: 后台改分享奖励规则", logx.Field("admin_id", adminID), logx.Field("source", SwitchSourceConfig), logx.Field("reason", reason))
	return s.Get(ctx)
}

// remark 写进 afv_system_config.remark（varchar(256)，按字符截断）。
func (s *ShareRewardSettings) remark(adminID int64, what, reason string) string {
	out := fmt.Sprintf("分享奖励规则：管理员 %d 于 %s %s。原因：%s", adminID, s.now().In(cstZone).Format("2006-01-02 15:04"), what, reason)
	if r := []rune(out); len(r) > 256 {
		out = string(r[:256])
	}
	return out
}
