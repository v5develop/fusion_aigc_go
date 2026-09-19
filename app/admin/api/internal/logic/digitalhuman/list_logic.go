// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户数字人列表
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.DigitalHumanListReq) (resp *types.DigitalHumanListResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminList(l.ctx, &digitalhuman.AdminListReq{
		UserId: req.UserId, Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.DigitalHumanListResp{List: make([]types.DigitalHuman, 0, len(out.List)), Total: out.Total}
	for _, h := range out.List {
		resp.List = append(resp.List, types.DigitalHuman{
			Id: h.Id, UserId: h.UserId, Name: h.Name, PortraitSource: h.PortraitSource, ImageUrl: h.ImageUrl,
			VoiceId: h.VoiceId, VoiceName: h.VoiceName, Script: h.Script, VideoUrl: h.VideoUrl, Duration: h.Duration,
			Status: h.Status, FailReason: h.FailReason, CreateTime: h.CreateTime,
		})
	}
	return resp, nil
}
