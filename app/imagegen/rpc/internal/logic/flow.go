package logic

import (
	"aigc-local-life/pkg/billing"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/logx"
)

func pagexOffset(page, size int64) (int64, int64) { return pagex.Offset(page, size) }

func newTaskID(svcCtx *svc.ServiceContext) string {
	return aigc.NewTaskID(svcCtx.Config.TaskIDPrefix)
}

// newRefTaskID 提交执行面用的 task_id，带上任务中心的编号（AiCreation_life_<src>_<id>），方便按它查执行面日志。
func newRefTaskID(svcCtx *svc.ServiceContext, src string, id int64, stage string) string {
	return aigc.NewLifeTaskID(svcCtx.Config.TaskIDPrefix, taskcenter.Ref(src, id), stage)
}

// refPlanTaskID 新建后把出剧本 / 解析的任务号换成带任务中心编号的（编号插入后才有）。
// 改失败就沿用插入时的随机号，不影响出结果。
func refPlanTaskID(ctx context.Context, svcCtx *svc.ServiceContext, row *model.AfvVideoCreation) {
	id := newRefTaskID(svcCtx, "vid", row.Id, "plan")
	if svcCtx.Video.SetPlanTaskID(ctx, row.Id, id) == nil {
		row.PlanTaskId = nullStr(id)
	}
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

// buildTask 按类型组装执行面入参：海报 78，长图 76，动作迁移 9，数字人口播 92。
//
// poster_type / poster_style 是喂给模型的，传中文展示名（如「节日」「扁平插画」）；
// 库里存的是编码，展示名在提交前查出来。
func buildTask(c *model.LocalLifeCreation, cfg *config.KindConf, mats []string, typeLabel string) aigc.Task {
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
	if c.Kind == model.KindActionTransfer {
		return aigc.MotionDrive{
			InputVideo: c.InputVideoUrl.String, ReferenceImage: c.InputImageUrl.String, Model: c.Model,
		}
	}
	if c.Kind == model.KindVoiceover {
		return aigc.Voiceover{
			Text: c.Content, CharURL: c.InputImageUrl.String, EnvURL: c.InputBgUrl.String, Duration: int(c.Duration),
			AspectRatio: c.Size, Resolution: cfg.Resolution, Model: c.Model,
		}
	}
	return aigc.LongImage{
		UserText: c.Content, TemplateURL: c.TemplateUrl.String, MaterialURLs: mats, Size: c.Size,
	}
}

// submit 一张图一个任务，逐个提交；提交失败的那张直接记失败，其余照常。
// attempt 为空是首次提交；重做时传一个新的标记，执行面任务号随之变化（同一个号执行面不会再跑一遍）。
func submit(ctx context.Context, svcCtx *svc.ServiceContext, c *model.LocalLifeCreation, cfg *config.KindConf, typeLabel, attempt string) []imageTask {
	mats := parseList(c.MaterialUrls)
	task := buildTask(c, cfg, mats, typeLabel)
	tasks := make([]imageTask, 0, c.ImageCount)
	for i := int64(0); i < c.ImageCount; i++ {
		stage := fmt.Sprintf("i%d", i+1)
		if attempt != "" {
			stage = attempt + "_" + stage
		}
		id := newRefTaskID(svcCtx, "img", c.Id, stage)
		if c.ImageCount == 1 && attempt == "" { // 只出一张：执行面任务就这一个，直接用任务中心编号
			id = aigc.ExactTaskID(svcCtx.Config.TaskIDPrefix, taskcenter.Ref("img", c.Id), false)
		}
		if _, err := svcCtx.Aigc.Submit(ctx, id, task); err != nil {
			aigc.LogSubmitFailed(ctx, imageTrace(c, id), fmt.Errorf("第 %d/%d 张: %w", i+1, c.ImageCount, err))
			tasks = append(tasks, imageTask{TaskID: id, Status: aigc.StatusFailed, Error: err.Error()})
			continue
		}
		tasks = append(tasks, imageTask{TaskID: id})
	}
	// TODO 任务：确认后写 afv_creation_task（海报 mode=78 / 长图 mode=76 / 动作迁移 9 / 数字人口播 92）
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
	p.pollVideos(ctx)                 // 探店视频 / 带货短剧（两步）
	defer svcCtx.TaskCenter.Sync(ctx) // 本轮的状态变化同步进任务中心
	rows, err := svcCtx.Creation.ListGenerating(ctx, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list generating: %v", err)
		return
	}
	for _, c := range rows {
		minutes := svcCtx.Config.Poll.TimeoutMinutes
		if cfg, err := kindConf(svcCtx, c.Kind); err == nil {
			minutes = cfg.Timeout(minutes)
		}
		p.pollOne(ctx, c, minutes)
	}
}

// pollOne 查这条记录名下还没结束的任务；全部结束（或超过 timeoutMinutes）就收尾。
func (p *Poller) pollOne(ctx context.Context, c *model.LocalLifeCreation, timeoutMinutes int) {
	svcCtx := p.svcCtx
	timedOut := time.Since(c.UpdateTime) > time.Duration(timeoutMinutes)*time.Minute
	tasks := parseTasks(c.Tasks.String)
	changed := false
	for i := range tasks {
		if tasks[i].Status != "" {
			continue
		}
		r := p.query(ctx, tasks[i].TaskID)
		trace := imageTrace(c, tasks[i].TaskID)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess && r.GeneratedImagePath != "":
			// 执行面产物在上游私有桶，转存到公有桶后再落库；转存失败下一轮重试
			u, terr := svcCtx.Storage.Transfer(ctx, r.GeneratedImagePath, c.Kind, c.UserId)
			if terr != nil {
				aigc.LogStepFailed(ctx, trace, "transfer", terr)
				continue
			}
			tasks[i].Status, tasks[i].URL, changed = aigc.StatusSuccess, u, true
		case r != nil && r.Done():
			tasks[i].Status, tasks[i].Error, changed = aigc.StatusFailed, r.ErrorMsg, true
			aigc.LogOutcome(ctx, trace, r)
			if r.Status != aigc.StatusFailed { // LogOutcome 只对 failed 打 Error；not_found / 成功无产物在这里补原因
				aigc.LogStepFailed(ctx, trace, "result", errors.New(outcomeCause(r)))
			}
		case timedOut:
			tasks[i].Status, tasks[i].Error, changed = aigc.StatusFailed, "生成超时", true
			last := "无查询结果"
			if r != nil {
				last = r.Describe()
			}
			aigc.LogStepFailed(ctx, trace, "timeout", fmt.Errorf("超过 %d 分钟仍未完成，最后一次查询：%s", timeoutMinutes, last))
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
	if pending > 0 { // 还有没出结果的，先把进展存下来；任务仍在途，续期预留，避免排队时被释放
		if err := svcCtx.Billing.Renew(ctx, c.UserId, c.RequestId.String); err != nil {
			logx.WithContext(ctx).Errorf("renew hold %d: %v", c.Id, err)
		}
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
		// 全部失败：每张图的执行面原因汇总成一条 task_failed，运维按记录号即可看到整单的失败经过
		aigc.LogTaskFailed(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "image", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("img", c.Id)}, reason, taskCauses(tasks))
	}
	// 先确认扣费，确认成功才交付。预留已过期释放时不能交付（否则用户白拿成品），改为失败。
	if status == model.StatusDone {
		if err := svcCtx.Billing.Settle(ctx, c.UserId, c.RequestId.String, len(urls)); err != nil {
			logx.WithContext(ctx).Errorf("settle %s %d: %v", c.Kind, c.Id, err)
			aigc.LogTaskFailed(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "image", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("img", c.Id)}, billing.DeliverBlockedReason, err)
			status, reason, urls = model.StatusFailed, billing.DeliverBlockedReason, nil
		}
	}
	ok, err := svcCtx.Creation.UpdateTasks(ctx, c.Id, dumpTasks(tasks), status, jsonList(urls), reason)
	if err != nil {
		// 确认已成功但写库失败：下一轮会再进来，Settle 幂等，不会重复扣费
		logx.WithContext(ctx).Errorf("finish %d: %v", c.Id, err)
		return
	}
	if !ok {
		return // 别处已经收过尾了（不重复结算）
	}
	if status == model.StatusDone {
		saveAssets(ctx, svcCtx, c, urls)
	} else {
		svcCtx.Billing.Fail(ctx, c.UserId, c.RequestId.String, reason)
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

// saveAssets 成品写进素材库：每张图一行，用第一个任务 ID 串成一组。
func saveAssets(ctx context.Context, svcCtx *svc.ServiceContext, c *model.LocalLifeCreation, urls []string) {
	if !svcCtx.Config.Asset.Enabled {
		return
	}
	cfg, err := kindConf(svcCtx, c.Kind)
	if err != nil {
		return
	}
	props, _ := json.Marshal(map[string]any{"creationId": c.Id, "kind": c.Kind, "posterType": c.PosterType.String})
	name := c.Content
	switch c.Kind {
	case model.KindActionTransfer:
		name = "动作迁移"
	case model.KindVoiceover:
		name = "数字人口播"
	}
	group := groupTaskID(c)
	list := make([]asset.Asset, 0, len(urls))
	for _, u := range urls {
		list = append(list, asset.Asset{
			UserID: c.UserId, SourceTaskID: group, Type: cfg.AssetType, EntryCode: cfg.AssetEntryCode,
			Name: name, CoverURL: coverOf(c, u), ResultURL: u, AspectRatio: c.Size, AIPrompt: c.Content,
			Properties: string(props),
		})
	}
	if err := svcCtx.Asset.InsertSet(ctx, list); err != nil {
		logx.WithContext(ctx).Errorf("save assets of %s %d: %v", c.Kind, c.Id, err)
	}
}

// groupTaskID 一次生成的多张图在素材库里用同一个 source_task_id 成组（取第一个任务 ID）。
func groupTaskID(c *model.LocalLifeCreation) string {
	if t := parseTasks(c.Tasks.String); len(t) > 0 {
		return t[0].TaskID
	}
	return ""
}

// coverOf 素材库封面：动作迁移、数字人口播的成品是视频，用人物图当封面；图片类成品用自身。
func coverOf(c *model.LocalLifeCreation, url string) string {
	if (c.Kind == model.KindActionTransfer || c.Kind == model.KindVoiceover) && c.InputImageUrl.Valid {
		return c.InputImageUrl.String
	}
	return url
}

// imageTrace 一张图的排障坐标：记录号、任务中心编号、执行面任务号。
func imageTrace(c *model.LocalLifeCreation, execTaskID string) aigc.Trace {
	return aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "image", RecordID: c.Id, UserID: c.UserId,
		TaskCenterID: taskcenter.Ref("img", c.Id), ExecTaskID: execTaskID}
}

// outcomeCause 执行面非成功终态的原因（只进日志，不进用户可见的文案）。
// not_found 写明执行面查不到该任务；成功却没有产物地址也单独说明，免得日志里只剩一句空的失败。
func outcomeCause(r *aigc.Result) string {
	switch r.Status {
	case aigc.StatusNotFound:
		return "执行面查不到该任务（提交可能未生效）；" + r.Describe()
	case aigc.StatusSuccess:
		return "执行面返回成功但没有产物地址；" + r.Describe()
	}
	return r.Describe()
}

// taskCauses 多张图里失败的那些：每张一行（执行面任务号 + 原因），给 task_failed 的 cause 用。
func taskCauses(tasks []imageTask) error {
	var lines []string
	for _, t := range tasks {
		if t.Status == aigc.StatusFailed {
			lines = append(lines, fmt.Sprintf("exec_task_id=%s error=%q", t.TaskID, t.Error))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return errors.New(strings.Join(lines, "; "))
}
