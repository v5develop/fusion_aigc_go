package logic

import (
	"context"
	"errors"
	"strings"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminSaveOptionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminSaveOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminSaveOptionLogic {
	return &AdminSaveOptionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminSaveOptionLogic) AdminSaveOption(in *pb.OptionInfo) (*pb.OptionInfo, error) {
	switch in.OptionType {
	case model.OptionContentType, model.OptionStyle, model.OptionIndustry, model.OptionWordCount,
		model.OptionPrompt, model.OptionBlockWord:
	default:
		return nil, xerr.Param("选项类型只能是 CONTENT_TYPE / STYLE / INDUSTRY / WORD_COUNT / PROMPT / BLOCKWORD")
	}
	code, label := strings.TrimSpace(in.Code), strings.TrimSpace(in.Label)
	if code == "" || label == "" {
		return nil, xerr.Param("编码和名称必填")
	}
	if in.Status != 0 && in.Status != 1 {
		return nil, xerr.Param("状态只能是 0 或 1")
	}
	exist, err := l.svcCtx.Option.FindOneByOptionTypeCodeDeletedId(l.ctx, in.OptionType, code, 0)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	if exist != nil && exist.Id != in.Id {
		return nil, xerr.New(xerr.Conflict, "同类型下编码已存在")
	}

	row := &model.AfvCopywritingOption{
		Id: in.Id, OptionType: in.OptionType, Code: code, Label: label,
		Value: nullStr(in.Value), SortOrder: int64(in.SortOrder), Status: int64(in.Status),
	}
	if in.Id == 0 {
		res, err := l.svcCtx.Option.Insert(l.ctx, row)
		if err != nil {
			return nil, err
		}
		if row.Id, err = res.LastInsertId(); err != nil {
			return nil, err
		}
		return toOption(row), nil
	}
	old, err := l.svcCtx.Option.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) || (err == nil && old.Deleted != 0) {
		return nil, xerr.NotFoundf("选项")
	}
	if err != nil {
		return nil, err
	}
	old.OptionType, old.Code, old.Label = row.OptionType, row.Code, row.Label
	old.Value, old.SortOrder, old.Status = row.Value, row.SortOrder, row.Status
	if err := l.svcCtx.Option.Update(l.ctx, old); err != nil {
		return nil, err
	}
	return toOption(old), nil
}
