package logic

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

func (c *core) AdminListRuns(ctx context.Context, in *canvas.AdminListRunsReq) (*canvas.AdminListRunsResp, error) {
	off, lim := repo.Page(in.Page, in.PageSize, 20, 100)
	list, total, err := c.s.Repo.Run.AdminList(ctx, repo.AdminFilter{
		UserID: in.UserId, CanvasID: in.CanvasId, Kind: in.Kind, Backend: in.Backend, Status: in.Status,
		StartMs: in.Start, EndMs: in.End, Offset: off, Limit: lim,
	})
	if err != nil {
		return nil, err
	}
	out := &canvas.AdminListRunsResp{Total: total, List: make([]*canvas.AdminRun, 0, len(list))}
	for _, r := range list {
		a := &canvas.AdminRun{
			Run: c.toResult(r), Id: r.ID, UserId: r.UserID, Backend: string(r.Backend), BackendRef: r.BackendRef.String,
			ModelGroup: r.ModelGroup, BillingRequestId: r.BillingRequestID.String, BillingStatus: r.BillingStatus,
			CostCny: r.CostCNY, Attempt: r.Attempt, Request: string(r.Request),
		}
		if r.StartedAt.Valid {
			a.StartedAt = ms(r.StartedAt.Time)
		}
		out.List = append(out.List, a)
	}
	return out, nil
}

// AdminForceFailRun 运营强制失败一个未结束的 run：gateway 后端退款并取消上游调用；studio 取消批次（由 studio 退款）；
// 已提交的 Java 工具用 BearerCache 里该用户最近的 token 调 Java 取消接口（由 Java 退款）；没有或已失效 → 退化为停止跟踪，
// error_reason 注明「Java 任务未能取消（无有效登录态），可能仍会执行并扣费」，并打 WARN（check=canvas.java_cancel_degraded）。
func (c *core) AdminForceFailRun(ctx context.Context, in *canvas.AdminRunReq) (*canvas.Empty, error) {
	r, err := c.s.Repo.Run.Get(ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, xerr.NotFoundf("执行记录")
	}
	if model.RunFinal(r.Status) {
		return nil, xerr.New(xerr.Conflict, "执行已结束，不能强制失败")
	}
	msg := "运营已终止该任务"
	if s := strings.TrimSpace(in.Reason); s != "" {
		msg += "：" + s
	}
	if javaLive(r) { // 运行中的 Java 任务，或排队中已发过请求、可按幂等键找回的
		// 文案主体截短，给「Java 任务是否已取消」的后缀留位置（error_reason 255 字）
		ended, pending := c.stopJava(ctx, r, stopAdmin, repo.Trunc(msg, 120), c.s.Bearers.Get(r.UserID), 0)
		if !ended && !pending {
			return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
		}
		logx.WithContext(ctx).Infof("canvas: 管理员 %d 强制失败 Java run %d（已结束=%v，待重试取消=%v）：%s", in.AdminId, r.ID, ended, pending, msg)
		return &canvas.Empty{}, nil
	}
	if !c.endRun(ctx, r, r.Status, model.RunFailed, "ADMIN_FORCE_FAIL", msg, true, false) {
		return nil, xerr.New(xerr.Conflict, "执行状态已变化，请刷新")
	}
	c.cancelUpstream(ctx, r, "运营强制失败")
	c.reclaimUnrefCalls(ctx, r, "运营强制失败")
	logx.WithContext(ctx).Infof("canvas: 管理员 %d 强制失败 run %d：%s", in.AdminId, r.ID, msg)
	return &canvas.Empty{}, nil
}

func (c *core) AdminListTemplates(ctx context.Context, in *canvas.AdminListTemplatesReq) (*canvas.AdminListTemplatesResp, error) {
	off, lim := repo.Page(in.Page, in.PageSize, 20, 100)
	list, total, err := c.s.Repo.Template.AdminList(ctx, in.Scope, in.Enabled, strings.TrimSpace(in.Q), off, lim)
	if err != nil {
		return nil, err
	}
	out := &canvas.AdminListTemplatesResp{Total: total, List: make([]*canvas.TemplateInfo, 0, len(list))}
	for _, t := range list {
		out.List = append(out.List, toTemplate(t, true))
	}
	return out, nil
}

func boolInt8(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

func (c *core) AdminUpsertOfficialTemplate(ctx context.Context, in *canvas.AdminUpsertTemplateReq) (*canvas.TemplateInfo, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" || utf8.RuneCountInString(title) > 100 {
		return nil, xerr.Param("标题必填，最多 100 字")
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return nil, xerr.Param("说明最多 500 字")
	}
	d, err := ParseSnapshot([]byte(in.Snapshot), c.s.Config.Limits.MaxSnapshotBytes)
	if err != nil {
		return nil, err
	}
	if err := ValidateDoc(d, c.s.Config.Limits.MaxNodes, c.imageLimit); err != nil {
		return nil, err
	}
	d.StripAllResults()
	t := &model.Template{
		ID: in.Id, Scope: model.ScopeOfficial, Title: title, Description: strings.TrimSpace(in.Description),
		Snapshot: d.Marshal(), CoverURL: strings.TrimSpace(in.CoverUrl), Sort: in.Sort, Enabled: boolInt8(in.Enabled),
	}
	if in.Id == 0 {
		if err := c.s.Repo.Template.Insert(ctx, t); err != nil {
			return nil, err
		}
	} else {
		ok, err := c.s.Repo.Template.UpdateOfficial(ctx, t)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, xerr.NotFoundf("官方模板")
		}
	}
	got, err := c.s.Repo.Template.Get(ctx, t.ID)
	if err != nil || got == nil {
		return toTemplate(t, true), err
	}
	return toTemplate(got, true), nil
}

func (c *core) AdminToggleTemplate(ctx context.Context, in *canvas.AdminToggleReq) (*canvas.Empty, error) {
	ok, err := c.s.Repo.Template.SetEnabled(ctx, in.Id, in.Enabled)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("模板")
	}
	return &canvas.Empty{}, nil
}

func (c *core) AdminListPrompts(ctx context.Context, in *canvas.AdminListPromptsReq) (*canvas.AdminListPromptsResp, error) {
	off, lim := repo.Page(in.Page, in.PageSize, 20, 100)
	list, total, err := c.s.Repo.Prompt.AdminList(ctx, in.Scope, in.Enabled, repo.PromptFilter{
		Vertical: in.Vertical, Group: in.Group, Q: strings.TrimSpace(in.Q), Offset: off, Limit: lim,
	})
	if err != nil {
		return nil, err
	}
	out := &canvas.AdminListPromptsResp{Total: total, List: make([]*canvas.PromptItem, 0, len(list))}
	for _, p := range list {
		out.List = append(out.List, toPrompt(p))
	}
	return out, nil
}

// officialGroups 官方提示词二级分类（docs/api/api-canvas.md 8.1）。
var officialGroups = map[string]map[string]bool{
	model.VerticalEcommerce: {"model": true, "scene": true, "product": true, "detail": true, "apparel": true},
	model.VerticalGeneral:   {"photo": true, "composition": true, "style": true, "material": true, "text": true},
}

func (c *core) AdminUpsertOfficialPrompt(ctx context.Context, in *canvas.AdminUpsertPromptReq) (*canvas.PromptItem, error) {
	groups, ok := officialGroups[in.Vertical]
	if !ok {
		return nil, xerr.Param("官方提示词的 vertical 只能是 ecommerce / general")
	}
	if !groups[in.Group] {
		return nil, xerr.Param("group 不在该一级分类的取值内")
	}
	mb, tb, err := c.checkPrompt(in.Title, in.Prompt, in.Models, in.Tags)
	if err != nil {
		return nil, err
	}
	p := &model.PromptItem{
		ID: in.Id, Title: strings.TrimSpace(in.Title), Prompt: in.Prompt, Vertical: in.Vertical, GroupKey: in.Group,
		Models: mb, Tags: tb, Source: repo.Trunc(strings.TrimSpace(in.Source), 64), Sort: in.Sort, Enabled: boolInt8(in.Enabled),
		UserID: sql.NullInt64{},
	}
	if in.Id == 0 {
		if err := c.s.Repo.Prompt.Insert(ctx, p); err != nil {
			return nil, err
		}
	} else {
		ok, err := c.s.Repo.Prompt.UpdateContent(ctx, p, 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, xerr.NotFoundf("官方提示词")
		}
	}
	got, err := c.s.Repo.Prompt.Get(ctx, p.ID)
	if err != nil || got == nil {
		return toPrompt(p), err
	}
	return toPrompt(got), nil
}

func (c *core) AdminTogglePrompt(ctx context.Context, in *canvas.AdminToggleReq) (*canvas.Empty, error) {
	ok, err := c.s.Repo.Prompt.SetEnabled(ctx, in.Id, in.Enabled)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("官方提示词")
	}
	return &canvas.Empty{}, nil
}

func (c *core) AdminCostSummary(ctx context.Context, in *canvas.AdminCostSummaryReq) (*canvas.AdminCostSummaryResp, error) {
	rows, err := c.s.Repo.Run.CostSummary(ctx, in.Start, in.End, in.GroupBy)
	if err != nil {
		return nil, err
	}
	out := &canvas.AdminCostSummaryResp{Rows: make([]*canvas.CostRow, 0, len(rows))}
	for _, r := range rows {
		out.Rows = append(out.Rows, &canvas.CostRow{
			Key: r.Key, Runs: r.Runs, Succeeded: r.Succeeded, Failed: r.Failed, Canceled: r.Canceled, CostCny: r.Cost, Points: r.Points,
		})
	}
	return out, nil
}
