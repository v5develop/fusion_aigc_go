package logic

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLogic {
	return &CreateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 自主撰写：直接保存
// 自主撰写：直接保存
func (l *CreateLogic) Create(in *pb.CreateReq) (*pb.CopywritingInfo, error) {
	title, body := strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if body == "" {
		return nil, xerr.Param("请输入文案正文")
	}
	if runeLen(title) > 60 {
		return nil, xerr.Param("标题最多 60 字")
	}
	if title == "" {
		title = trimRunes(body, 20)
	}
	row := &model.AfvCopywriting{
		UserId: in.UserId, OwnerType: nullInt(in.OwnerType), OwnerId: nullInt(in.OwnerId),
		Source: model.SourceManual, Title: title, Body: body, WordCount: int64(runeLen(body)),
		Status: model.StatusDone,
	}
	res, err := l.svcCtx.Copywriting.Insert(l.ctx, row)
	if err != nil {
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	row.CreateTime = time.Now()
	return toInfo(row, nil), nil
}
