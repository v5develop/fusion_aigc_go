package logic

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type PlanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PlanLogic {
	return &PlanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开始构思（异步，轮询 Get；完成后进入规划页）
func (l *PlanLogic) Plan(in *pb.PlanReq) (*pb.NoteInfo, error) {
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	// 幂等：同一用户同一 request_id 只构思一次
	if old, err := l.svcCtx.Note.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toInfo(old), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	if noteTypeLabel(in.NoteType) == "" {
		return nil, xerr.Param("请选择笔记类型")
	}
	topic := strings.TrimSpace(in.Topic)
	switch {
	case topic == "":
		return nil, xerr.Param("请输入笔记主题或思路")
	case runeLen(topic) > lim.MaxTopicLen:
		return nil, xerr.Newf(xerr.ParamError, "主题最多 %d 字", lim.MaxTopicLen)
	}
	if err := checkText(l.ctx, l.svcCtx, "note_topic", topic); err != nil {
		return nil, err
	}
	count := int64(in.ImageCount)
	if count == 0 {
		count = int64(lim.DefaultImages)
	}
	if count < 1 {
		return nil, xerr.Param("套图数量至少 1 张")
	}
	materials, err := l.materials(in.MaterialUrls)
	if err != nil {
		return nil, err
	}
	var personURL string
	if in.DigitalHumanId > 0 {
		dh, err := l.svcCtx.DigitalHumanRpc.Get(l.ctx, &digitalhuman.IdUserReq{Id: in.DigitalHumanId, UserId: in.UserId})
		if err != nil {
			if code, _, ok := xerr.Parse(err); ok && code == xerr.NotFound {
				return nil, xerr.Param("所选数字人不存在")
			}
			return nil, err
		}
		if dh.Status != dhStatusDone {
			return nil, xerr.Param("所选数字人还未生成完成")
		}
		personURL = dh.ImageUrl
	}

	row := &model.AfvImageTextNote{
		UserId: in.UserId, NoteType: in.NoteType, Topic: topic, MaterialUrls: nullStr(jsonList(materials)),
		DigitalHumanId: sql.NullInt64{Int64: in.DigitalHumanId, Valid: in.DigitalHumanId > 0}, PersonUrl: nullStr(personURL),
		PlanTaskId: nullStr(newTaskID(l.svcCtx)), ImageCount: count, RequestId: nullStr(in.RequestId),
		Status: model.StatusPlanning,
	}
	res, err := l.svcCtx.Note.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) { // 并发重复提交
			old, ferr := l.svcCtx.Note.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toInfo(old), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	submitPlan(l.ctx, l.svcCtx, row)
	if cur, err := l.svcCtx.Note.FindOne(l.ctx, row.Id); err == nil {
		row = cur
	}
	return toInfo(row), nil
}

// dhStatusDone 数字人「已完成」（digitalhuman-rpc 的状态值）
const dhStatusDone = 2

// materials 校验素材：只接受本存储签发的上传地址，去重，最多 MaxMaterials 张。
func (l *PlanLogic) materials(urls []string) ([]string, error) {
	out := make([]string, 0, len(urls))
	seen := map[string]bool{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		if !l.svcCtx.Storage.Owns(u) {
			return nil, xerr.Param("素材请先通过上传接口上传")
		}
		seen[u] = true
		out = append(out, u)
	}
	if max := l.svcCtx.Config.Limits.MaxMaterials; len(out) > max {
		return nil, xerr.Newf(xerr.ParamError, "素材最多 %d 张", max)
	}
	return out, nil
}
