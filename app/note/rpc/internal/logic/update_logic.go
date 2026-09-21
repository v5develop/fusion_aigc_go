package logic

import (
	"context"
	"strings"

	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

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

// 编辑已完成的笔记：标题、正文、话题、图片（增删）
func (l *UpdateLogic) Update(in *pb.UpdateReq) (*pb.NoteInfo, error) {
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if n.Status != model.StatusDone {
		return nil, xerr.New(xerr.Conflict, "笔记生成完成后才能编辑")
	}
	title, content := strings.TrimSpace(in.Title), strings.TrimSpace(in.Content)
	tags := normalizeTags(in.Tags, l.svcCtx.Config.Limits.MaxTags)
	if err := checkNoteText(l.ctx, l.svcCtx, "note_edit", title, content, tags); err != nil {
		return nil, err
	}
	// 图片：原有的可删可留，新增的须来自上传接口
	current := map[string]bool{}
	for _, u := range parseList(n.ImageUrls) {
		current[u] = true
	}
	images := make([]string, 0, len(in.ImageUrls))
	seen := map[string]bool{}
	for _, u := range in.ImageUrls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		if !current[u] {
			if err := storage.Owned(l.svcCtx.Storage, u); err != nil {
				return nil, xerr.Param("新增图片" + err.Error())
			}
		}
		seen[u] = true
		images = append(images, u)
	}
	// 上限 3 张；生成的张数更多时，可以保留全部生成图，但不能再往上加
	max := max(l.svcCtx.Config.Limits.MaxEditImages, int(n.ImageCount))
	switch {
	case len(images) == 0:
		return nil, xerr.Param("至少保留一张图片")
	case len(images) > max:
		return nil, xerr.Newf(xerr.ParamError, "图片最多 %d 张", max)
	}
	ok, err := l.svcCtx.Note.UpdateContent(l.ctx, n.Id, title, content, jsonList(tags), jsonList(images))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.New(xerr.Conflict, "笔记生成完成后才能编辑")
	}
	if n, err = l.svcCtx.Note.FindOne(l.ctx, n.Id); err != nil {
		return nil, err
	}
	// 素材库跟着改：先删旧的一组，再按编辑后的标题与图片重写
	if l.svcCtx.Config.Asset.Enabled {
		if err := l.svcCtx.Asset.SoftDeleteByTask(l.ctx, n.UserId, n.GenTaskId.String); err != nil {
			l.Errorf("sync assets of note %d: %v", n.Id, err)
		} else {
			saveNoteAssets(l.ctx, l.svcCtx, n, images)
		}
	}
	return toInfo(n), nil
}
