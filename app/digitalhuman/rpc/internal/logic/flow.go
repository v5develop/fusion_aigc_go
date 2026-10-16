package logic

import (
	"aigc-life/pkg/taskcenter"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

// 执行面支持的时长范围（秒）。
// 85（云端 Seedance 2.5）支持 4–30；lite 被路由到 45（本地 LTX），只到 25 秒，
// 超了执行面会自己截断——与其交出一段被砍掉半句话的视频，不如提交前就拦住。
const (
	minVideoSeconds     = 4
	maxVideoSeconds     = 30
	maxVideoSecondsLite = 25
)

// maxSecondsOf 该模型能出多长。
func maxSecondsOf(mdl string) int {
	if mdl == modelLite {
		return maxVideoSecondsLite
	}
	return maxVideoSeconds
}

func storageKey(svcCtx *svc.ServiceContext, kind string, userID int64, ext string) string {
	return storage.ObjectKey(svcCtx.Config.Storage.Prefix, kind, userID, ext)
}

// ---- AI 定制形象（执行面 75）----

// 提示词模板在选项表 PROMPT 类型里，运营可改；读不到时用内置文本。
const (
	promptPortrait         = "portrait"
	promptPortraitVariants = "portrait_variants"
)

var fallbackPrompts = map[string]string{
	promptPortrait: "真实摄影风格的人物半身像，竖版 9:16 构图。人物：{age}{gender}，{skin}肤色，职业形象为{role}。{description}" +
		"面向镜头、神情自然亲切，光线柔和均匀，背景简洁干净，画面清晰，适合作为数字人口播形象。",
	promptPortraitVariants: "穿着得体的职业装，站在明亮的门店里\n穿着休闲日常装，背景为浅色纯色墙面\n" +
		"穿着时尚简约的服装，背景为温馨的室内环境\n穿着清爽的浅色上衣，背景为户外自然光街景",
}

func prompt(ctx context.Context, svcCtx *svc.ServiceContext, code string) string {
	o, err := svcCtx.Option.FindOneByOptionTypeCodeDeletedId(ctx, model.OptionPrompt, code, 0)
	if err == nil && o.Status == 1 && strings.TrimSpace(o.Value.String) != "" {
		return o.Value.String
	}
	if err != nil && err != model.ErrNotFound {
		logx.WithContext(ctx).Errorf("load prompt %s: %v", code, err)
	}
	return fallbackPrompts[code]
}

// portraitLabels AI 定制表单选中的各项展示名，拼进提示词。
type portraitLabels struct {
	Gender, Age, Skin, Role, Description string
}

// buildPortraitPrompts 生成 n 条提示词：同一人设 + 各不相同的服装/背景，4 张图才有得选。
func buildPortraitPrompts(ctx context.Context, svcCtx *svc.ServiceContext, l portraitLabels, n int) []string {
	role := l.Role
	if role == "" {
		role = "本地生活商家"
	}
	desc := strings.TrimSpace(l.Description)
	if desc != "" && !strings.HasSuffix(desc, "。") {
		desc += "。"
	}
	base := strings.NewReplacer("{age}", l.Age, "{gender}", l.Gender, "{skin}", l.Skin,
		"{role}", role, "{description}", desc).Replace(prompt(ctx, svcCtx, promptPortrait))

	var variants []string
	for _, v := range strings.Split(prompt(ctx, svcCtx, promptPortraitVariants), "\n") {
		if v = strings.TrimSpace(v); v != "" {
			variants = append(variants, v)
		}
	}
	out := make([]string, n)
	for i := range out {
		out[i] = base
		if len(variants) > 0 {
			out[i] += variants[i%len(variants)] + "。"
		}
	}
	return out
}

// submitPortrait 提交出图任务；提交失败直接把记录标记为失败。
func submitPortrait(ctx context.Context, svcCtx *svc.ServiceContext, p *model.AfvDigitalHumanPortrait, prompts []string) {
	images := make([]aigc.NoteImage, 0, len(prompts))
	for _, s := range prompts {
		images = append(images, aigc.NoteImage{ImagePrompt: s, ReferenceMaterials: []int{}})
	}
	cfg := svcCtx.Config.Portrait
	_, err := svcCtx.Aigc.Submit(ctx, p.TaskId.String, aigc.NoteImages{Images: images, Model: cfg.Model, Size: cfg.Size})
	if err != nil {
		logx.WithContext(ctx).Errorf("submit portrait %d: %v", p.Id, err)
		finishPortrait(ctx, svcCtx, p.Id, model.PortraitFailed, nil, "提交生成失败，请重试")
		return
	}
	// AI 定制形象不收费（不在计费入口里）
	// TODO 任务：确认后写 afv_creation_task（mode=75，execution_task_id=task_id）
}

// finishPortrait 写终态；同 failHuman，ctx 可能已经超时，写库要用新的。
func finishPortrait(ctx context.Context, svcCtx *svc.ServiceContext, id, status int64, urls []string, reason string) {
	ctx, cancel := detach(ctx, 10*time.Second)
	defer cancel()
	var js string
	if len(urls) > 0 {
		b, _ := json.Marshal(urls)
		js = string(b)
	}
	if _, err := svcCtx.Portrait.Finish(ctx, id, status, js, reason); err != nil {
		logx.WithContext(ctx).Errorf("finish portrait %d: %v", id, err)
	}
}

// ---- 开始生成：配音 → 口播视频（执行面 85）----

// startDigitalHuman 后台跑配音（同步调百炼，几秒），成功后提交视频任务，之后由 Poller 跟进。
func startDigitalHuman(svcCtx *svc.ServiceContext, h *model.AfvDigitalHuman, voice string) {
	threading.GoSafe(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Duration(svcCtx.Config.TTS.TimeoutSeconds)*time.Second)
		defer cancel()
		log := logx.WithContext(ctx).WithFields(logx.Field("digital_human_id", h.Id))

		d, err := synthesize(ctx, svcCtx, h.UserId, h.Script, voice)
		if err != nil {
			log.Errorf("tts: %v", err)
			failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, "配音生成失败，请重试")
			return
		}
		limit := maxSecondsOf(h.Model)
		secs := int(math.Ceil(d.Duration.Seconds()))
		if secs > limit {
			failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, fmt.Sprintf("配音时长超过 %d 秒，请精简配音内容", limit))
			return
		}
		secs = max(secs, minVideoSeconds)

		// lite 走本地 LTX：执行面自己从 OSS 下载，给的必须是 key，不是公网 URL。
		imagePath, audioPath := h.ImageUrl, d.URL
		if h.Model == modelLite {
			if imagePath, err = svcCtx.Storage.ObjectKey(ctx, h.ImageUrl); err != nil {
				log.Errorf("lite 取形象图 key: %v", err)
				failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, "形象图不在可用的存储里，请重新上传或改用云端模型")
				return
			}
			if audioPath, err = svcCtx.Storage.ObjectKey(ctx, d.URL); err != nil {
				log.Errorf("lite 取配音 key: %v", err)
				failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, "配音存储异常，请重试")
				return
			}
		}

		taskID := aigc.ExactTaskID(taskcenter.Ref("dh", h.Id), h.VideoTaskId.Valid) // 数字人出片只有这一个执行面任务，和任务中心编号一致
		// 配音时长出来了，冻结点数；点数不够就到此为止，记录标失败
		if _, err := svcCtx.Billing.Hold(ctx, humanCharge(h, taskID, secs)); err != nil {
			_, msg, _ := xerr.Parse(err)
			failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, msg)
			return
		}
		ok, err := svcCtx.Human.StartRendering(ctx, h.Id, d.URL, int64(secs), taskID)
		if err != nil || !ok {
			log.Errorf("start rendering: ok=%v err=%v", ok, err)
			svcCtx.Billing.Fail(ctx, h.UserId, taskID, "未能开始生成")
			return
		}
		cfg := svcCtx.Config.Video
		_, err = svcCtx.Aigc.Submit(ctx, taskID, aigc.DigitalHuman{
			ImagePath: imagePath, AudioPath: audioPath, AspectRatio: cfg.AspectRatio, Resolution: cfg.Resolution,
			Duration: secs, Model: wireModel(h.Model),
		})
		if err != nil {
			log.Errorf("submit video: %v", err)
			failHuman(ctx, svcCtx, h.Id, model.DhStatusRendering, "提交生成失败，请重试")
			return
		}
		// TODO 任务：确认后写 afv_creation_task：配音一条 mode=0，视频一条 mode=85（parent_task_id 串起来）
	})
}

// failHuman 标记失败。
//
// 注意：调用方的 ctx 可能正是因为超时才走到这里（配音超时），用它写库会被直接拒，
// 记录就永远停在「配音中」。所以这里另起一个短超时的 ctx。
func failHuman(ctx context.Context, svcCtx *svc.ServiceContext, id, from int64, reason string) {
	ctx, cancel := detach(ctx, 10*time.Second)
	defer cancel()
	ok, err := svcCtx.Human.Finish(ctx, id, from, model.DhStatusFailed, "", reason)
	if err != nil {
		logx.WithContext(ctx).Errorf("mark failed %d: %v", id, err)
		return
	}
	// 出片阶段失败 / 超时：退回冻结的点数（配音阶段还没冻结）
	if ok && from == model.DhStatusRendering {
		if h, ferr := svcCtx.Human.FindOne(ctx, id); ferr == nil {
			svcCtx.Billing.Fail(ctx, h.UserId, h.VideoTaskId.String, reason)
		} else {
			logx.WithContext(ctx).Errorf("digital human %d: 找不到记录，无法退回点数: %v", id, ferr)
		}
	}
}

// ---- 轮询 ----

// Poller 定时查询执行面结果，并回收超时与卡住的记录。
//
// 状态都在库里：进程重启后接着查，不依赖内存。
//   - AI 定制形象：生成中 → 调 getRes → 完成/失败；超过 Portrait.TimeoutMinutes 判失败
//   - 数字人：视频生成中 → 调 getRes → 完成/失败；超过 Video.TimeoutMinutes 判失败
//   - 数字人：配音中超过 2 倍 TTS 超时（服务重启留下的）判失败
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
	logx.Info("digital human poller started")
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
	p.pollPortraits(ctx)
	p.pollVideos(ctx)
	p.recoverDubbing(ctx)
	p.svcCtx.TaskCenter.Sync(ctx) // 本轮的状态变化同步进任务中心
}

func (p *Poller) pollPortraits(ctx context.Context) {
	svcCtx := p.svcCtx
	list, err := svcCtx.Portrait.ListGenerating(ctx, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list portraits: %v", err)
		return
	}
	deadline := time.Duration(svcCtx.Config.Portrait.TimeoutMinutes) * time.Minute
	for _, it := range list {
		r := p.query(ctx, it.TaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess:
			if len(r.Outputs()) == 0 {
				finishPortrait(ctx, svcCtx, it.Id, model.PortraitFailed, nil, "生成结果为空，请重试")
				continue
			}
			urls := transferAll(ctx, svcCtx, r.Outputs(), "portrait", it.UserId) // 上游私有桶 → 公有桶
			if len(urls) == 0 {
				continue // 转存失败，保持生成中，下一轮重试
			}
			if ok, _ := svcCtx.Portrait.Finish(ctx, it.Id, model.PortraitDone, jsonArray(urls), ""); ok {
				savePortraitAsset(ctx, svcCtx, it, urls) // 设计稿：4 张图自动存进素材库
			}
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("portrait %d task %s %s: %s", it.Id, it.TaskId.String, r.Status, r.ErrorMsg)
			finishPortrait(ctx, svcCtx, it.Id, model.PortraitFailed, nil, "生成失败，请重试")
		case time.Since(it.CreateTime) > deadline:
			finishPortrait(ctx, svcCtx, it.Id, model.PortraitFailed, nil, "生成超时，请重试")
		}
	}
}

func (p *Poller) pollVideos(ctx context.Context) {
	svcCtx := p.svcCtx
	list, err := svcCtx.Human.ListByStatus(ctx, model.DhStatusRendering, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list rendering: %v", err)
		return
	}
	deadline := time.Duration(svcCtx.Config.Video.TimeoutMinutes) * time.Minute
	for _, it := range list {
		r := p.query(ctx, it.VideoTaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess && r.GeneratedImagePath != "":
			videoURL := transfer(ctx, svcCtx, r.GeneratedImagePath, "video", it.UserId) // 上游私有桶 → 公有桶
			if videoURL == "" {
				continue // 转存失败，保持生成中，下一轮重试
			}
			if ok, err := svcCtx.Human.Finish(ctx, it.Id, model.DhStatusRendering, model.DhStatusDone, videoURL, ""); err != nil {
				logx.WithContext(ctx).Errorf("finish %d: %v", it.Id, err)
			} else if ok {
				saveVideoAsset(ctx, svcCtx, it, videoURL)
				svcCtx.Billing.Done(ctx, it.UserId, it.VideoTaskId.String, 0)
			}
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("digital human %d task %s %s: %s", it.Id, it.VideoTaskId.String, r.Status, r.ErrorMsg)
			failHuman(ctx, svcCtx, it.Id, model.DhStatusRendering, "视频生成失败，请重试")
		case time.Since(it.UpdateTime) > deadline:
			failHuman(ctx, svcCtx, it.Id, model.DhStatusRendering, "视频生成超时，请重试")
		}
	}
}

// recoverDubbing 配音跑在进程内协程里，服务重启会留下「配音中」的记录。
func (p *Poller) recoverDubbing(ctx context.Context) {
	before := time.Now().Add(-6 * time.Duration(p.svcCtx.Config.TTS.TimeoutSeconds) * time.Second)
	list, err := p.svcCtx.Human.ListByStatus(ctx, model.DhStatusDubbing, before, pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list dubbing: %v", err)
		return
	}
	for _, it := range list {
		failHuman(ctx, p.svcCtx, it.Id, model.DhStatusDubbing, "配音超时，请重试")
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

// ---- 素材库 ----

func jsonArray(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// savePortraitAsset AI 定制形象：一行主资产 + 每张图一条子资产（同现有 customizedModel 的存法）。
func savePortraitAsset(ctx context.Context, svcCtx *svc.ServiceContext, p *model.AfvDigitalHumanPortrait, urls []string) {
	cfg := svcCtx.Config.Asset
	if !cfg.Enabled {
		return
	}
	items := make([]asset.Item, 0, len(urls))
	for i, u := range urls {
		items = append(items, asset.Item{
			ItemType: "variant", Name: fmt.Sprintf("形象 %d", i+1), MediaURL: u, MediaType: "image", SortOrder: int64(i),
		})
	}
	a := asset.Asset{
		UserID: p.UserId, SourceTaskID: p.TaskId.String, Type: cfg.Portrait.Type, EntryCode: cfg.Portrait.EntryCode,
		Name: "AI定制数字人形象", CoverURL: urls[0], ResultURL: urls[0], AspectRatio: svcCtx.Config.Portrait.Size,
		AIPrompt: p.Description.String,
	}
	if err := svcCtx.Asset.InsertWithItems(ctx, a, items); err != nil {
		logx.WithContext(ctx).Errorf("save portrait asset %d: %v", p.Id, err)
	}
}

// saveVideoAsset 口播视频：一行资产，封面用形象图。
func saveVideoAsset(ctx context.Context, svcCtx *svc.ServiceContext, h *model.AfvDigitalHuman, videoURL string) {
	cfg := svcCtx.Config.Asset
	if !cfg.Enabled {
		return
	}
	props, _ := json.Marshal(map[string]any{"digitalHumanId": h.Id, "duration": h.Duration.Int64, "voiceId": h.VoiceId})
	a := asset.Asset{
		UserID: h.UserId, SourceTaskID: h.VideoTaskId.String, Type: cfg.Video.Type, EntryCode: cfg.Video.EntryCode,
		Name: h.Name, CoverURL: h.ImageUrl, ResultURL: videoURL, AspectRatio: svcCtx.Config.Video.AspectRatio,
		AIPrompt: h.Script, Properties: string(props),
	}
	if err := svcCtx.Asset.InsertSet(ctx, []asset.Asset{a}); err != nil {
		logx.WithContext(ctx).Errorf("save video asset %d: %v", h.Id, err)
	}
}

// ---- 产物转存 ----
//
// 执行面的产物落在上游私有桶，不对外可读也不长期保留（现有系统同样要转存，
// 见 afv_creation_task.is_transferred）。这里统一转到公有桶后再落库。

func transfer(ctx context.Context, svcCtx *svc.ServiceContext, srcURL, kind string, userID int64) string {
	u, err := svcCtx.Storage.Transfer(ctx, srcURL, kind, userID)
	if err != nil {
		logx.WithContext(ctx).Errorf("transfer %s: %v", srcURL, err)
		return ""
	}
	return u
}

// transferAll 全部转存成功才返回，任一失败返回空（调用方按失败处理，下一轮重试）。
func transferAll(ctx context.Context, svcCtx *svc.ServiceContext, srcURLs []string, kind string, userID int64) []string {
	out := make([]string, 0, len(srcURLs))
	for _, src := range srcURLs {
		u := transfer(ctx, svcCtx, src, kind, userID)
		if u == "" {
			return nil
		}
		out = append(out, u)
	}
	return out
}

// detach ctx 已经结束时另起一个（只保留日志用的信息），保证终态一定写得进库。
func detach(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithTimeout(ctx, timeout)
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
