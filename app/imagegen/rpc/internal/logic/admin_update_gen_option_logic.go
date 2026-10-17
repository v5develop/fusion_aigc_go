package logic

import (
	"context"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpdateGenOptionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminUpdateGenOptionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpdateGenOptionLogic {
	return &AdminUpdateGenOptionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminUpdateGenOption 改一个选项。编码不可改；校验：名称非空、每组至少保留一个启用项、默认项必须启用。
func (l *AdminUpdateGenOptionLogic) AdminUpdateGenOption(in *pb.UpdateGenOptionReq) (*pb.GenOptionItem, error) {
	label := strings.TrimSpace(in.Label)
	if label == "" || utf8.RuneCountInString(label) > 64 {
		return nil, xerr.Param("名称为 1–64 个字")
	}
	if in.SortOrder < 0 {
		return nil, xerr.Param("排序不能为负数")
	}
	if in.IsDefault && !in.Enabled {
		return nil, xerr.Param("默认项必须启用")
	}

	all, err := l.svcCtx.GenOpts.ListGenOptions(l.ctx)
	if err != nil {
		return nil, err
	}
	var target *model.GenOption
	for _, r := range all {
		if r.Id == in.Id {
			target = r
			break
		}
	}
	if target == nil {
		return nil, xerr.Param("选项不存在")
	}
	if !in.Enabled {
		others := 0
		for _, r := range all {
			if r.Group == target.Group && r.Id != target.Id && r.Enabled {
				others++
			}
		}
		if others == 0 {
			return nil, xerr.Param("每组至少保留一个启用项")
		}
	}

	// 海报风格名称会直接进入出图提示词，后台不允许改名；启用 / 排序 / 默认照常可改
	if target.Group == genPosterStyles && label != target.Label {
		return nil, xerr.Param("海报风格名称会进入出图提示词，不能在后台修改")
	}

	target.Label = label
	target.SortOrder = in.SortOrder
	target.Enabled = in.Enabled
	target.IsDefault = in.IsDefault
	if err := l.svcCtx.GenOpts.SaveGenOption(l.ctx, target); err != nil {
		return nil, err
	}
	invalidateGenOptions()
	return genItem(target), nil
}
