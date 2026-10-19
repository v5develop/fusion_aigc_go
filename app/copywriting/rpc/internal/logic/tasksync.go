package logic

import (
	"context"
	"time"

	"aigc-local-life/app/copywriting/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// TaskCenterSync 定时把文案同步进任务中心（afv_creation_task）。
//
// 文案的生成在请求协程里完成，没有统一的状态变更入口，所以不在每次状态变化时同步，
// 而是按固定间隔扫描 afv_copywriting 的变化（与其他服务的同步源口径一致）。
type TaskCenterSync struct {
	svcCtx   *svc.ServiceContext
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewTaskCenterSync(svcCtx *svc.ServiceContext) *TaskCenterSync {
	return &TaskCenterSync{svcCtx: svcCtx, interval: 10 * time.Second, done: make(chan struct{})}
}

func (s *TaskCenterSync) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer close(s.done)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	logx.Info("copywriting task center sync started")
	for {
		s.svcCtx.TaskCenter.Sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *TaskCenterSync) Stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}
