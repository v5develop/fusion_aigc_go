package logic

import (
	"context"
	"errors"
	"strings"
	"time"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreatePortraitLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePortraitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePortraitLogic {
	return &CreatePortraitLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AI 定制形象：异步出 4 张 9:16 图（执行面 75），轮询 GetPortrait
func (l *CreatePortraitLogic) CreatePortrait(in *pb.CreatePortraitReq) (*pb.PortraitInfo, error) {
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	// 幂等：同一用户同一 request_id 只生成一次
	if old, err := l.svcCtx.Portrait.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toPortrait(old), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	if err := requireAgreement(l.ctx, l.svcCtx, in.UserId); err != nil {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	labels, err := resolvePortraitLabels(l.ctx, l.svcCtx, in.Gender, in.Age, in.SkinTone, in.Role)
	if err != nil {
		return nil, err
	}
	desc := strings.TrimSpace(in.Description)
	if runeLen(desc) > l.svcCtx.Config.Limits.MaxDescriptionLen {
		return nil, xerr.Newf(xerr.ParamError, "创意描述最多 %d 字", l.svcCtx.Config.Limits.MaxDescriptionLen)
	}
	if err := checkText(l.ctx, l.svcCtx, "portrait_description", desc); err != nil {
		return nil, err
	}
	labels.Description = desc

	row := &model.AfvDigitalHumanPortrait{
		UserId: in.UserId, Gender: in.Gender, Age: in.Age, SkinTone: in.SkinTone, Role: nullStr(in.Role),
		Description: nullStr(desc), TaskId: nullStr(newTaskID(l.svcCtx)), RequestId: nullStr(in.RequestId),
		Status: model.PortraitGenerating,
	}
	res, err := l.svcCtx.Portrait.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) { // 并发重复提交
			old, ferr := l.svcCtx.Portrait.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toPortrait(old), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	row.CreateTime = time.Now()
	submitPortrait(l.ctx, l.svcCtx, row, buildPortraitPrompts(l.ctx, l.svcCtx, *labels, l.svcCtx.Config.Portrait.Count))
	if cur, err := l.svcCtx.Portrait.FindOne(l.ctx, row.Id); err == nil {
		row = cur
	}
	return toPortrait(row), nil
}
