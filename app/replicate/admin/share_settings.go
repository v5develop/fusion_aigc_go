package admin

import (
	"context"
	"fmt"

	"aigc-local-life/pkg/xerr"
)

// 分享奖励规则（CONTRACT §U）：是否发放、每条点数、每人每月上限。
// 存储与「数据库 > 配置文件」的读取在 httpapi.ShareRewardSettings（afv_system_config，每次请求现读，无缓存，改动立即生效）；
// 本包只定义取值、校验与来源接口，Service 在列表 / 详情 / 标记发放时各取一次。

const (
	// 默认值（配置文件没写、库里没配 / 非法时兜底；与 CONTRACT §U 一致）
	DefaultRewardEnabled      = true
	DefaultRewardPoints       = 30
	DefaultRewardMonthlyLimit = 3

	// 取值范围
	MinRewardPoints, MaxRewardPoints   = 1, 1000
	MinRewardMonthly, MaxRewardMonthly = 0, 100 // 0 = 不限
)

// RewardSettings 一份生效的奖励规则。
type RewardSettings struct {
	Enabled bool `json:"enabled"`
	// Points 每条奖励点数（1–1000）
	Points int `json:"points"`
	// MonthlyLimit 同一用户每自然月最多发放次数（0–100，0 = 不限）
	MonthlyLimit int `json:"monthlyLimit"`
}

// DefaultRewardSettings 内置默认。
func DefaultRewardSettings() RewardSettings {
	return RewardSettings{Enabled: DefaultRewardEnabled, Points: DefaultRewardPoints, MonthlyLimit: DefaultRewardMonthlyLimit}
}

// ValidPoints / ValidMonthlyLimit 取值范围校验。
func ValidPoints(n int) bool       { return n >= MinRewardPoints && n <= MaxRewardPoints }
func ValidMonthlyLimit(n int) bool { return n >= MinRewardMonthly && n <= MaxRewardMonthly }

// Validate 校验写入值（PUT 用）。
func (r RewardSettings) Validate() error {
	if !ValidPoints(r.Points) {
		return xerr.Param(fmt.Sprintf("每条奖励点数须在 %d–%d 之间", MinRewardPoints, MaxRewardPoints))
	}
	if !ValidMonthlyLimit(r.MonthlyLimit) {
		return xerr.Param(fmt.Sprintf("每月上限须在 %d–%d 之间（0 表示不限）", MinRewardMonthly, MaxRewardMonthly))
	}
	return nil
}

// RewardSettingsSource 取当前生效的奖励规则；实现必须永不失败（读库失败 / 值非法时自行回落到配置文件值）。
type RewardSettingsSource interface {
	Load(ctx context.Context) RewardSettings
}

// SetRewardSettings 装配奖励规则来源；不装配时用内置默认。
func (s *Service) SetRewardSettings(src RewardSettingsSource) { s.rewardSrc = src }

func (s *Service) rewardSettings(ctx context.Context) RewardSettings {
	if s.rewardSrc == nil {
		return DefaultRewardSettings()
	}
	return s.rewardSrc.Load(ctx)
}
