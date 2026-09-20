package logic

import (
	"context"
	"encoding/json"
	"time"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/pagex"

	"github.com/zeromicro/go-zero/core/logx"
)

func pagexOffset(page, size int64) (int64, int64) { return pagex.Offset(page, size) }

func newTaskID(svcCtx *svc.ServiceContext) string {
	return aigc.NewTaskID(svcCtx.Config.TaskIDPrefix)
}

// imageTask 一张图对应的执行面任务与结果。一次生成 N 张就是 N 个任务。
type imageTask struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"` // 空=进行中；success / failed
	URL    string `json:"url,omitempty"`
	Error  string `json:"error,omitempty"`
}

func parseTasks(s string) []imageTask {
	var t []imageTask
	_ = json.Unmarshal([]byte(s), &t)
	return t
}

func dumpTasks(t []imageTask) string {
	b, _ := json.Marshal(t)
	return string(b)
}

// buildTask 按类型组装执行面入参：海报 78，长图 76。
//
// poster_type / poster_style 是喂给模型的，传中文展示名（如「节日」「扁平插画」）；
// 库里存的是编码，展示名在提交前查出来。
func buildTask(c *model.AfvImageCreation, cfg *config.KindConf, mats []string, typeLabel string) aigc.Task {
	if c.Kind == model.KindPoster {
		style := ""
		if c.PosterStyle.Valid {
			style, _ = styleLabel(c.PosterStyle.String)
		}
		return aigc.Poster{
			Template: c.TemplateUrl.String, Materials: mats, UserText: c.Content,
			PosterType: typeLabel, PosterStyle: style, Size: c.Size, Model: cfg.Model,
		}
	}
	return aigc.LongImage{
		UserText: c.Content, TemplateURL: c.TemplateUrl.String, MaterialURLs: mats, Size: c.Size,
	}
}

// submit 一张图一个任务，逐个提交；提交失败的那张直接记失败，其余照常。
func submit(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvImageCreation, cfg *config.KindConf, typeLabel string) []imageTask {
	mats := parseList(c.MaterialUrls)
	task := buildTask(c, cfg, mats, typeLabel)
	tasks := make([]imageTask, 0, c.ImageCount)
	for i := int64(0); i < c.ImageCount; i++ {
		id := newTaskID(svcCtx)
		if _, err := svcCtx.Aigc.Submit(ctx, id, task); err != nil {
			logx.WithContext(ctx).Errorf("submit %s %d (%d/%d): %v", c.Kind, c.Id, i+1, c.ImageCount, err)
			tasks = append(tasks, imageTask{TaskID: id, Status: aigc.StatusFailed, Error: "提交生成失败"})
			continue
		}
		tasks = append(tasks, imageTask{TaskID: id})
	}
	// TODO 计费：等计费方案确认后，这里预扣 100 点（按次还是按张待定），失败退回
	// TODO 任务：确认后写 afv_creation_task（海报 mode=78 / 长图 mode=76）
	return tasks
}

// Poller 定时查询执行面结果。状态都在库里，进程重启后接着查。
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
	logx.Info("imagegen poller started")
	for {
		p.tick(ctx)
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

func (p *Poller) tick(ctx context.Context) {
	svcCtx := p.svcCtx
	rows, err := svcCtx.Creation.ListGenerating(ctx, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list generating: %v", err)
		return
	}
	timeout := time.Duration(svcCtx.Config.Poll.TimeoutMinutes) * time.Minute
	for _, c := range rows {
		p.pollOne(ctx, c, time.Since(c.UpdateTime) > timeout)
	}
}

// pollOne 查这条记录名下还没结束的任务；全部结束（或超时）就收尾。
func (p *Poller) pollOne(ctx context.Context, c *model.AfvImageCreation, timedOut bool) {
	svcCtx := p.svcCtx
	tasks := parseTasks(c.Tasks.String)
	changed := false
	for i := range tasks {
		if tasks[i].Status != "" {
			continue
		}
		r := p.query(ctx, tasks[i].TaskID)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess && r.GeneratedImagePath != "":
			tasks[i].Status, tasks[i].URL, changed = aigc.StatusSuccess, r.GeneratedImagePath, true
		case r != nil && r.Done():
			tasks[i].Status, tasks[i].Error, changed = aigc.StatusFailed, r.ErrorMsg, true
			logx.WithContext(ctx).Infof("%s %d task %s %s: %s", c.Kind, c.Id, tasks[i].TaskID, r.Status, r.ErrorMsg)
		case timedOut:
			tasks[i].Status, tasks[i].Error, changed = aigc.StatusFailed, "生成超时", true
		}
	}
	var urls []string
	pending := 0
	for _, t := range tasks {
		switch t.Status {
		case "":
			pending++
		case aigc.StatusSuccess:
			urls = append(urls, t.URL)
		}
	}
	if pending > 0 { // 还有没出结果的，先把进展存下来
		if changed {
			if _, err := svcCtx.Creation.UpdateTasks(ctx, c.Id, dumpTasks(tasks), model.StatusGenerating, "", ""); err != nil {
				logx.WithContext(ctx).Errorf("update tasks %d: %v", c.Id, err)
			}
		}
		return
	}
	// 没有待处理的任务就收尾。提交阶段就全失败的记录这时也能结算，不会卡在生成中。

	status, reason := model.StatusDone, ""
	if len(urls) == 0 {
		status, reason = model.StatusFailed, "生成失败，请重试"
	}
	ok, err := svcCtx.Creation.UpdateTasks(ctx, c.Id, dumpTasks(tasks), status, jsonList(urls), reason)
	if err != nil {
		logx.WithContext(ctx).Errorf("finish %d: %v", c.Id, err)
		return
	}
	if ok && status == model.StatusDone {
		saveAssets(ctx, svcCtx, c, urls)
	}
	// TODO 计费：成功结算 / 失败退回
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

// saveAssets 成品写进素材库：每张图一行，用第一个任务 ID 串成一组。
func saveAssets(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvImageCreation, urls []string) {
	if !svcCtx.Config.Asset.Enabled {
		return
	}
	cfg, err := kindConf(svcCtx, c.Kind)
	if err != nil {
		return
	}
	props, _ := json.Marshal(map[string]any{"creationId": c.Id, "kind": c.Kind, "posterType": c.PosterType.String})
	group := groupTaskID(c)
	list := make([]asset.Asset, 0, len(urls))
	for _, u := range urls {
		list = append(list, asset.Asset{
			UserID: c.UserId, SourceTaskID: group, Type: cfg.AssetType, EntryCode: cfg.AssetEntryCode,
			Name: c.Content, CoverURL: u, ResultURL: u, AspectRatio: c.Size, AIPrompt: c.Content,
			Properties: string(props),
		})
	}
	if err := svcCtx.Asset.InsertSet(ctx, list); err != nil {
		logx.WithContext(ctx).Errorf("save assets of %s %d: %v", c.Kind, c.Id, err)
	}
}

// groupTaskID 一次生成的多张图在素材库里用同一个 source_task_id 成组（取第一个任务 ID）。
func groupTaskID(c *model.AfvImageCreation) string {
	if t := parseTasks(c.Tasks.String); len(t) > 0 {
		return t[0].TaskID
	}
	return ""
}
