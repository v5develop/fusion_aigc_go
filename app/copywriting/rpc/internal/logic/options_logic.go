package logic

import (
	"context"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- App ----
func (l *OptionsLogic) Options(in *pb.Empty) (*pb.OptionsResp, error) {
	list, err := l.svcCtx.Option.ListByType(l.ctx, "", true)
	if err != nil {
		return nil, err
	}
	resp := &pb.OptionsResp{
		MaxTopicLen:     int32(l.svcCtx.Config.Generate.MaxTopicLen),
		MaxReferenceLen: int32(l.svcCtx.Config.Generate.MaxReferenceLen),
	}
	for _, o := range list {
		info := toOption(o)
		switch o.OptionType {
		case model.OptionContentType:
			resp.ContentTypes = append(resp.ContentTypes, info)
		case model.OptionStyle:
			resp.Styles = append(resp.Styles, info)
		case model.OptionIndustry:
			resp.Industries = append(resp.Industries, info)
		case model.OptionWordCount:
			resp.WordCounts = append(resp.WordCounts, info)
		}
	}
	// 模型只展示，不给用户选
	if w, err := l.svcCtx.Shared.ResolveWritingModel(l.ctx); err != nil {
		l.Errorf("resolve writing model: %v", err)
	} else {
		resp.Model = &pb.ModelInfo{Id: w.Id, Name: w.Name, Code: w.Code, Platform: w.Platform}
	}
	return resp, nil
}
