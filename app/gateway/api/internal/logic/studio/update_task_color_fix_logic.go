// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateTaskColorFixLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 保存单张结果的校色参数（仅本人、仅成功的任务；校色在前端烘图，服务端只存五个滑杆值）；返回该任务（同 GET /tasks/:task_id）
func NewUpdateTaskColorFixLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTaskColorFixLogic {
	return &UpdateTaskColorFixLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateTaskColorFixLogic) UpdateTaskColorFix(req *types.StudioTaskColorFixReq) (resp *types.StudioTask, err error) {
	p := req.Params
	out, err := l.svcCtx.StudioRpc.UpdateTaskColorFix(l.ctx, &studio_client.UpdateTaskColorFixReq{
		TaskId: req.TaskId, UserId: ctxdata.UserID(l.ctx),
		Params: &studio_client.ColorFixParams{Hue: p.Hue, Saturation: p.Saturation, Value: p.Value, Brightness: p.Brightness, Contrast: p.Contrast},
	})
	if err != nil {
		return nil, err
	}
	t := toTask(out)
	return &t, nil
}
