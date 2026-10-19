package logic

import (
	"context"
	"time"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// StuckRecovery 回收卡在「生成中」的文案。
//
// 生成跑在进程内的协程里，服务重启或崩溃会让记录永远停在生成中，前端一直轮询。
// 这里启动时先扫一遍，之后定时扫，超过生成超时时间仍未结束的记录按计费单对账：
//
//   - 没有预留单（billing 未开启 / 未冻结过）：标记失败，不涉及点数；
//   - 预留单仍是 HELD（冻结中）：标记失败并退回冻结；
//   - 预留单已是 REFUNDED / CANCELLED / EXPIRED：点数已经退回，只标记失败；
//   - 预留单是 CONFIRMED / PARTIALLY_REFUNDED：点数已经扣了但结果没落库，
//     这种情况不自动处理（既不能白给结果，也不能擅自退款），只记 ERROR 等人工核对。
//
// 对账依据是 afv_billing_reservation，而不是 afv_creation_task：任务表是从文案表同步过去的派生副本，
// 拿它对账等于拿自己对自己。
type StuckRecovery struct {
	svcCtx   *svc.ServiceContext
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewStuckRecovery(svcCtx *svc.ServiceContext) *StuckRecovery {
	return &StuckRecovery{svcCtx: svcCtx, interval: time.Minute, done: make(chan struct{})}
}

func (r *StuckRecovery) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	defer close(r.done)

	r.sweep(ctx) // 启动即回收上次遗留的
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	logx.Info("stuck recovery started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

func (r *StuckRecovery) Stop() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
	}
}

func (r *StuckRecovery) sweep(ctx context.Context) {
	// 留一倍超时时间的余量，避免误杀正在生成的
	timeout := time.Duration(r.svcCtx.Config.Generate.TimeoutSeconds) * time.Second
	before := time.Now().Add(-2 * timeout)

	list, err := r.svcCtx.Copywriting.ListStuck(ctx, before, 200)
	if err != nil {
		logx.WithContext(ctx).Errorf("list stuck: %v", err)
		return
	}
	for _, c := range list {
		r.recoverOne(ctx, c)
	}
}

// recoverOne 对账并回收一条卡住的记录。失败只记日志，下一轮会重试。
func (r *StuckRecovery) recoverOne(ctx context.Context, c *model.AfvCopywriting) {
	requestID := billingRequestID(c.Id)
	status, err := r.svcCtx.Billing.Status(ctx, c.UserId, requestID)
	if err != nil {
		logx.WithContext(ctx).Errorf("recover stuck %d: 查询预留单失败，下一轮重试: %v", c.Id, err)
		return
	}
	if status == "CONFIRMED" || status == "PARTIALLY_REFUNDED" {
		logx.WithContext(ctx).Errorf("recover stuck %d: 预留单 %s 已是 %s（点数已扣）但结果未落库，需人工核对，未自动处理", c.Id, requestID, status)
		return
	}
	// 先退款再标失败：退款后进程崩溃，下一轮记录仍卡住，但预留单已不是 HELD，只会补标失败、不会重复退款；
	// 反过来先标失败再退款，崩溃在中间就会永远漏退。只有 HELD 才真正退款，其余状态 Fail 是空操作。
	r.svcCtx.Billing.Fail(ctx, c.UserId, requestID, "生成超时，退回点数")
	ok, err := r.svcCtx.Copywriting.FinishGenerate(ctx, c.Id, model.StatusFailed, "", "", 0, "生成超时，请重试")
	if err != nil {
		logx.WithContext(ctx).Errorf("recover stuck %d: %v", c.Id, err)
		return
	}
	if !ok {
		return // 已被生成协程或别的回收结束
	}
	logx.WithContext(ctx).Infof("recovered stuck copywriting %d (created %s, billing=%q)", c.Id, c.CreateTime.Format(time.RFC3339), status)
}
