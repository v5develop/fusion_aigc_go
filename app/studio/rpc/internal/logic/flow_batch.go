package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"

	"github.com/zeromicro/go-zero/core/logx"
)

// batchPayload afv_studio_batch.payload（submit_common.go 三种 spec 的并集，键名一致）。
type batchPayload struct {
	// 商拍
	ModelImageURLs []string `json:"model_image_urls"`
	GarmentImages  []struct {
		URL   string `json:"url"`
		Role  string `json:"role"`
		Angle string `json:"angle"`
	} `json:"garment_images"`
	Items         []string          `json:"items"`
	SetMode       string            `json:"set_mode"`
	Lengths       map[string]string `json:"lengths"`
	Sleeves       map[string]string `json:"sleeves"`
	Fabric        string            `json:"fabric"`
	CountPerModel int               `json:"count_per_model"`
	// 姿态
	SourceImageURL string            `json:"source_image_url"`
	PoseImageURLs  []string          `json:"pose_image_urls"`
	GlobalPrompt   string            `json:"global_prompt"`
	PosePrompts    map[string]string `json:"pose_prompts"`
	// 多人配对（PreviewPose 确认后提交；没传时为 0 / nil）
	SourcePersonCount int                `json:"source_person_count"`
	PoseMappings      []PoseMappingInput `json:"mappings"`
	// 图片生成
	ImageURLs     []string `json:"image_urls"`
	Count         int      `json:"count"`
	RewritePrompt bool     `json:"rewrite_prompt"`
	// 公共
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	Resolution  string `json:"resolution"`
	AspectRatio string `json:"aspect_ratio"`
}

func parsePayload(raw json.RawMessage) (*batchPayload, error) {
	var p batchPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("批次 payload 无法解析: %w", err)
	}
	return &p, nil
}

// stepError 编排失败：User 是给用户看的中文原因，Retryable 表示下一轮可以再来（受 orchestrateMaxFailures 约束）。
type stepError struct {
	Step      string
	User      string
	Retryable bool
	Err       error
}

func (e *stepError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *stepError) Unwrap() error { return e.Err }

// classify 外部错误 → stepError。
func classify(step string, err error) error {
	if err == nil {
		return nil
	}
	var se *stepError
	if errors.As(err, &se) || errors.Is(err, errAborted) || errors.Is(err, context.Canceled) {
		return err
	}
	e := &stepError{Step: step, Err: err}
	var re *reverse.Error
	var me *mediasvc.Error
	switch {
	case reverse.IsNotConfigured(err):
		e.User = "反推服务未配置，点数已退回"
	case reverse.IsRejected(err):
		msg := ""
		if errors.As(err, &re) {
			msg = strings.TrimSpace(re.Message)
		}
		if t, ok := noPersonText(err); ok {
			msg = t
		}
		if msg == "" {
			msg = "图片无法识别"
		}
		e.User = msg + "，点数已退回"
	case reverse.IsRetryable(err):
		e.User, e.Retryable = "反推服务暂不可用，点数已退回", true
	case errors.As(err, &me):
		e.User, e.Retryable = "图片下载或处理失败，点数已退回", !me.Permanent()
	case errors.Is(err, context.DeadlineExceeded):
		e.User, e.Retryable = "处理超时，点数已退回", true
	default:
		e.User = "处理失败，点数已退回"
	}
	return e
}

// orchestrate 推进一个批次到 生成中。可重入：每次从头重放，下载 / 裁图走媒体服务幂等键，反推结果走批次缓存。
func (f *flow) orchestrate(ctx context.Context, batchID int64) {
	log := logx.WithContext(ctx)
	b, err := f.repo.Batch.GetByID(ctx, batchID, 0)
	if err != nil || b == nil {
		return
	}
	if b.Status != model.BatchQueued && b.Status != model.BatchCropping && b.Status != model.BatchPrompting {
		return
	}
	// 用户在编排前已把子任务全部取消
	counts, err := f.repo.Task.CountByBatch(ctx, b.ID)
	if err != nil {
		log.Errorf("studio: 批次 %d 统计子任务失败: %v", b.ID, err)
		return
	}
	if counts[model.TaskQueued] == 0 && counts[model.TaskRunning] == 0 {
		f.converge(ctx, b.ID)
		return
	}
	r := &batchRun{f: f, b: b}
	err = r.run(ctx)
	switch {
	case err == nil:
	case errors.Is(err, errAborted):
		f.converge(ctx, b.ID)
	case errors.Is(err, context.Canceled):
	default:
		var se *stepError
		if !errors.As(err, &se) {
			se = classify("orchestrate", err).(*stepError)
		}
		retries := stepLogRetries(parseStepLogs(b.StepLog))
		if se.Retryable && retries < orchestrateMaxFailures {
			log.Infof("studio: 批次 %d 编排可重试失败（第 %d 次），下一轮再来: %v", b.ID, retries+1, err)
			f.stepLog(ctx, b.ID, model.StepLog{Step: stepRetry, Detail: se.Step + ": " + se.User})
			return
		}
		log.Errorf("studio: 批次 %d 编排失败，整批退款: %v", b.ID, err)
		f.failBatch(ctx, b.ID, se.User)
	}
}

// failBatch 整批失败：排队的子任务逐个 CAS 排队→失败并退款，批次写原因后收敛（全部失败）。
// 用逐个 CAS 而不是 Task.FailQueued：保证每个子任务只退一次款、只计一次数。
func (f *flow) failBatch(ctx context.Context, batchID int64, reason string) {
	log := logx.WithContext(ctx)
	b, err := f.repo.Batch.GetByID(ctx, batchID, 0)
	if err != nil || b == nil {
		return
	}
	tasks, err := f.repo.Task.ListByBatch(ctx, b.ID)
	if err != nil {
		log.Errorf("studio: 批次 %d 列子任务失败: %v", b.ID, err)
		return
	}
	n := 0
	for _, t := range tasks {
		if t.Status != model.TaskQueued {
			continue
		}
		extra := map[string]any{"error_reason": reason}
		refund := refundable(f.bill.Mode(), t)
		if refund {
			extra["billing_status"] = model.BillingRefunded
		}
		ok, err := f.repo.Task.TransitStatus(ctx, t.ID, model.TaskQueued, model.TaskFailed, extra)
		if err != nil || !ok {
			continue
		}
		n++
		if refund {
			f.refundTask(ctx, b.UserID, t.BillingRequestID.String, reason)
		}
	}
	if err := f.repo.Batch.UpdateCounters(ctx, b.ID, 0, n); err != nil {
		log.Errorf("studio: 批次 %d 更新计数失败: %v", b.ID, err)
	}
	if err := f.repo.Batch.SetError(ctx, b.ID, reason); err != nil {
		log.Errorf("studio: 批次 %d 写失败原因失败: %v", b.ID, err)
	}
	f.stepLog(ctx, b.ID, model.StepLog{Step: stepFailed, TaskTotal: n, Detail: reason})
	f.converge(ctx, b.ID)
}

// ---------------------------------------------------------------- 一次编排

type batchRun struct {
	f     *flow
	b     *model.Batch
	p     *batchPayload
	group string
}

func (r *batchRun) jobID() string { return strconv.FormatInt(r.b.ID, 10) }

func (r *batchRun) run(ctx context.Context) error {
	p, err := parsePayload(r.b.Payload)
	if err != nil {
		return &stepError{Step: "payload", User: "请求数据异常，点数已退回", Err: err}
	}
	r.p = p
	if r.f.media == nil && r.b.Kind != model.KindImageGen {
		return &stepError{Step: "media", User: "媒体服务未配置，点数已退回", Err: errors.New("media 未配置"), Retryable: true}
	}
	switch r.b.Kind {
	case model.KindTryon:
		return r.tryon(ctx)
	case model.KindPose:
		return r.pose(ctx)
	case model.KindImageGen:
		return r.imagegen(ctx)
	}
	return &stepError{Step: "kind", User: "未知的任务类型，点数已退回", Err: fmt.Errorf("kind %q", r.b.Kind)}
}

// stage CAS 推进批次阶段（只前进），写一条 step_log（step=阶段名）。没抢到（被取消 / 已收敛）返回 errAborted。
func (r *batchRun) stage(ctx context.Context, to int8) error {
	if r.b.Status >= to {
		return nil
	}
	ok, err := r.f.repo.Batch.UpdateStatus(ctx, r.b.ID, r.b.Status, to, nil)
	if err != nil {
		return err
	}
	if !ok {
		return errAborted
	}
	r.b.Status = to
	r.f.stepLog(ctx, r.b.ID, model.StepLog{Step: batchStatusName(to)})
	return nil
}

// plan 顺序执行步骤，每步一行日志 + step_log。
func (r *batchRun) plan(ctx context.Context, steps ...pipeline.Step) error {
	_, err := pipeline.Plan{JobID: r.jobID(), Steps: steps, OnLog: func(lg pipeline.StepLog) {
		r.f.logStep(ctx, r.b.ID, lg)
	}}.Run(ctx)
	return err
}

func step(name string, fn func(ctx context.Context) (pipeline.StepLog, error)) pipeline.Step {
	return pipeline.StepFunc{StepName: name, Fn: func(ctx context.Context) (pipeline.StepLog, error) {
		lg, err := fn(ctx)
		return lg, classify(name, err)
	}}
}

// prepare 下载 → 规整（长边 ≤maxLong、JPEG）→ 取字节算 SHA1、拼 data URL。maxLong=0 不规整（图片生成参考图只入库）。
func (r *batchRun) prepare(ctx context.Context, im *pipeline.Image, maxLong int, needData bool) error {
	return prepareImage(ctx, r.f.media, im, maxLong, needData)
}

// prepareImage 下载（媒体服务 ingest）→ 按长边压缩 → 需要时取回字节拼 data URL。编排与姿态配对预览共用。
func prepareImage(ctx context.Context, media pipeline.Media, im *pipeline.Image, maxLong int, needData bool) error {
	res, err := media.IngestURL(ctx, im.URL, mediasvc.PurposeUserInput, "")
	if err != nil {
		return err
	}
	if maxLong > 0 {
		if res, err = media.Conform(ctx, res.RID, mediasvc.ConformParams{LongEdgeMax: maxLong, Format: "jpeg"}, ""); err != nil {
			return err
		}
	}
	im.RID, im.Width, im.Height, im.SHA256 = res.RID, res.Width, res.Height, res.SHA256
	if !needData {
		return nil
	}
	data, mime, err := media.FetchBytes(ctx, res.RID)
	if err != nil {
		return err
	}
	im.SHA1 = pipeline.SHA1Hex(data)
	if im.SHA256 == "" {
		im.SHA256 = sha256Hex(data)
	}
	im.Data = reverse.DataURL(mime, data)
	return nil
}

// prepareAll 并发（≤4）prepare 一组图，任一失败返回第一个错误。
func (r *batchRun) prepareAll(ctx context.Context, list []*pipeline.Image, maxLong func(*pipeline.Image) int, needData bool) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, 4)
	)
	for _, im := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(im *pipeline.Image) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := r.prepare(ctx, im, maxLong(im), needData); err != nil {
				mu.Lock()
				if first == nil {
					first = fmt.Errorf("%s: %w", im.Key, err)
				}
				mu.Unlock()
			}
		}(im)
	}
	wg.Wait()
	return first
}

func ptrs(list []pipeline.Image) []*pipeline.Image {
	out := make([]*pipeline.Image, len(list))
	for i := range list {
		out[i] = &list[i]
	}
	return out
}

// cacheGet 读批次缓存到 v；没有返回 false。
func (r *batchRun) cacheGet(ctx context.Context, key, kind string, v any) (bool, error) {
	raw, ok, err := r.f.repo.Cache.Get(ctx, r.b.ID, key, kind)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, nil // 坏缓存当没有
	}
	return true, nil
}

func (r *batchRun) cachePut(ctx context.Context, key, kind string, v any) {
	raw, err := json.Marshal(v)
	if err == nil {
		err = r.f.repo.Cache.Put(ctx, r.b.ID, key, kind, raw)
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 批次 %d 写缓存 %s/%s 失败: %v", r.b.ID, kind, key, err)
	}
}

func (r *batchRun) modelGroup() (string, error) {
	if r.group != "" {
		return r.group, nil
	}
	g, err := pipeline.MapModel(r.p.Model, r.f.cfg.ModelGroupMap())
	if err != nil {
		return "", &stepError{Step: "model", User: "不支持的模型，点数已退回", Err: err}
	}
	r.group = g
	return g, nil
}

func (r *batchRun) labels(t *model.Task) map[string]string {
	return map[string]string{"batch": strconv.FormatInt(r.b.ID, 10), "task": t.TaskID.String, "kind": string(r.b.Kind)}
}

// setPrompts 给每个未终结的子任务写最终 prompt_text（= 发给 gateway 的全文，含首行图序）、input_images、prompt_meta。
// build 返回本任务的请求与实际送生图的图（截断后）；返回 skip=true 的任务不写。
func (r *batchRun) setPrompts(ctx context.Context, build func(t *model.Task) (prompt string, inputs []model.InputImage, meta map[string]any, err error)) error {
	tasks, err := r.f.repo.Task.ListByBatch(ctx, r.b.ID)
	if err != nil {
		return err
	}
	for _, t := range tasks {
		if model.TaskFinal(t.Status) {
			continue
		}
		text, inputs, meta, err := build(t)
		if err != nil {
			return err
		}
		var in, mt json.RawMessage
		if len(inputs) > 0 {
			in, _ = json.Marshal(inputs)
		}
		if len(meta) > 0 {
			mt, _ = json.Marshal(meta)
		}
		if err := r.f.repo.Task.SetPrompt(ctx, t.ID, text, mt, in); err != nil {
			return err
		}
	}
	return nil
}

// promptOf 从构造好的请求里取 prompt 与实际张数（被截断时以请求为准）。
func promptOf(req any) (string, int) {
	switch in := req.(type) {
	case pipeline.ImageEditInput:
		return in.Prompt, len(in.Images)
	case pipeline.ImageGenerateInput:
		return in.Prompt, 0
	}
	return "", 0
}

func inputImage(im pipeline.Image, role string) model.InputImage {
	return model.InputImage{ID: im.Key, URL: "mr://" + im.RID, Role: role, Sha1: im.SHA1, Sha256: im.SHA256}
}

// mediaRefs 送 gateway 的参考图：一律 mr://{rid} 并带 sha256（不传 https：网关要白名单 + 有效签名）。
func mediaRefs(list []pipeline.Image) []aigateway.MediaRef {
	out := make([]aigateway.MediaRef, len(list))
	for i, im := range list {
		out[i] = aigateway.MediaRef{URI: "mr://" + im.RID, Sha256: im.SHA256}
	}
	return out
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// genParams 子任务实际下发的生图参数，存进 prompt_meta.gen：重提 / 重启时按它重建，保证请求体与首次一致。
type genParams struct {
	AspectRatio string `json:"aspect_ratio,omitempty"`
	Resolution  string `json:"resolution,omitempty"`
	Quality     string `json:"quality,omitempty"`
}

// genOptions 本批的生图选项：质量按组与种类取，参考图上限优先 catalog。
func (r *batchRun) genOptions(ctx context.Context, group string) pipeline.GenOptions {
	return pipeline.GenOptions{
		Quality:      pipeline.QualityFor(group, string(r.b.Kind), ""),
		MaxRefImages: r.f.catalogMaxRefs(ctx, group),
	}
}

func roleOf(im pipeline.Image) string {
	switch im.Kind {
	case pipeline.ImageModel:
		return "model"
	case pipeline.ImagePose:
		return "pose"
	case pipeline.ImageRef:
		return "ref"
	}
	return "garment"
}

// ---------------------------------------------------------------- 商拍

func (r *batchRun) tryon(ctx context.Context) error {
	p := r.p
	garments := make([]pipeline.GarmentInput, 0, len(p.GarmentImages))
	for _, g := range p.GarmentImages {
		garments = append(garments, pipeline.GarmentInput{URL: g.URL, Role: g.Role, Angle: g.Angle})
	}
	gs, numWarns := pipeline.NumberGarments(p.Items, garments)
	ms := pipeline.NumberModels(p.ModelImageURLs)
	if len(gs) == 0 || len(ms) == 0 {
		return &stepError{Step: pipeline.StepIngest, User: "没有可用的服装图或模特图，点数已退回", Err: errors.New("empty images")}
	}
	spec := pipeline.TryonSpec{Items: p.Items, SetMode: p.SetMode, Fabric: p.Fabric, Notes: p.Prompt, Lengths: p.Lengths, Sleeves: p.Sleeves}
	cat := pipeline.InferCategory(p.Items)
	if err := r.stage(ctx, model.BatchCropping); err != nil {
		return err
	}
	var (
		plan    reverse.CropPlanResp
		details []pipeline.Image
		sel     pipeline.Selection
	)
	err := r.plan(ctx,
		step(pipeline.StepIngest, func(ctx context.Context) (pipeline.StepLog, error) {
			all := append(ptrs(gs), ptrs(ms)...)
			err := r.prepareAll(ctx, all, func(*pipeline.Image) int { return 1600 }, true)
			return pipeline.StepLog{Images: len(all), Warnings: numWarns}, err
		}),
		step(pipeline.StepCropPlan, func(ctx context.Context) (pipeline.StepLog, error) {
			lg := pipeline.StepLog{Images: len(gs)}
			if ok, err := r.cacheGet(ctx, pipeline.CropPlanKey(0), model.CacheCropPlan, &plan); err != nil || ok {
				return lg, err
			}
			req := reverse.CropPlanReq{Category: cat, Items: p.Items, Fabric: p.Fabric, Notes: p.Prompt}
			for _, g := range gs {
				req.Images = append(req.Images, reverse.CropPlanImage{Data: g.Data, Angle: g.Angle, Role: g.Role})
			}
			cctx, cancel := context.WithTimeout(ctx, r.f.cropTimeout())
			defer cancel()
			resp, err := r.f.rev.CropPlan(cctx, req)
			if err != nil {
				return lg, err
			}
			plan = *resp
			r.cachePut(ctx, pipeline.CropPlanKey(0), model.CacheCropPlan, plan)
			if raw, err := json.Marshal(plan); err == nil {
				_ = r.f.repo.Batch.SetCropPlan(ctx, r.b.ID, raw)
			}
			return lg, nil
		}),
		step(pipeline.StepCrop, func(ctx context.Context) (pipeline.StepLog, error) {
			jobs, warns := pipeline.BuildCropJobs(gs, plan.Plans, p.Items)
			lg := pipeline.StepLog{Images: len(jobs)}
			for _, j := range jobs {
				res, err := r.f.media.Crop(ctx, j.Source.RID, j.Boxes, pipeline.CropParams, "")
				if err != nil { // 单张主图裁不出来不阻断：少几张细节
					lg.Degraded = true
					warns = append(warns, fmt.Sprintf("%s 裁图失败：%v", j.Source.Key, err))
					continue
				}
				details = append(details, pipeline.DetailImages(j, res)...)
			}
			kept := details[:0]
			for _, d := range details {
				data, mime, err := r.f.media.FetchBytes(ctx, d.RID)
				if err != nil {
					lg.Degraded = true
					warns = append(warns, fmt.Sprintf("%s 取切图失败：%v", d.Key, err))
					continue
				}
				d.SHA1, d.Data = pipeline.SHA1Hex(data), reverse.DataURL(mime, data)
				if d.SHA256 == "" {
					d.SHA256 = sha256Hex(data)
				}
				kept = append(kept, d)
			}
			details = kept
			lg.Crops, lg.Warnings = len(details), warns
			return lg, nil
		}),
		step(pipeline.StepCropCheck, func(ctx context.Context) (pipeline.StepLog, error) {
			lg := pipeline.StepLog{Images: len(details)}
			if len(details) == 0 {
				return lg, nil
			}
			items := make([]reverse.CropCheckItem, len(details))
			for i, d := range details {
				items[i] = reverse.CropCheckItem{Data: d.Data, Label: d.Label}
			}
			cctx, cancel := context.WithTimeout(ctx, r.f.cropTimeout())
			defer cancel()
			chk, cerr := r.f.rev.CropCheck(cctx, items) // 失败不阻断
			var warns []string
			details, warns = pipeline.FilterCropChecks(details, chk, cerr)
			rec := map[string]any{"results": chk, "kept": len(details)}
			if cerr != nil {
				rec["error"] = cerr.Error()
				lg.Degraded = true
			}
			if raw, err := json.Marshal(rec); err == nil {
				_ = r.f.repo.Batch.SetCropCheck(ctx, r.b.ID, raw)
			}
			lg.Crops, lg.Warnings = len(details), warns
			return lg, nil
		}),
		step(pipeline.StepSelect, func(ctx context.Context) (pipeline.StepLog, error) {
			sel = pipeline.SelectImages(cat, gs, details, &ms[0])
			out := make([]model.SelectedImages, 0, len(ms))
			for i, m := range ms {
				s := withModel(sel, m)
				imgs := make([]model.InputImage, 0, len(s.Images))
				for _, im := range pipeline.GenerateOrder(s) {
					imgs = append(imgs, inputImage(im, roleOf(im)))
				}
				out = append(out, model.SelectedImages{ModelIndex: i, Images: imgs})
			}
			if raw, err := json.Marshal(out); err == nil {
				_ = r.f.repo.Batch.SetSelected(ctx, r.b.ID, raw)
			}
			return pipeline.StepLog{Images: len(sel.Images), Crops: sel.Details}, nil
		}),
	)
	if err != nil {
		return err
	}
	if err := r.stage(ctx, model.BatchPrompting); err != nil {
		return err
	}
	prompts := map[string]*analyzeCache{} // 模特 sha1 → 反推结果（带当时用的提示词版本）
	var pv, pt string                     // 本批的提示词版本（首次决定后落批次缓存，重放沿用）
	err = r.plan(ctx, step(pipeline.StepAnalyze, func(ctx context.Context) (pipeline.StepLog, error) {
		lg := pipeline.StepLog{Images: len(sel.Images)}
		var err error
		if pv, pt, err = r.tryonPromptVersion(ctx); err != nil {
			return lg, err
		}
		var todo []pipeline.Image
		seen := map[string]bool{}
		for _, m := range ms {
			key := pipeline.AnalyzeKey(m.SHA1)
			if seen[key] {
				continue
			}
			seen[key] = true
			var cached analyzeCache
			ok, err := r.cacheGet(ctx, key, model.CacheAnalyze, &cached)
			if err != nil {
				return lg, err
			}
			if ok && cached.Prompt != "" {
				if !cached.Versioned { // 旧缓存没记版本：按本批版本标注（尽力而为）
					cached.PromptVersion, cached.TargetModel = pv, pt
				}
				prompts[key] = &cached
				continue
			}
			todo = append(todo, m)
		}
		if len(todo) == 0 {
			return lg, nil
		}
		actx, cancel := context.WithTimeout(ctx, r.f.analyzeTimeout())
		defer cancel()
		breq := pipeline.BuildTryonAnalyzeBatchReq(spec, sel, todo, r.f.cfg.Reverse.Concurrency)
		breq.AnalyzeReq = pipeline.WithPromptVersion(breq.AnalyzeReq, pv, pt)
		resp, err := r.f.rev.AnalyzeBatch(actx, breq)
		if err != nil {
			return lg, err
		}
		byID := map[string]pipeline.Image{}
		for _, m := range todo {
			byID[m.Key] = m
		}
		done := map[string]bool{}
		for _, res := range resp.Results {
			m, ok := byID[res.ModelID]
			if !ok {
				continue
			}
			ar := &reverse.AnalyzeResp{Prompt: res.Prompt, Meta: res.Meta, Warning: res.Warning}
			if res.Error != "" || strings.TrimSpace(res.Prompt) == "" { // 单个模特失败：单独重试一次
				one, err := r.f.rev.Analyze(actx, pipeline.WithPromptVersion(pipeline.BuildTryonAnalyzeReq(spec, withModel(sel, m)), pv, pt))
				if err != nil {
					return lg, err
				}
				ar = one
				lg.Warnings = append(lg.Warnings, fmt.Sprintf("%s analyze-batch 失败，已单独重试", m.Key))
			}
			if ar.Meta.Degraded {
				lg.Degraded = true
			}
			key := pipeline.AnalyzeKey(m.SHA1)
			c := &analyzeCache{AnalyzeResp: *ar, PromptVersion: pv, TargetModel: pt, Versioned: true}
			prompts[key] = c
			done[m.Key] = true
			r.cachePut(ctx, key, model.CacheAnalyze, c)
		}
		for _, m := range todo {
			if !done[m.Key] {
				return lg, &stepError{Step: pipeline.StepAnalyze, User: "反推结果缺失，点数已退回", Retryable: true,
					Err: fmt.Errorf("analyze-batch 没有返回 %s", m.Key)}
			}
		}
		return lg, nil
	}))
	if err != nil {
		return err
	}
	group, err := r.modelGroup()
	if err != nil {
		return err
	}
	err = r.setPrompts(ctx, func(t *model.Task) (string, []model.InputImage, map[string]any, error) {
		if int(t.ModelIndex) >= len(ms) {
			return "", nil, nil, fmt.Errorf("子任务 %d 模特下标越界", t.ID)
		}
		m := ms[t.ModelIndex]
		ar := prompts[pipeline.AnalyzeKey(m.SHA1)]
		order := pipeline.GenerateOrder(withModel(sel, m))
		g := orDefault(t.ModelGroup, group)
		opt := r.genOptions(ctx, g)
		gp := genParams{AspectRatio: orDefault(p.AspectRatio, pipeline.NearestAspectRatio(m.Width, m.Height)), Resolution: p.Resolution, Quality: opt.Quality}
		req, warns, err := pipeline.BuildImageEditRequestRefs(pipeline.KindTryon, g, ar.Prompt, mediaRefs(order),
			gp.Resolution, gp.AspectRatio, r.labels(t), opt)
		if err != nil {
			return "", nil, nil, &stepError{Step: pipeline.StepGenerate, User: "生图请求构造失败，点数已退回", Err: err}
		}
		text, n := promptOf(req.Input)
		inputs := make([]model.InputImage, 0, n)
		for _, im := range order[:n] {
			inputs = append(inputs, inputImage(im, roleOf(im)))
		}
		meta := map[string]any{"model_key": m.Key, "degraded": ar.Meta.Degraded, "reverse_model": ar.Meta.Model, "gen": gp}
		if ar.PromptVersion != "" { // 灰度命中：后台按 prompt_version 对比新旧版效果（取该反推结果实际用的版本）
			meta["prompt_version"], meta["target_model"] = ar.PromptVersion, ar.TargetModel
		}
		if ar.Warning != "" || len(warns) > 0 {
			meta["warnings"] = append(warns, ar.Warning)
		}
		return text, inputs, meta, nil
	})
	if err != nil {
		return err
	}
	return r.stage(ctx, model.BatchGenerating)
}

// analyzeCache 商拍反推缓存体：sidecar 返回 + 生成它时用的提示词版本。旧缓存没有 versioned（版本未知）。
type analyzeCache struct {
	reverse.AnalyzeResp
	PromptVersion string `json:"prompt_version,omitempty"`
	TargetModel   string `json:"target_model,omitempty"`
	Versioned     bool   `json:"versioned,omitempty"`
}

// promptVersionKey 本批提示词版本在批次缓存里的键（kind=analyze；其余 analyze 键是 40 位 sha1，不会撞）。
const promptVersionKey = "prompt-version"

// promptVersionRec 本批提示词版本的落库记录。Version 为空 = 稳定版。
type promptVersionRec struct {
	Version string `json:"version"`
	Target  string `json:"target"`
	Decided bool   `json:"decided"`
}

// tryonPromptVersion 本批商拍反推的提示词版本：批次缓存里有就沿用；没有时按当前灰度比例算一次并先落库再用，
// 运营中途改比例不会让同一批次重放时换版本（旧版本的缓存结果也不会被贴上新版本标签）。落库失败按可重试处理（下一轮再来）。
// 模型组映射失败时返回稳定版（不落库，后面 modelGroup 会报错）。
func (r *batchRun) tryonPromptVersion(ctx context.Context) (string, string, error) {
	var rec promptVersionRec
	ok, err := r.cacheGet(ctx, promptVersionKey, model.CacheAnalyze, &rec)
	if err != nil {
		return "", "", err
	}
	if ok && rec.Decided {
		return rec.Version, rec.Target, nil
	}
	g, gerr := r.modelGroup()
	if gerr != nil {
		return "", "", nil
	}
	rec.Version, rec.Target = pipeline.TryonPromptVersion(g, r.b.ID, r.f.tryonNewPromptPercent(ctx))
	rec.Decided = true
	raw, _ := json.Marshal(rec)
	if err := r.f.repo.Cache.Put(ctx, r.b.ID, promptVersionKey, model.CacheAnalyze, raw); err != nil {
		return "", "", &stepError{Step: pipeline.StepAnalyze, User: "处理失败，点数已退回", Retryable: true, Err: fmt.Errorf("记录提示词版本: %w", err)}
	}
	return rec.Version, rec.Target, nil
}

// withModel 组图结果里的模特换成 m（各模特共用同一组服装图）。
func withModel(sel pipeline.Selection, m pipeline.Image) pipeline.Selection {
	out := sel
	out.Images = append([]pipeline.Image(nil), sel.Images...)
	if out.Model && len(out.Images) > 0 {
		out.Images[len(out.Images)-1] = m
	} else {
		out.Images = append(out.Images, m)
		out.Model = true
	}
	return out
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return def
}

// ---------------------------------------------------------------- 姿态

// poseSourceCache source 缓存体：sidecar 原样 source（回传给 pose/prompt）与 meta。
type poseSourceCache struct {
	Source json.RawMessage  `json:"source"`
	Meta   reverse.PoseMeta `json:"meta"`
}

// previewSourceTTL 配对预览读到的原图结果可被编排复用的时长（从预览时算）。超时后编排重新读图，只按人数校验配对。
const previewSourceTTL = 2 * time.Hour

// previewSourceCache 配对预览存的原图读图结果：同 poseSourceCache，另带写入时间（unix 秒）判断是否过期。
type previewSourceCache struct {
	Source   json.RawMessage  `json:"source"`
	Meta     reverse.PoseMeta `json:"meta"`
	CachedAt int64            `json:"cached_at"`
}

// previewSourceBatchID 预览结果在 afv_studio_prompt_cache 里的 batch_id：用户号取负。真实批次号是自增正数，
// 两边不会撞键；唯一键 (batch_id, cache_key=原图 sha1, kind=source) 天然按 (用户, 原图) 去重，同图再预览覆盖为最新一次。
func previewSourceBatchID(userID int64) int64 { return -userID }

// previewSource 取本用户对这张原图（sha1）最近一次配对预览的读图结果：没有、坏数据、过期，或人数与提交时确认的
// source_person_count 不一致时返回 nil（编排照常重新读图）。读库失败同样当没有，只记日志。
func (r *batchRun) previewSource(ctx context.Context, sha1 string) *reverse.PoseSource {
	if r.b.UserID <= 0 || sha1 == "" {
		return nil
	}
	raw, ok, err := r.f.repo.Cache.Get(ctx, previewSourceBatchID(r.b.UserID), pipeline.SourceKey(sha1), model.CacheSource)
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 批次 %d 读配对预览原图结果失败，改为重新读图: %v", r.b.ID, err)
		return nil
	}
	var c previewSourceCache
	if !ok || json.Unmarshal(raw, &c) != nil || len(c.Source) == 0 || c.CachedAt <= 0 {
		return nil
	}
	if age := r.f.now().Sub(time.Unix(c.CachedAt, 0)); age < 0 || age > previewSourceTTL {
		return nil
	}
	s := &reverse.PoseSource{Raw: c.Source, Meta: c.Meta}
	if json.Unmarshal(c.Source, &s.Source) != nil {
		return nil
	}
	if r.p.SourcePersonCount > 0 && s.Source.PeopleCount() != r.p.SourcePersonCount {
		return nil
	}
	return s
}

func (r *batchRun) pose(ctx context.Context) error {
	p := r.p
	src := pipeline.Image{Key: "m-1", Kind: pipeline.ImageModel, Role: pipeline.RoleModel, URL: strings.TrimSpace(p.SourceImageURL)}
	poses := make([]pipeline.Image, len(p.PoseImageURLs))
	for i, u := range p.PoseImageURLs {
		poses[i] = pipeline.Image{Key: fmt.Sprintf("p-%d", i+1), Kind: pipeline.ImagePose, Source: i, URL: strings.TrimSpace(u)}
	}
	if err := r.stage(ctx, model.BatchPrompting); err != nil { // 姿态没有切图
		return err
	}
	var source *reverse.PoseSource
	results := make([]*reverse.PosePrompt, len(poses))
	mappingWarns := make([][]string, len(poses)) // 每张姿势图的配对告警（写进子任务 meta）
	err := r.plan(ctx,
		step(pipeline.StepIngest, func(ctx context.Context) (pipeline.StepLog, error) {
			all := append([]*pipeline.Image{&src}, ptrs(poses)...)
			err := r.prepareAll(ctx, all, func(im *pipeline.Image) int { // 原图 1600、姿势 1400（s1.1）
				if im.Kind == pipeline.ImagePose {
					return 1400
				}
				return 1600
			}, true)
			if err == nil {
				if e := r.f.repo.Batch.SetSourceSha1(ctx, r.b.ID, src.SHA1); e != nil {
					logx.WithContext(ctx).Errorf("studio: 批次 %d 写 source_sha1 失败: %v", r.b.ID, e)
				}
			}
			return pipeline.StepLog{Images: len(all)}, err
		}),
		step(pipeline.StepSource, func(ctx context.Context) (pipeline.StepLog, error) {
			lg := pipeline.StepLog{Images: 1}
			key := pipeline.SourceKey(src.SHA1)
			var c poseSourceCache
			if ok, err := r.cacheGet(ctx, key, model.CacheSource, &c); err != nil {
				return lg, err
			} else if ok && len(c.Source) > 0 {
				source = &reverse.PoseSource{Raw: c.Source, Meta: c.Meta}
				_ = json.Unmarshal(c.Source, &source.Source)
				return lg, nil
			}
			// 用户做过配对预览：直接用预览读到的那份（人物左→右顺序与用户确认配对时一致），不再让 LLM 重读一遍
			if s := r.previewSource(ctx, src.SHA1); s != nil {
				source = s
				r.cachePut(ctx, key, model.CacheSource, poseSourceCache{Source: s.Raw, Meta: s.Meta})
				logx.WithContext(ctx).Infof("studio: 批次 %d 复用配对预览的原图读图结果（%d 人）", r.b.ID, s.Source.PeopleCount())
				return lg, nil
			}
			actx, cancel := context.WithTimeout(ctx, r.f.analyzeTimeout())
			defer cancel()
			s, err := r.f.rev.PoseSource(actx, src.Data)
			if err != nil {
				return lg, err // 422 无人物 / 未成年 → classify 用 sidecar 文案，整批失败退款
			}
			source = s
			raw := s.Raw
			if len(raw) == 0 {
				raw, _ = json.Marshal(s.Source)
			}
			r.cachePut(ctx, key, model.CacheSource, poseSourceCache{Source: raw, Meta: s.Meta})
			return lg, nil
		}),
		step(pipeline.StepPrompt, func(ctx context.Context) (pipeline.StepLog, error) {
			lg := pipeline.StepLog{Images: len(poses)}
			actx, cancel := context.WithTimeout(ctx, r.f.analyzeTimeout())
			defer cancel()
			var (
				wg    sync.WaitGroup
				mu    sync.Mutex
				first error
			)
			for i := range poses {
				notes, mapping, want, key, warn := p.posePromptPlan(i, poses[i].SHA1, source.Source.PeopleCount())
				if warn != "" {
					mappingWarns[i] = append(mappingWarns[i], warn)
					mu.Lock() // 前面已起的 goroutine 也在写 lg.Warnings
					lg.Warnings = append(lg.Warnings, fmt.Sprintf("%s %s", poses[i].Key, warn))
					mu.Unlock()
				}
				var c posePromptCache
				if ok, err := r.cacheGet(ctx, key, model.CachePrompt, &c); err != nil {
					return lg, err
				} else if ok && c.Prompt != "" {
					results[i] = &c.PosePrompt
					// 重放也要把配对告警带进子任务 meta
					if w := c.mappingWarn(want, mapping); w != "" {
						mappingWarns[i] = append(mappingWarns[i], w)
						mu.Lock()
						lg.Warnings = append(lg.Warnings, fmt.Sprintf("%s %s", poses[i].Key, w))
						mu.Unlock()
					}
					continue
				}
				wg.Add(1)
				go func(i int, key, notes string, mapping []int, want *PoseMappingInput) { // sidecar 客户端自带并发上限（Reverse.Concurrency）
					defer wg.Done()
					pp, w, err := fetchPosePrompt(actx, r.f.rev, source, poses[i].Data, notes, mapping, want)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						if first == nil {
							first = fmt.Errorf("%s: %w", poses[i].Key, err)
						}
						return
					}
					if w != "" {
						mappingWarns[i] = append(mappingWarns[i], w)
						lg.Warnings = append(lg.Warnings, fmt.Sprintf("%s %s", poses[i].Key, w))
					}
					results[i] = pp
					r.cachePut(ctx, key, model.CachePrompt, posePromptCache{PosePrompt: *pp, MappingChecked: true, MappingWarn: w})
				}(i, key, notes, mapping, want)
			}
			wg.Wait()
			return lg, first
		}),
	)
	if err != nil {
		return err
	}
	group, err := r.modelGroup()
	if err != nil {
		return err
	}
	err = r.setPrompts(ctx, func(t *model.Task) (string, []model.InputImage, map[string]any, error) {
		i := int(t.PoseIndex)
		if i >= len(poses) || results[i] == nil {
			return "", nil, nil, fmt.Errorf("子任务 %d 姿势下标越界", t.ID)
		}
		g := orDefault(t.ModelGroup, group)
		opt := r.genOptions(ctx, g)
		gp := genParams{AspectRatio: orDefault(p.AspectRatio, pipeline.NearestAspectRatio(src.Width, src.Height)), Resolution: p.Resolution, Quality: opt.Quality}
		// G4：图 1 = 原图，图 2 = 姿势
		req, warns, err := pipeline.BuildImageEditRequestRefs(pipeline.KindPose, g, results[i].Prompt, mediaRefs([]pipeline.Image{src, poses[i]}),
			gp.Resolution, gp.AspectRatio, r.labels(t), opt)
		if err != nil {
			return "", nil, nil, &stepError{Step: pipeline.StepGenerate, User: "生图请求构造失败，点数已退回", Err: err}
		}
		text, _ := promptOf(req.Input)
		inputs := []model.InputImage{inputImage(src, "source"), inputImage(poses[i], "pose")}
		meta := map[string]any{"pose_key": poses[i].Key, "orientation_deg": results[i].Pose.OrientationDeg,
			"face_visible": results[i].Pose.FaceVisible, "reverse_model": results[i].Meta.Model, "gen": gp}
		if results[i].Mapping != nil { // 多人：实际使用的配对与两边人数（后台排查「谁学了谁」）
			meta["mapping"] = results[i].Mapping
			meta["source_people"] = source.Source.PeopleCount()
			meta["pose_people"] = results[i].Pose.PeopleCount()
		}
		warns = append(warns, mappingWarns[i]...)
		if len(warns) > 0 {
			meta["warnings"] = warns
		}
		return text, inputs, meta, nil
	})
	if err != nil {
		return err
	}
	return r.stage(ctx, model.BatchGenerating)
}

// mappingFor 第 i 张姿势图用户确认的配对；没有返回 nil。
func (p *batchPayload) mappingFor(i int) *PoseMappingInput {
	for k := range p.PoseMappings {
		if p.PoseMappings[k].PoseIndex == i {
			return &p.PoseMappings[k]
		}
	}
	return nil
}

// posePromptPlan 第 i 张姿势图的 pose/prompt 入参：补充说明、发给 sidecar 的配对（可能被放弃，见 resolvePoseMapping）、
// 用户确认的原配对、批次内缓存键（配对进键：反转配对不会命中默认配对的缓存）与告警。
func (p *batchPayload) posePromptPlan(i int, poseSHA1 string, srcPeople int) (notes string, mapping []int, want *PoseMappingInput, key, warn string) {
	notes = pipeline.BuildPoseNotes(p.GlobalPrompt, p.PosePrompts[strconv.Itoa(i)])
	want = p.mappingFor(i)
	mapping, warn = resolvePoseMapping(want, p.SourcePersonCount, srcPeople)
	return notes, mapping, want, pipeline.PromptKey(poseSHA1, notes, mapping), warn
}

// posePromptCache 姿态提示词缓存体：sidecar 返回 + 当时的配对结论（重放时原样带回子任务 meta）。
// 旧缓存没有 mapping_checked，命中时按结果用 mappingDrift 重算。
type posePromptCache struct {
	reverse.PosePrompt
	MappingChecked bool   `json:"mapping_checked,omitempty"`
	MappingWarn    string `json:"mapping_warn,omitempty"`
}

// mappingWarn 缓存命中时的配对告警：新缓存存了当时的结论原样返回，旧缓存（没有 mapping_checked）按结果重算。
func (c *posePromptCache) mappingWarn(want *PoseMappingInput, sent []int) string {
	if c.MappingChecked {
		return c.MappingWarn
	}
	return mappingDrift(want, sent, &c.PosePrompt)
}

// fetchPosePrompt 调 sidecar pose/prompt（带配对），再按用户确认时的姿势图人数核对：
//   - 本次读出的姿势图人数与预览不同：用户的下标已指不到同一批人，丢掉这次结果、不带配对重读一次（sidecar 默认配对）并告警，
//     与原图人数不一致时的处理（resolvePoseMapping）一致；
//   - 人数一致但 sidecar 回的配对与提交的不同（sidecar 判定配对无效、整组退回默认）：按 sidecar 实际配对生成并告警。
func fetchPosePrompt(ctx context.Context, rev pipeline.Reverse, source *reverse.PoseSource, data, notes string, mapping []int, want *PoseMappingInput) (*reverse.PosePrompt, string, error) {
	pp, err := rev.PosePrompt(ctx, source, data, notes, mapping)
	if err != nil {
		return nil, "", err
	}
	if want == nil || len(mapping) == 0 {
		return pp, "", nil
	}
	if n := pp.Pose.PeopleCount(); n != want.PosePersonCount {
		def, err := rev.PosePrompt(ctx, source, data, notes, nil)
		if err != nil {
			return nil, "", err
		}
		return def, fmt.Sprintf("姿势图人数与预览不一致（预览 %d 人、本次识别 %d 人），已按默认配对生成（实际配对 %v）",
			want.PosePersonCount, n, def.Mapping), nil
	}
	if !equalInts(mapping, pp.Mapping) {
		return pp, correctedWarn(mapping, pp.Mapping), nil
	}
	return pp, "", nil
}

// resolvePoseMapping 决定发给 sidecar 的 mapping。用户的配对是按预览时的人数确认的（左→右下标），
// 本次 sidecar 读出的原图人数不同时，这组下标已经指不到同一批人，宁可退回默认配对并告警，也不按错位的下标生成。
// want 为 nil（没确认配对）或原图单人时返回 nil（sidecar 默认配对 / 单人不需要配对）。
func resolvePoseMapping(want *PoseMappingInput, previewSrc, actualSrc int) ([]int, string) {
	if want == nil || len(want.Mapping) == 0 {
		return nil, ""
	}
	if actualSrc < 2 || previewSrc != actualSrc || len(want.Mapping) != actualSrc {
		return nil, fmt.Sprintf("原图人数与预览不一致（预览 %d 人、本次识别 %d 人），已按默认配对生成", previewSrc, actualSrc)
	}
	return append([]int(nil), want.Mapping...), ""
}

// mappingDrift 只给旧缓存（没有 mapping_checked）重放用：按缓存的结果判断当时是否偏离了用户的配对。
func mappingDrift(want *PoseMappingInput, sent []int, pp *reverse.PosePrompt) string {
	if want == nil || len(sent) == 0 || pp == nil {
		return ""
	}
	if n := pp.Pose.PeopleCount(); n != want.PosePersonCount {
		return fmt.Sprintf("姿势图人数与预览不一致（预览 %d 人、本次识别 %d 人），实际配对 %v", want.PosePersonCount, n, pp.Mapping)
	}
	if !equalInts(sent, pp.Mapping) {
		return correctedWarn(sent, pp.Mapping)
	}
	return ""
}

// correctedWarn sidecar 判定提交的配对无效（越界 / 重复 / 全 -1，整组退回默认配对）时的告警。
// sidecar 不再改写有效配对里的 -1（保持原姿势），所以这句只在真被改写时出现。
func correctedWarn(sent, got []int) string {
	return fmt.Sprintf("配对被反推服务校正：提交 %v，实际 %v", sent, got)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- 图片生成

func (r *batchRun) imagegen(ctx context.Context) error {
	p := r.p
	refs := make([]pipeline.Image, len(p.ImageURLs))
	for i, u := range p.ImageURLs {
		refs[i] = pipeline.Image{Key: fmt.Sprintf("r-%d", i+1), Kind: pipeline.ImageRef, Source: i, URL: strings.TrimSpace(u)}
	}
	if err := r.stage(ctx, model.BatchPrompting); err != nil {
		return err
	}
	prompt := p.Prompt
	steps := []pipeline.Step{}
	if len(refs) > 0 {
		if r.f.media == nil {
			return &stepError{Step: "media", User: "媒体服务未配置，点数已退回", Err: errors.New("media 未配置"), Retryable: true}
		}
		steps = append(steps, step(pipeline.StepIngest, func(ctx context.Context) (pipeline.StepLog, error) {
			err := r.prepareAll(ctx, ptrs(refs), func(*pipeline.Image) int { return 0 }, false)
			return pipeline.StepLog{Images: len(refs)}, err
		}))
	}
	if p.RewritePrompt {
		steps = append(steps, step(stepRewrite, func(ctx context.Context) (pipeline.StepLog, error) {
			lg := pipeline.StepLog{Images: len(refs)}
			var c struct {
				Prompt string `json:"prompt"`
			}
			if ok, err := r.cacheGet(ctx, "rewrite", model.CachePrompt, &c); err != nil {
				return lg, err
			} else if ok && c.Prompt != "" {
				prompt = c.Prompt
				return lg, nil
			}
			out, cost, err := r.f.rewritePrompt(ctx, r.b.ID, p.Prompt, refs)
			lg.CostCNY = cost
			if err != nil { // 润色失败不阻断：按原提示词继续
				lg.Degraded = true
				lg.Warnings = []string{"润色失败，按原提示词生成：" + err.Error()}
				return lg, nil
			}
			prompt = out
			r.cachePut(ctx, "rewrite", model.CachePrompt, map[string]string{"prompt": out})
			return lg, nil
		}))
	}
	if err := r.plan(ctx, steps...); err != nil {
		return err
	}
	group, err := r.modelGroup()
	if err != nil {
		return err
	}
	err = r.setPrompts(ctx, func(t *model.Task) (string, []model.InputImage, map[string]any, error) {
		g := orDefault(t.ModelGroup, group)
		opt := r.genOptions(ctx, g)
		gp := genParams{AspectRatio: p.AspectRatio, Resolution: p.Resolution, Quality: opt.Quality}
		req, warns, err := pipeline.BuildImageGenRequestRefs(g, prompt, mediaRefs(refs), gp.Resolution, gp.AspectRatio, r.labels(t), opt)
		if err != nil {
			return "", nil, nil, &stepError{Step: pipeline.StepGenerate, User: "生图请求构造失败，点数已退回", Err: err}
		}
		text, n := promptOf(req.Input)
		var inputs []model.InputImage
		for _, im := range refs[:n] {
			inputs = append(inputs, inputImage(im, "ref"))
		}
		meta := map[string]any{"rewritten": prompt != p.Prompt, "gen": gp}
		if len(warns) > 0 {
			meta["warnings"] = warns
		}
		return text, inputs, meta, nil
	})
	if err != nil {
		return err
	}
	return r.stage(ctx, model.BatchGenerating)
}
