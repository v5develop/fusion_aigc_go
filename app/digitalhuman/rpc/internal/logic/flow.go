package logic

import (
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

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

// 执行面 85 支持的时长范围（秒）
const (
	minVideoSeconds = 4
	maxVideoSeconds = 30
)

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
	// TODO 计费：等计费方案确认后，在这里预扣 100 点，失败时退回（同 AI文案）
	// TODO 任务：确认后写 afv_creation_task（mode=75，execution_task_id=task_id）
}

func finishPortrait(ctx context.Context, svcCtx *svc.ServiceContext, id, status int64, urls []string, reason string) {
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
		secs := int(math.Ceil(d.Duration.Seconds()))
		if secs > maxVideoSeconds {
			failHuman(ctx, svcCtx, h.Id, model.DhStatusDubbing, fmt.Sprintf("配音时长超过 %d 秒，请精简配音内容", maxVideoSeconds))
			return
		}
		secs = max(secs, minVideoSeconds)

		taskID := newTaskID(svcCtx)
		ok, err := svcCtx.Human.StartRendering(ctx, h.Id, d.URL, int64(secs), taskID)
		if err != nil || !ok {
			log.Errorf("start rendering: ok=%v err=%v", ok, err)
			return
		}
		cfg := svcCtx.Config.Video
		_, err = svcCtx.Aigc.Submit(ctx, taskID, aigc.DigitalHuman{
			ImagePath: h.ImageUrl, AudioPath: d.URL, AspectRatio: cfg.AspectRatio, Resolution: cfg.Resolution, Duration: secs,
		})
		if err != nil {
			log.Errorf("submit video: %v", err)
			failHuman(ctx, svcCtx, h.Id, model.DhStatusRendering, "提交生成失败，请重试")
			return
		}
		// TODO 任务：确认后写 afv_creation_task：配音一条 mode=0，视频一条 mode=85（parent_task_id 串起来）
	})
}

func failHuman(ctx context.Context, svcCtx *svc.ServiceContext, id, from int64, reason string) {
	if _, err := svcCtx.Human.Finish(ctx, id, from, model.DhStatusFailed, "", reason); err != nil {
		logx.WithContext(ctx).Errorf("mark failed %d: %v", id, err)
	}
	// TODO 计费：失败退回
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
			urls := r.Outputs()
			if len(urls) == 0 {
				finishPortrait(ctx, svcCtx, it.Id, model.PortraitFailed, nil, "生成结果为空，请重试")
			} else if ok, _ := svcCtx.Portrait.Finish(ctx, it.Id, model.PortraitDone, jsonArray(urls), ""); ok {
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
			if ok, err := svcCtx.Human.Finish(ctx, it.Id, model.DhStatusRendering, model.DhStatusDone, r.GeneratedImagePath, ""); err != nil {
				logx.WithContext(ctx).Errorf("finish %d: %v", it.Id, err)
			} else if ok {
				saveVideoAsset(ctx, svcCtx, it, r.GeneratedImagePath)
			}
			// TODO 计费：成功结算
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
