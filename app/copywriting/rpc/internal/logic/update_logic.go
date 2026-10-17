package logic

import (
	"context"
	"errors"
	"strings"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *UpdateLogic) Update(in *pb.UpdateReq) (*pb.CopywritingInfo, error) {
	title, body := strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if body == "" {
		return nil, xerr.Param("请输入文案正文")
	}
	if runeLen(title) > 60 {
		return nil, xerr.Param("标题最多 60 字")
	}
	c, err := l.svcCtx.Copywriting.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("文案")
	}
	if err != nil {
		return nil, err
	}
	if c.Status == model.StatusGenerating {
		return nil, xerr.New(xerr.Conflict, "生成中的文案不能编辑")
	}
	if err := l.svcCtx.Copywriting.UpdateContent(l.ctx, c.Id, title, body, int64(runeLen(body))); err != nil {
		return nil, err
	}
	c.Title, c.Body, c.WordCount = title, body, int64(runeLen(body))
	return toInfo(c, contentTypeLabels(l.ctx, l.svcCtx)), nil
}
