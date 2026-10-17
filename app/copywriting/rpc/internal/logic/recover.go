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
// 这里启动时先扫一遍，之后定时扫，超过生成超时时间仍未结束的一律标记为失败。
// TODO 接入 afv_creation_task 后，可改为以任务表为准做对账。
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
		ok, err := r.svcCtx.Copywriting.FinishGenerate(ctx, c.Id, model.StatusFailed, "", "", 0, "生成超时，请重试")
		if err != nil {
			logx.WithContext(ctx).Errorf("recover stuck %d: %v", c.Id, err)
			continue
		}
		if ok {
			logx.WithContext(ctx).Infof("recovered stuck copywriting %d (created %s)", c.Id, c.CreateTime.Format(time.RFC3339))
		}
	}
}
