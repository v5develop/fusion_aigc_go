package localtask

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type ForceFailTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营强制失败生成失败的任务
func NewForceFailTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForceFailTaskLogic {
	return &ForceFailTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ForceFailTask 按 origin_ref 分发到对应服务的强制失败接口：只收失败态记录，置为封禁重做，点数不动（失败时已退回）。
// 与重做同一套分发，Java 后台转发过来。
func (l *ForceFailTaskLogic) ForceFailTask(req *types.ForceFailTaskReq) (resp *types.EmptyResp, err error) {
	prefix, id, ok := parseOriginRef(req.OriginRef)
	if !ok {
		return nil, xerr.Param("任务坐标不合法")
	}
	operator := ctxdata.AdminID(l.ctx)
	switch prefix {
	case "img":
		_, err = l.svcCtx.ImageGenRpc.AdminForceFail(l.ctx, &imagegen.IdReq{Id: id})
	case "vid":
		_, err = l.svcCtx.ImageGenRpc.AdminForceFailVideo(l.ctx, &imagegen.IdReq{Id: id})
	case "note":
		_, err = l.svcCtx.NoteRpc.AdminForceFail(l.ctx, &note.IdReq{Id: id})
	case "dh":
		_, err = l.svcCtx.DigitalHumanRpc.AdminForceFail(l.ctx, &digitalhuman.IdReq{Id: id})
	default:
		return nil, xerr.Param("未知的任务来源")
	}
	if err != nil {
		l.Errorw("admin force-fail local life task failed", logx.Field("origin_ref", req.OriginRef),
			logx.Field("operator", operator), logx.Field("err", err.Error()))
		return nil, err
	}
	l.Infow("admin force-fail local life task", logx.Field("origin_ref", req.OriginRef), logx.Field("operator", operator))
	return &types.EmptyResp{}, nil
}
