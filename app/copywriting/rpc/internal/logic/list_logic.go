package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/pagex"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListLogic) List(in *pb.ListReq) (*pb.CopywritingList, error) {
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	list, total, err := l.svcCtx.Copywriting.ListByUser(l.ctx, model.ListFilter{
		UserID: in.UserId, Keyword: strings.TrimSpace(in.Keyword), ContentType: in.ContentType,
		Status: int64(in.Status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	labels := contentTypeLabels(l.ctx, l.svcCtx)
	resp := &pb.CopywritingList{Total: total, List: make([]*pb.CopywritingInfo, 0, len(list))}
	for _, c := range list {
		resp.List = append(resp.List, toInfo(c, labels))
	}
	return resp, nil
}
