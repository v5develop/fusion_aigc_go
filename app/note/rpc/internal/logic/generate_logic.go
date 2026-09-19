package logic

import (
	"context"
	"strings"

	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateLogic {
	return &GenerateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 确认开始生成；生成失败后再次调用即重新生成
func (l *GenerateLogic) Generate(in *pb.GenerateReq) (*pb.NoteInfo, error) {
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if n.Status == model.StatusGenerating || n.Status == model.StatusDone {
		return toInfo(n), nil // 重复点击：已在生成或已完成，直接返回当前状态
	}
	if n.Status != model.StatusPlanned && n.Status != model.StatusGenFailed {
		return nil, xerr.New(xerr.Conflict, "笔记还未构思完成")
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	s, err := parseScript([]byte(n.PlanScript.String))
	if err != nil {
		l.Errorf("note %d parse script: %v", n.Id, err)
		return nil, xerr.New(xerr.Conflict, "构思结果异常，请重新构思")
	}

	// 规划页改过的文字；不传沿用构思结果（生成失败后重试时）
	title, content, tags := n.Title, n.Content.String, parseList(n.Tags)
	if in.Title != "" || in.Content != "" || in.Tags != nil {
		title, content = strings.TrimSpace(in.Title), strings.TrimSpace(in.Content)
		tags = normalizeTags(in.Tags, l.svcCtx.Config.Limits.MaxTags)
		if err := checkNoteText(l.ctx, l.svcCtx, "note_plan_edit", title, content, tags); err != nil {
			return nil, err
		}
	}
	count := int(n.ImageCount)
	if in.ImageCount != 0 {
		count = int(in.ImageCount)
	}
	// 套图数量不设上限，但出图提示词是构思时按张数生成的：改少直接取前几张，改多需要重新构思
	switch {
	case count < 1:
		return nil, xerr.Param("套图数量至少 1 张")
	case count > len(s.Images):
		return nil, xerr.Newf(xerr.ParamError, "当前方案只有 %d 张图，增加数量请点「重新生成」重新构思", len(s.Images))
	}
	task := imagesTask(l.svcCtx, s, count)
	if err := task.Validate(); err != nil {
		l.Errorf("note %d images task: %v", n.Id, err)
		return nil, xerr.New(xerr.Conflict, "构思结果异常，请重新构思")
	}

	taskID := newTaskID(l.svcCtx)
	ok, err := l.svcCtx.Note.StartGenerate(l.ctx, n.Id, title, content, jsonList(tags), int64(count), taskID)
	if err != nil {
		return nil, err
	}
	if ok {
		submitGenerate(l.ctx, l.svcCtx, n.Id, taskID, task)
	}
	if n, err = l.svcCtx.Note.FindOne(l.ctx, n.Id); err != nil {
		return nil, err
	}
	return toInfo(n), nil
}
