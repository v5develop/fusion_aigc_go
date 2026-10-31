package logic

import (
	"context"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListHistoryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListHistoryLogic {
	return &ListHistoryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 本链路历史（游标分页），含完整请求快照供回填
func (l *ListHistoryLogic) ListHistory(in *studio.ListHistoryReq) (*studio.ListHistoryResp, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	if err := checkKind(in.Kind); err != nil {
		return nil, err
	}
	ms, id, err := ParseCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := int(NormalizeLimit(in.Limit))
	r := l.svcCtx.Repo
	// 多取一条判断有没有下一页
	list, err := r.Batch.ListHistory(l.ctx, in.UserId, model.Kind(in.Kind), repo.Cursor{Ms: ms, ID: id}, limit+1)
	if err != nil {
		return nil, err
	}
	resp := &studio.ListHistoryResp{}
	if len(list) > limit {
		list = list[:limit]
		last := list[len(list)-1]
		resp.NextCursor = FormatCursor(last.CreatedAt.UnixMilli(), last.ID)
	}
	ids := make([]int64, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	tasks, err := r.Task.ListByBatches(l.ctx, ids)
	if err != nil {
		return nil, err
	}
	resp.Items = make([]*studio.BatchDetail, 0, len(list))
	for _, b := range list {
		resp.Items = append(resp.Items, toBatchDetail(b, tasks[b.ID]))
	}
	return resp, nil
}
