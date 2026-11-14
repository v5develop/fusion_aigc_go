package httpapi

import (
	"context"
	"time"

	radmin "aigc-local-life/app/replicate/admin"
	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/app/replicate/rules/analysisfix"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/notify"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// AdminForcer admin-api 强制收尾用的内核（app/replicate/admin.Forcer）：
//   - 运行参数与 gateway 同源（afv_system_config 的 replicate.*，60 秒缓存，读法同 Runtime）；一次都没读到时拒绝收尾
//     （返回「运行参数暂不可用」），不拿内核默认值去动钱；之后读失败沿用上一次成功的值；
//   - 装通知器（收尾后按新状态补发站内通知，取消的段计入「没做成」）；
//   - taskCenter=true（ReplicateAdmin.TaskCenterEnabled，默认 true，须与 gateway 的 Replicate.TaskCenterEnabled 一致）时，
//     收尾后异步触发一轮任务中心同步（gateway 轮询器另有周期同步兜底）。
//
// 不装媒体服务：收尾不合成成片、不入素材库（由 gateway 轮询器的 finalize / 发件箱补做）。
type AdminForcer struct {
	rt   *Runtime
	sync *taskcenter.AsyncSync
}

// NewAdminForcer conn 为业务库；bc 必须与 gateway-api 的 Replicate.Billing 一致。
func NewAdminForcer(conn sqlx.SqlConn, bc billing.Conf, taskCenter bool) (*AdminForcer, *repo.Repo, *billing.Service, error) {
	db, err := bundb.New(conn)
	if err != nil {
		return nil, nil, nil, err
	}
	store := repo.New(db)
	bill := billing.New(bc, conn)
	ledger := core.NewBillingLedger(bill)
	notifier := notify.New(conn)
	build := func(cfg core.Config, _ bool) *core.Service {
		return core.New(cfg, store, ledger, core.WithNotifier(notifier))
	}
	f := &AdminForcer{rt: newRuntime(core.DefaultConfig(), false, systemConfigLoader(conn), build)}
	if taskCenter {
		sy := taskcenter.New(conn, 10*time.Minute, TaskSource())
		f.sync = taskcenter.NewAsyncSync("replicate-admin", sy.Sync)
	}
	return f, store, bill, nil
}

// ResolveAnalysis 读取时按当前运行参数（afv_system_config.replicate.analysis_rules）的纠偏规则读出分析结果；
// 运行参数暂不可用时按默认规则。后台示例详情用（带 corrections / rulesVersion）。
func (f *AdminForcer) ResolveAnalysis(ctx context.Context, a *model.Analysis) (*core.AnalysisResult, error) {
	if svc, _ := f.rt.Service(ctx); svc != nil && f.rt.Loaded() {
		return svc.ResolveAnalysis(a)
	}
	return core.ResolveAnalysisWith(a, analysisfix.Config{})
}

// ForceFinish 见 core.Service.ForceFinish。
func (f *AdminForcer) ForceFinish(ctx context.Context, jobID int64) (*core.ForceFinishResult, error) {
	svc, _ := f.rt.Service(ctx) // 运行时总开关（replicate.enabled）只拦用户端新请求，后台收尾照常
	if svc == nil || !f.rt.Loaded() {
		return nil, xerr.New(xerr.NotReady, "运行参数暂不可用，请稍后再试")
	}
	res, err := svc.ForceFinish(ctx, jobID)
	if err == nil && f.sync != nil {
		f.sync.Kick()
	}
	return res, err
}

// 后台示例详情经 forcer 拿当前规则配置做读取时纠偏（admin.Service.resolveAnalysis 按接口断言取用）。
var _ radmin.AnalysisResolver = (*AdminForcer)(nil)
