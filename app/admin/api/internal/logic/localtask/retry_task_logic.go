package localtask

import (
	"context"
	"strconv"
	"strings"

	"aigc-local-life/app/admin/api/internal/middleware"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营重做失败的任务
func NewRetryTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryTaskLogic {
	return &RetryTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RetryTask 按 origin_ref（<来源>:<主键>）分发到对应服务的重做接口。来源与 pkg/taskcenter 的 Prefix 一致：
// img 图片 / 长图，vid 视频，note 笔记，dh 数字人；studio 的 tyo 商拍 / pse 姿态 / simg 图片生成（主键是 afv_studio_task.id）。重新冻结与提交都在各自的 RPC 里做。
// 已经提交给厂商的在途任务不提供取消，所以这里只有重做。
func (l *RetryTaskLogic) RetryTask(req *types.RetryTaskReq) (resp *types.EmptyResp, err error) {
	prefix, id, ok := parseOriginRef(req.OriginRef)
	if !ok {
		return nil, xerr.Param("任务坐标不合法")
	}
	operator := ctxdata.AdminID(l.ctx)
	switch prefix {
	case "img":
		_, err = l.svcCtx.ImageGenRpc.AdminRetry(l.ctx, &imagegen.IdReq{Id: id})
	case "vid":
		_, err = l.svcCtx.ImageGenRpc.AdminRetryVideo(l.ctx, &imagegen.IdReq{Id: id})
	case "note":
		_, err = l.svcCtx.NoteRpc.AdminRetry(l.ctx, &note.IdReq{Id: id})
	case "dh":
		_, err = l.svcCtx.DigitalHumanRpc.AdminRetry(l.ctx, &digitalhuman.IdReq{Id: id})
	default:
		if !model.IsDefaultPrefix(prefix) {
			return nil, xerr.Param("未知的任务来源")
		}
		// studio：商拍换装 / 姿态复刻 / 图片生成（afv_studio_task 主键）。入口按 local_life 鉴权，studio 任务另要 studio:update
		if err := middleware.Require(l.ctx, studioUpdatePerm); err != nil {
			return nil, err
		}
		_, err = l.svcCtx.StudioRpc.AdminRetryTask(l.ctx, &studio_client.AdminTaskReq{Id: id, OperatorId: operator})
	}
	if err != nil {
		l.Errorw("admin retry local life task failed", logx.Field("origin_ref", req.OriginRef),
			logx.Field("operator", operator), logx.Field("err", err.Error()))
		return nil, err
	}
	l.Infow("admin retry local life task", logx.Field("origin_ref", req.OriginRef), logx.Field("operator", operator))
	return &types.EmptyResp{}, nil
}

// studioUpdatePerm studio 任务的重做 / 强制失败要的权限码（与 /api/v1/studio/admin 的 POST 同一个）。
// studio 的前缀（tyo / pse / simg）取 app/studio/model 的默认值常量，与 studio-rpc TaskIDPrefix 默认值共用。
const studioUpdatePerm = "studio:update"

// parseOriginRef 拆出 <来源>:<主键>。主键必须是正整数。
func parseOriginRef(ref string) (prefix string, id int64, ok bool) {
	prefix, idText, found := strings.Cut(strings.TrimSpace(ref), ":")
	if !found || prefix == "" {
		return "", 0, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return prefix, id, true
}
