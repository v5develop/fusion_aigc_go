package logic

import (
	"context"
	"fmt"
	"strings"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type ImportActionVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewImportActionVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ImportActionVideoLogic {
	return &ImportActionVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ImportActionVideoLogic) ImportActionVideo(in *pb.ImportActionVideoReq) (*pb.ActionVideo, error) {
	// 视频由前端直传，这里只认地址归属；大小与类型前端控制
	if err := storage.Owned(l.svcCtx.Storage, in.Url); err != nil {
		return nil, xerr.Param("请先上传视频")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" { // 便签#8：导入后命名为「我的动作N」
		n, err := l.svcCtx.ActionVideo.CountByUser(l.ctx, in.UserId)
		if err != nil {
			return nil, err
		}
		name = fmt.Sprintf("我的动作%d", n+1)
	}
	if runeLen(name) > 32 {
		return nil, xerr.Param("名称最多 32 字")
	}
	row := &model.AfvActionVideo{UserId: in.UserId, Name: name, Url: in.Url, CoverUrl: nullStr(in.CoverUrl)}
	res, err := l.svcCtx.ActionVideo.Insert(l.ctx, row)
	if err != nil {
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	return &pb.ActionVideo{Id: row.Id, Name: row.Name, Url: row.Url, CoverUrl: row.CoverUrl.String, Mine: true}, nil
}
