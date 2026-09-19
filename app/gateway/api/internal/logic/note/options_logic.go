// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/note/rpc/note"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 图文笔记表单配置：笔记类型、素材与套图上限
func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OptionsLogic) Options() (resp *types.NoteOptionsResp, err error) {
	out, err := l.svcCtx.NoteRpc.Options(l.ctx, &note.Empty{})
	if err != nil {
		return nil, err
	}
	resp = &types.NoteOptionsResp{
		Types: make([]types.NoteType, 0, len(out.Types)), MaxMaterials: out.MaxMaterials,
		DefaultImages: out.DefaultImages, MaxEditImages: out.MaxEditImages, MaxTopicLen: out.MaxTopicLen,
	}
	for _, t := range out.Types {
		resp.Types = append(resp.Types, types.NoteType{Code: t.Code, Label: t.Label, MaterialLabel: t.MaterialLabel, MaterialHint: t.MaterialHint})
	}
	return resp, nil
}
