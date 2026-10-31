// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"

	"github.com/zeromicro/go-zero/core/logx"
)

type SidecarStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 依赖状态：反推 sidecar / ai-gateway / 媒体服务
func NewSidecarStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SidecarStatusLogic {
	return &SidecarStatusLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *SidecarStatusLogic) SidecarStatus() (resp *types.StudioSidecarStatusResp, err error) {
	out, err := l.svcCtx.StudioRpc.AdminSidecarStatus(l.ctx, &studio_client.Empty{})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioSidecarStatusResp{Deps: make([]types.StudioDependency, 0, len(out.Deps)),
		RunningTasks: out.RunningTasks, QueuedBatches: out.QueuedBatches}
	for _, d := range out.Deps {
		resp.Deps = append(resp.Deps, types.StudioDependency{Name: d.Name, Ok: d.Ok, BaseUrl: d.BaseUrl,
			LatencyMs: d.LatencyMs, Version: d.Version, Detail: d.Detail})
	}
	return resp, nil
}
