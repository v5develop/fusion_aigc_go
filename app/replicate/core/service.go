package core

import (
	"context"
	"sync/atomic"
	"time"

	"aigc-local-life/pkg/lease"
)

// Service 业务内核入口（rpc 层的 logic 只做参数转换后调这里）。
type Service struct {
	cfg    Config
	store  Store
	ledger Ledger
	gw     Gateway
	media  Media
	cutter Cutter
	// composer 成片合成（拼接 / 占位 / 叠字），见 finish.go
	composer Composer
	policy   PolicyResolver
	signer   Signer   // 播放 / 下载地址现签（可选）
	urlOwner URLOwner // http(s) 输入的归属校验（可选；未装配时 http(s) 输入拒收）
	now      func() time.Time
	owner    string
	// renewFailures 预留续期失败累计（判据=RENEW_FAILED 告警日志带它）
	renewFailures atomic.Int64

	// 上线补齐（P0-3/4/5）：方案审核（moderation.go）、站内通知（notify.go）、入素材库（delivery.go），均可选
	mod      *moderator
	notifier Notifier
	sent     sentCache
	delivery *deliverer
	// lib 示例 / 改标注 / 我的方案（library.go，可选；不装配时这些接口回 7003）
	lib LibraryStore
	// workspace 用户的工作空间 id（确认时 WorkspaceID=0 回落它；可选，没装配 / 查不到记 0）
	workspace func(ctx context.Context, userID int64) (int64, error)
	// etaCal 方案卡 ETA 的历史校准（eta.go，可选；不装配则恒用先验 prior=true）
	etaCal *EtaCalibrator
	// previews 定妆图读写与每日额度（preview.go，可选；不装配时定妆图回 7003）
	previews PreviewStore
}

// Option 装配可选依赖。
type Option func(*Service)

// WithGateway 网关（分析的台词 / 画面理解、出片 video.generate）。
func WithGateway(g Gateway) Option { return func(s *Service) { s.gw = g } }

// WithMedia 媒体服务（分析的落桶 / 探测 / 抽帧 / 联系表）。
func WithMedia(m Media) Option { return func(s *Service) { s.media = m } }

// WithCutter 区间切段（媒体服务 /video/cut 上线后装配；不装配则长视频分段不开放）。
func WithCutter(c Cutter) Option { return func(s *Service) { s.cutter = c } }

// WithWorkspace 确认时的工作空间：请求没带（WorkspaceID=0）时回落用户的个人空间（pkg/workspace.PersonalID，同 studio / 画布）。
// 查不到 / 出错记 0（只告警，不挡确认）：素材入库时 pkg/asset 自己会按个人空间兜底建档。
func WithWorkspace(f func(ctx context.Context, userID int64) (int64, error)) Option {
	return func(s *Service) { s.workspace = f }
}

// WithEtaCalibrator 方案卡 ETA 的历史校准（进程内一份，跨运行参数重建共用）。不装配则 ETA 恒为先验（prior=true）。
func WithEtaCalibrator(c *EtaCalibrator) Option { return func(s *Service) { s.etaCal = c } }

// WithPolicy 水印 / 会员策略（默认 DefaultPolicy）。
func WithPolicy(p PolicyResolver) Option { return func(s *Service) { s.policy = p } }

// WithSigner 任务视图的播放 / 下载地址现签（媒体服务 ResourceURLs）。不装配则只下发 rid。
func WithSigner(sg Signer) Option { return func(s *Service) { s.signer = sg } }

// WithURLOwner http(s) 源视频 / 参考图的归属校验（本站存储签发且属于当前用户）。不装配时只接受 mr:// 引用。
func WithURLOwner(o URLOwner) Option { return func(s *Service) { s.urlOwner = o } }

// WithClock 测试用时钟。
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithOwner 测试用执行者标识（默认 lease.Owner()）。
func WithOwner(o string) Option { return func(s *Service) { s.owner = o } }

// New 装配。store 与 ledger 必填。
func New(cfg Config, store Store, ledger Ledger, opts ...Option) *Service {
	s := &Service{cfg: cfg.withDefaults(), store: store, ledger: ledger, now: time.Now, policy: DefaultPolicy{}}
	for _, o := range opts {
		o(s)
	}
	if s.owner == "" {
		s.owner = lease.Owner()
	}
	return s
}

// Limits 数值口径（GET /api/v1/replicate/limits，前端文案与后端校验同源）。
func (s *Service) Limits() LimitsView {
	l := s.cfg.Limits
	v := LimitsView{Limits: l, SegTargetSec: l.SegTarget(), LongformEnabled: s.cutter != nil,
		MaxInFlight: s.cfg.MaxInFlight, ContinueHours: int(s.cfg.ContinueWindow / time.Hour),
		RedoMaxPerSegment: s.cfg.RedoMaxPerSegment, DefaultModel: s.DefaultModel(), WriterEnabled: s.cfg.WriterModelGroup != "", ConfirmThresholdPoints: s.cfg.ConfirmThresholdPoints}
	for _, m := range s.enabledModels() {
		v.Models = append(v.Models, m.Group)
	}
	return v
}
