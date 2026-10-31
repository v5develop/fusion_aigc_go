package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
)

// ---------------- 模板 ----------------

func toTemplate(t *model.Template, withSnapshot bool) *canvas.TemplateInfo {
	out := &canvas.TemplateInfo{
		Id: t.ID, Scope: t.Scope, Title: t.Title, Description: t.Description, CoverUrl: t.CoverURL, Sort: t.Sort,
		Enabled: t.Enabled == 1, UserId: t.UserID.Int64, CreatedAt: ms(t.CreatedAt), UpdatedAt: ms(t.UpdatedAt),
	}
	if d, err := ParseSnapshot(t.Snapshot, 0); err == nil {
		out.NodeCount, out.EdgeCount = int32(len(d.Nodes)), int32(len(d.Edges))
	}
	if withSnapshot {
		out.Snapshot = string(t.Snapshot)
	}
	return out
}

func visibleTemplate(t *model.Template, userID int64) bool {
	if t == nil || t.Deleted == 1 {
		return false
	}
	if t.Scope == model.ScopeOfficial {
		return t.Enabled == 1
	}
	return t.UserID.Valid && t.UserID.Int64 == userID
}

func (c *core) ListTemplates(ctx context.Context, in *canvas.ListTemplatesReq) (*canvas.ListTemplatesResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	scope := in.Scope
	switch scope {
	case "", model.ScopeOfficial:
		scope = model.ScopeOfficial
	case model.ScopeUser, "all":
	default:
		return nil, xerr.Param("scope 只支持 official / user / all")
	}
	list, err := c.s.Repo.Template.ListVisible(ctx, in.UserId, scope)
	if err != nil {
		return nil, err
	}
	// 列表不带快照（≤200 条全量快照会超过 gRPC 4MB），节点 / 连线数照给；快照走 GetTemplate
	out := &canvas.ListTemplatesResp{List: make([]*canvas.TemplateInfo, 0, len(list))}
	for _, t := range list {
		out.List = append(out.List, toTemplate(t, false))
	}
	return out, nil
}

func (c *core) GetTemplate(ctx context.Context, in *canvas.TemplateIdReq) (*canvas.TemplateInfo, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	t, err := c.s.Repo.Template.Get(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if !visibleTemplate(t, in.UserId) {
		return nil, xerr.NotFoundf("模板")
	}
	return toTemplate(t, true), nil
}

func (c *core) CreateTemplateFromCanvas(ctx context.Context, in *canvas.CreateTemplateFromCanvasReq) (*canvas.TemplateInfo, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	cv, err := c.s.Repo.Canvas.GetOwned(ctx, in.UserId, in.CanvasId)
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, xerr.NotFoundf("画布")
	}
	title, err := c.cleanTitle(firstNonEmpty(in.Title, cv.Title))
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return nil, xerr.Param("模板说明最多 500 字")
	}
	d, err := ParseSnapshot(cv.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	d.StripAllResults()
	t := &model.Template{
		Scope: model.ScopeUser, UserID: sql.NullInt64{Int64: in.UserId, Valid: true}, Title: title,
		Description: strings.TrimSpace(in.Description), Snapshot: d.Marshal(), CoverURL: d.Cover(), Enabled: 1,
	}
	if err := c.s.Repo.Template.Insert(ctx, t); err != nil {
		return nil, err
	}
	t.CreatedAt, t.UpdatedAt = c.now(), c.now()
	return toTemplate(t, true), nil
}

// CreateCanvasFromTemplate 复制模板快照成新画布：节点 / 连线 id 全部重新生成，内部连线与 data 内引用同步改写。
func (c *core) CreateCanvasFromTemplate(ctx context.Context, in *canvas.CreateCanvasFromTemplateReq) (*canvas.CanvasDoc, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	t, err := c.s.Repo.Template.Get(ctx, in.TemplateId)
	if err != nil {
		return nil, err
	}
	if !visibleTemplate(t, in.UserId) {
		return nil, xerr.NotFoundf("模板")
	}
	title, err := c.cleanTitle(firstNonEmpty(in.Title, t.Title))
	if err != nil {
		return nil, err
	}
	d, err := ParseSnapshot(t.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	d.RemapIDs(NewID)
	d.StripAllResults()
	x, snap, err := c.insertCanvas(ctx, in.UserId, in.WorkspaceId, title, d)
	if err != nil {
		return nil, err
	}
	return toDoc(x, snap), nil
}

func (c *core) DeleteTemplate(ctx context.Context, in *canvas.TemplateIdReq) (*canvas.Empty, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	t, err := c.s.Repo.Template.Get(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if t != nil && t.Deleted == 0 && t.Scope == model.ScopeOfficial {
		return nil, xerr.New(xerr.Conflict, "官方模板不可删除")
	}
	ok, err := c.s.Repo.Template.SoftDeleteUser(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("模板")
	}
	return &canvas.Empty{}, nil
}

// ---------------- 提示词库 ----------------

func toPrompt(p *model.PromptItem) *canvas.PromptItem {
	out := &canvas.PromptItem{
		Id: p.ID, Title: p.Title, Prompt: p.Prompt, Vertical: p.Vertical, Group: p.GroupKey, Source: p.Source,
		Owner: "official", Enabled: p.Enabled == 1, Sort: p.Sort, UserId: p.UserID.Int64,
		CreatedAt: ms(p.CreatedAt), UpdatedAt: ms(p.UpdatedAt), Models: []string{}, Tags: []string{},
	}
	if !p.Official() {
		out.Owner = "mine"
	}
	_ = json.Unmarshal(p.Models, &out.Models)
	_ = json.Unmarshal(p.Tags, &out.Tags)
	return out
}

// 提示词限制。
const (
	maxPromptTitle = 100
	maxListItems   = 10
	maxItemLen     = 32
)

func cleanList(name string, xs []string) ([]string, error) {
	out := make([]string, 0, len(xs))
	seen := map[string]bool{}
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		if utf8.RuneCountInString(x) > maxItemLen {
			return nil, xerr.Param(fmt.Sprintf("%s 单项最多 %d 字", name, maxItemLen))
		}
		seen[x] = true
		out = append(out, x)
	}
	if len(out) > maxListItems {
		return nil, xerr.Param(fmt.Sprintf("%s 最多 %d 项", name, maxListItems))
	}
	return out, nil
}

// checkPrompt 校验标题 / 正文 / models / tags，返回 JSON 化的 models、tags。
func (c *core) checkPrompt(title, prompt string, models, tags []string) (json.RawMessage, json.RawMessage, error) {
	switch {
	case strings.TrimSpace(title) == "":
		return nil, nil, xerr.New(xerr.ParamMissing, "缺少标题")
	case utf8.RuneCountInString(title) > maxPromptTitle:
		return nil, nil, xerr.Param("标题最多 100 字")
	case strings.TrimSpace(prompt) == "":
		return nil, nil, xerr.New(xerr.ParamMissing, "缺少提示词正文")
	case utf8.RuneCountInString(prompt) > c.s.Config.Limits.MaxPromptLen:
		return nil, nil, xerr.Param(fmt.Sprintf("提示词正文最多 %d 字", c.s.Config.Limits.MaxPromptLen))
	}
	ms, err := cleanList("models", models)
	if err != nil {
		return nil, nil, err
	}
	if len(ms) == 0 {
		ms = []string{"通用"}
	}
	ts, err := cleanList("tags", tags)
	if err != nil {
		return nil, nil, err
	}
	mb, _ := json.Marshal(ms)
	tb, _ := json.Marshal(ts)
	return mb, tb, nil
}

func (c *core) ListPrompts(ctx context.Context, in *canvas.ListPromptsReq) (*canvas.ListPromptsResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	switch in.Vertical {
	case "", model.VerticalEcommerce, model.VerticalGeneral, model.VerticalMine:
	default:
		return nil, xerr.Param("vertical 只支持 ecommerce / general / mine")
	}
	off, lim := repo.Page(int64(in.Page), int64(in.PageSize), 20, 50)
	list, total, err := c.s.Repo.Prompt.ListVisible(ctx, repo.PromptFilter{
		UserID: in.UserId, Vertical: in.Vertical, Group: strings.TrimSpace(in.Group), Model: strings.TrimSpace(in.Model),
		Q: strings.TrimSpace(in.Q), Offset: off, Limit: lim,
	})
	if err != nil {
		return nil, err
	}
	out := &canvas.ListPromptsResp{Total: total, List: make([]*canvas.PromptItem, 0, len(list))}
	for _, p := range list {
		out.List = append(out.List, toPrompt(p))
	}
	return out, nil
}

func (c *core) CreatePrompt(ctx context.Context, in *canvas.UpsertPromptReq) (*canvas.PromptItem, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	mb, tb, err := c.checkPrompt(in.Title, in.Prompt, in.Models, in.Tags)
	if err != nil {
		return nil, err
	}
	n, err := c.s.Repo.Prompt.CountByUser(ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	if n >= int64(c.s.Config.Limits.MaxPromptsPerUser) {
		return nil, xerr.Param(fmt.Sprintf("个人提示词最多 %d 条", c.s.Config.Limits.MaxPromptsPerUser))
	}
	p := &model.PromptItem{
		UserID: sql.NullInt64{Int64: in.UserId, Valid: true}, Title: strings.TrimSpace(in.Title), Prompt: in.Prompt,
		Vertical: model.VerticalMine, GroupKey: model.GroupFavorite, Models: mb, Tags: tb, Source: "我的", Enabled: 1,
	}
	if err := c.s.Repo.Prompt.Insert(ctx, p); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt = c.now(), c.now()
	return toPrompt(p), nil
}

// ownPrompt 取本人的提示词：官方 4002，不存在 / 别人的 4001。
func (c *core) ownPrompt(ctx context.Context, userID, id int64) (*model.PromptItem, error) {
	if err := needUser(userID); err != nil {
		return nil, err
	}
	p, err := c.s.Repo.Prompt.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case p == nil:
		return nil, xerr.NotFoundf("提示词")
	case p.Official():
		return nil, xerr.New(xerr.Conflict, "官方提示词不可修改")
	case p.UserID.Int64 != userID:
		return nil, xerr.NotFoundf("提示词")
	}
	return p, nil
}

func (c *core) UpdatePrompt(ctx context.Context, in *canvas.UpsertPromptReq) (*canvas.PromptItem, error) {
	p, err := c.ownPrompt(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	mb, tb, err := c.checkPrompt(in.Title, in.Prompt, in.Models, in.Tags)
	if err != nil {
		return nil, err
	}
	p.Title, p.Prompt, p.Models, p.Tags = strings.TrimSpace(in.Title), in.Prompt, mb, tb
	ok, err := c.s.Repo.Prompt.UpdateContent(ctx, p, in.UserId)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("提示词")
	}
	p.UpdatedAt = c.now()
	return toPrompt(p), nil
}

func (c *core) DeletePrompt(ctx context.Context, in *canvas.PromptIdReq) (*canvas.Empty, error) {
	if _, err := c.ownPrompt(ctx, in.UserId, in.Id); err != nil {
		return nil, err
	}
	ok, err := c.s.Repo.Prompt.SoftDeleteUser(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("提示词")
	}
	return &canvas.Empty{}, nil
}
