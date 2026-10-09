package logic

import (
	"context"
	"encoding/json"
	"time"

	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"

	"github.com/zeromicro/go-zero/core/logx"
)

func newTaskID(svcCtx *svc.ServiceContext) string {
	return aigc.NewTaskID(svcCtx.Config.TaskIDPrefix)
}

// submitPlan 提交构思任务（74）；提交失败直接标记构思失败。
func submitPlan(ctx context.Context, svcCtx *svc.ServiceContext, n *model.AfvImageTextNote) {
	task := aigc.NoteScript{
		UserText: n.Topic, NoteType: noteTypeLabel(n.NoteType), MaterialURLs: parseList(n.MaterialUrls),
		PersonURL: n.PersonUrl.String, ImageCount: int(n.ImageCount),
		ShopProfile: shopProfileText(ctx, svcCtx, n.ShopProfileId.Int64, n.UserId),
	}
	if _, err := svcCtx.Aigc.Submit(ctx, n.PlanTaskId.String, task); err != nil {
		logx.WithContext(ctx).Errorf("submit plan %d: %v", n.Id, err)
		failPlan(ctx, svcCtx, n.Id, "提交构思失败，请重试")
		return
	}
	// TODO 计费：等计费方案确认后，构思扣 10 点，失败退回
	// TODO 任务：确认后写 afv_creation_task（mode=74，execution_task_id=plan_task_id）
}

// submitGenerate 提交出图任务（75）；提交失败直接标记生成失败。
func submitGenerate(ctx context.Context, svcCtx *svc.ServiceContext, id int64, taskID string, task aigc.NoteImages) {
	if _, err := svcCtx.Aigc.Submit(ctx, taskID, task); err != nil {
		logx.WithContext(ctx).Errorf("submit generate %d: %v", id, err)
		failGenerate(ctx, svcCtx, id, "提交生成失败，请重试")
		return
	}
	// TODO 计费：等计费方案确认后，生成扣 100 点，失败退回
	// TODO 任务：确认后写 afv_creation_task（mode=75，parent 为构思任务）
}

func failPlan(ctx context.Context, svcCtx *svc.ServiceContext, id int64, reason string) {
	if _, err := svcCtx.Note.FinishPlan(ctx, id, nil, reason); err != nil {
		logx.WithContext(ctx).Errorf("mark plan failed %d: %v", id, err)
	}
}

func failGenerate(ctx context.Context, svcCtx *svc.ServiceContext, id int64, reason string) {
	if _, err := svcCtx.Note.FinishGenerate(ctx, id, model.StatusGenFailed, "", reason); err != nil {
		logx.WithContext(ctx).Errorf("mark generate failed %d: %v", id, err)
	}
}

// Poller 定时查询执行面结果，并回收超时的记录。状态都在库里，进程重启后接着查。
type Poller struct {
	svcCtx *svc.ServiceContext
	cancel context.CancelFunc
	done   chan struct{}
}

func NewPoller(svcCtx *svc.ServiceContext) *Poller {
	return &Poller{svcCtx: svcCtx, done: make(chan struct{})}
}

func (p *Poller) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	defer close(p.done)

	ticker := time.NewTicker(time.Duration(p.svcCtx.Config.Poll.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	logx.Info("note poller started")
	for {
		p.pollPlans(ctx)
		p.pollGenerates(ctx)
		p.svcCtx.TaskCenter.Sync(ctx) // 本轮的状态变化同步进任务中心
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) Stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}

const pollBatch = 100

func (p *Poller) pollPlans(ctx context.Context) {
	svcCtx := p.svcCtx
	list, err := svcCtx.Note.ListByStatus(ctx, model.StatusPlanning, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list planning: %v", err)
		return
	}
	deadline := time.Duration(svcCtx.Config.Plan.TimeoutMinutes) * time.Minute
	for _, n := range list {
		r := p.query(ctx, n.PlanTaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess:
			p.finishPlan(ctx, n, r.Script)
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("note %d plan %s %s: %s", n.Id, n.PlanTaskId.String, r.Status, r.ErrorMsg)
			failPlan(ctx, svcCtx, n.Id, "构思失败，请重试")
		case time.Since(n.UpdateTime) > deadline:
			failPlan(ctx, svcCtx, n.Id, "构思超时，请重试")
		}
	}
}

// finishPlan 解析 74 的 script，审核后写入规划结果。
// script 里的参考图（images_url）在执行面临时目录 aigc_temp/，先转存到我们的目录再落库，
// 否则用户隔一段时间再点生成，75 会取不到参考图。转存失败保持构思中，下一轮重试。
func (p *Poller) finishPlan(ctx context.Context, n *model.AfvImageTextNote, raw json.RawMessage) {
	svcCtx := p.svcCtx
	s, err := parseScript(raw)
	if err != nil {
		logx.WithContext(ctx).Errorf("note %d parse script: %v", n.Id, err)
		failPlan(ctx, svcCtx, n.Id, "构思结果异常，请重试")
		return
	}
	tags := normalizeTags(s.Note.Tags, svcCtx.Config.Limits.MaxTags)
	if r, err := svcCtx.Moderation.Check(ctx, "note_plan_output", s.Note.Title+"\n"+s.Note.Content+"\n"+jsonList(tags)); err == nil && !r.Passed {
		logx.WithContext(ctx).Infof("note %d plan blocked, label=%s", n.Id, r.Label)
		failPlan(ctx, svcCtx, n.Id, "构思内容未通过审核，请调整主题后重试")
		return
	}
	if len(s.ImagesURL) > 0 {
		urls := transferAll(ctx, svcCtx, s.ImagesURL, "note_ref", n.UserId)
		if len(urls) == 0 {
			logx.WithContext(ctx).Errorf("note %d 参考图转存失败，下一轮重试", n.Id)
			return
		}
		if raw, err = replaceImagesURL(raw, urls); err != nil {
			logx.WithContext(ctx).Errorf("note %d rewrite script: %v", n.Id, err)
			failPlan(ctx, svcCtx, n.Id, "构思结果异常，请重试")
			return
		}
	}
	plan := &model.Plan{Script: string(raw), Title: s.Note.Title, Content: s.Note.Content, Tags: jsonList(tags)}
	if _, err := svcCtx.Note.FinishPlan(ctx, n.Id, plan, ""); err != nil {
		logx.WithContext(ctx).Errorf("finish plan %d: %v", n.Id, err)
	}
}

func (p *Poller) pollGenerates(ctx context.Context) {
	svcCtx := p.svcCtx
	list, err := svcCtx.Note.ListByStatus(ctx, model.StatusGenerating, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list generating: %v", err)
		return
	}
	deadline := time.Duration(svcCtx.Config.Generate.TimeoutMinutes) * time.Minute
	for _, n := range list {
		r := p.query(ctx, n.GenTaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess:
			if len(r.Outputs()) == 0 {
				failGenerate(ctx, svcCtx, n.Id, "生成结果为空，请重试")
				continue
			}
			// 执行面产物在上游私有桶，转存到公有桶后再落库
			urls := transferAll(ctx, svcCtx, r.Outputs(), "note", n.UserId)
			if len(urls) == 0 {
				logx.WithContext(ctx).Errorf("note %d 转存失败，下一轮重试", n.Id)
				continue
			}
			if ok, err := svcCtx.Note.FinishGenerate(ctx, n.Id, model.StatusDone, jsonList(urls), ""); err != nil {
				logx.WithContext(ctx).Errorf("finish generate %d: %v", n.Id, err)
			} else if ok {
				saveNoteAssets(ctx, svcCtx, n, urls) // 设计稿：成品自动存进素材库
			}
			// TODO 计费：成功结算
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("note %d generate %s %s: %s", n.Id, n.GenTaskId.String, r.Status, r.ErrorMsg)
			failGenerate(ctx, svcCtx, n.Id, "生成失败，请重试")
		case time.Since(n.UpdateTime) > deadline:
			failGenerate(ctx, svcCtx, n.Id, "生成超时，请重试")
		}
	}
}

// query 查一次结果；网络抖动等查询失败返回 nil，下一轮再查。
func (p *Poller) query(ctx context.Context, taskID string) *aigc.Result {
	if taskID == "" || !p.svcCtx.Aigc.Enabled() {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r, err := p.svcCtx.Aigc.GetResult(qctx, taskID)
	if err != nil {
		logx.WithContext(ctx).Errorf("get result %s: %v", taskID, err)
		return nil
	}
	return r
}

// saveNoteAssets 笔记成品写进素材库：每张图一行，用 source_task_id 串成一组（同现有套图的存法）。
func saveNoteAssets(ctx context.Context, svcCtx *svc.ServiceContext, n *model.AfvImageTextNote, urls []string) {
	cfg := svcCtx.Config.Asset
	if !cfg.Enabled {
		return
	}
	var prompts []string
	if s, err := parseScript([]byte(n.PlanScript.String)); err == nil {
		for _, img := range s.Images {
			prompts = append(prompts, img.ImagePrompt)
		}
	}
	props, _ := json.Marshal(map[string]any{"noteId": n.Id, "noteType": n.NoteType, "tags": parseList(n.Tags)})
	list := make([]asset.Asset, 0, len(urls))
	for i, u := range urls {
		a := asset.Asset{
			UserID: n.UserId, SourceTaskID: n.GenTaskId.String, Type: cfg.Type, EntryCode: cfg.EntryCode,
			Name: n.Title, CoverURL: u, ResultURL: u, AspectRatio: svcCtx.Config.Generate.Size,
			Properties: string(props),
		}
		if i < len(prompts) {
			a.AIPrompt = prompts[i]
		}
		list = append(list, a)
	}
	if err := svcCtx.Asset.InsertSet(ctx, list); err != nil {
		logx.WithContext(ctx).Errorf("save note assets %d: %v", n.Id, err)
	}
}

// transferAll 把执行面产物转存到公有桶（上游私有桶不对外可读也不长期保留）。
// 任一失败返回空，调用方保持生成中，下一轮重试。
func transferAll(ctx context.Context, svcCtx *svc.ServiceContext, srcURLs []string, kind string, userID int64) []string {
	out := make([]string, 0, len(srcURLs))
	for _, src := range srcURLs {
		u, err := svcCtx.Storage.Transfer(ctx, src, kind, userID)
		if err != nil {
			logx.WithContext(ctx).Errorf("transfer %s: %v", src, err)
			return nil
		}
		out = append(out, u)
	}
	return out
}
