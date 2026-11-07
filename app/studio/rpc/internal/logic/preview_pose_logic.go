package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type PreviewPoseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPreviewPoseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PreviewPoseLogic {
	return &PreviewPoseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 姿态多人配对预览（只读、不计费）：读原图与每张姿势图的人物（左→右），回人数与默认配对，
// 供用户在付费提交前确认 / 反转配对；结果里的人数原样带回 SubmitPose 的 source_person_count / mappings[].pose_person_count。
// 原图读图结果按 (用户, 原图 sha1) 存一份（见 previewSourceTTL），编排时直接复用，保证生成用的人物顺序就是用户确认的那份。
func (l *PreviewPoseLogic) PreviewPose(in *studio.PreviewPoseReq) (*studio.PreviewPoseResp, error) {
	s := l.svcCtx
	pv := &posePreviewer{
		owns:    newDeps(s).owns,
		timeout: time.Duration(s.Config.Timeouts.Analyze) * time.Second,
		slots:   s.PreviewSlots,
	}
	if s.MediaSvc != nil { // 别把 nil 指针装进接口
		pv.media = s.MediaSvc
	}
	if s.Reverse != nil && s.Reverse.Enabled() {
		pv.rev = s.Reverse
	}
	if s.Repo != nil && s.Repo.Cache != nil {
		pv.cache = s.Repo.Cache
	}
	return pv.preview(l.ctx, in)
}

// previewInflight 每个用户同一时间只跑一个预览（读图不计费，防止连点把 sidecar 打满）。进程内去重，多实例各自一份。
var previewInflight sync.Map

// 变量而非常量只为单测缩短。
var (
	// previewSafety 预览整体留给 RPC 返回的余量：内部截止时间 = 请求截止时间 − 它，到点还没读完的姿势图记「识别超时」，
	// 已读完的照常返回（不让服务端方法超时把整份结果吞掉）。
	previewSafety = 10 * time.Second
	// previewMinCall 剩余时间不够一次读图就不再发，直接记超时（发了也读不完，白占 sidecar）。
	previewMinCall = 15 * time.Second
	// previewSaveTimeout 存原图读图结果的写库时限。
	previewSaveTimeout = 5 * time.Second
)

// previewCachePutter 预览把原图读图结果写进批次缓存表（repo.CacheRepo 满足）。
type previewCachePutter interface {
	Put(ctx context.Context, batchID int64, key, kind string, payload json.RawMessage) error
}

// posePreviewer 预览的依赖，单测直接构造。
type posePreviewer struct {
	media   pipeline.Media
	rev     pipeline.Reverse
	owns    func(url string) bool
	timeout time.Duration // 每次 sidecar 读图的时限上限（Timeouts.Analyze），实际还受请求剩余时间约束
	// slots 全进程预览读图名额（svc.PreviewSlots）：每次 sidecar 调用先占一个，最多占用 cap(slots) 个共享名额。
	// nil 只在单测里出现（不限）
	slots chan struct{}
	cache previewCachePutter // nil = 不存原图读图结果（编排会重新读图）
	now   func() time.Time
}

// preview 下载压缩（与编排同尺寸：原图 1600、姿势 1400）→ pose/source 读原图 → 姿势图按顺序逐张走 pose/prompt
// （与编排同一条读图路径，人数口径一致），取 pose.people；原图 ≥2 人时再取 sidecar 默认配对；提示词丢弃。
// 原图单人也读姿势图（只为回人数，提示多出的人物会被忽略），mapping_enabled=false、default_mapping 为空。
// 单张姿势图失败（含时间不够的「识别超时」）只写在该项 error 里；原图失败（无人物 / 未成年 / 服务不可用 / 超时）整个预览报错。
func (p *posePreviewer) preview(ctx context.Context, in *studio.PreviewPoseReq) (*studio.PreviewPoseResp, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	if err := checkURL("人物图", in.SourceImageUrl); err != nil {
		return nil, err
	}
	if len(in.PoseImageUrls) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "请上传姿势图")
	}
	if len(in.PoseImageUrls) > MaxPoses {
		return nil, xerr.Param(fmt.Sprintf("姿势最多 %d 张", MaxPoses))
	}
	if err := checkURLs("姿势图", in.PoseImageUrls); err != nil {
		return nil, err
	}
	checks := append([]urlCheck{{"人物图", strings.TrimSpace(in.SourceImageUrl)}}, urlChecks("姿势图", trimAll(in.PoseImageUrls))...)
	for _, u := range checks {
		if p.owns == nil || !p.owns(u.url) {
			return nil, xerr.Param(u.field + " 不是本服务上传的地址")
		}
	}
	if p.media == nil || p.rev == nil {
		return nil, xerr.New(xerr.NotReady, "人物配对预览暂未开放，请稍后再试")
	}
	if _, busy := previewInflight.LoadOrStore(in.UserId, struct{}{}); busy {
		return nil, xerr.New(xerr.Conflict, "上一次人物配对预览还在进行中，请稍候")
	}
	defer previewInflight.Delete(in.UserId)

	// 整个预览的内部截止时间：请求截止时间（RPC 方法超时 / 客户端 deadline）减去余量
	bctx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var bcancel context.CancelFunc
		bctx, bcancel = context.WithDeadline(ctx, dl.Add(-previewSafety))
		defer bcancel()
	}

	src := pipeline.Image{Key: "m-1", Kind: pipeline.ImageModel, Role: pipeline.RoleModel, URL: checks[0].url}
	if err := prepareImage(bctx, p.media, &src, 1600, true); err != nil {
		return nil, previewErr("人物图", err)
	}
	var source *reverse.PoseSource
	err := p.withSlot(bctx, func(cctx context.Context) error {
		var e error
		source, e = p.rev.PoseSource(cctx, src.Data)
		return e
	})
	if err != nil {
		return nil, previewErr("人物图", err)
	}
	p.saveSource(ctx, in.UserId, src.SHA1, source)
	out := &studio.PreviewPoseResp{Source: sourcePreview(source.Source), Poses: []*studio.PosePreviewItem{}}
	// 原图单人不需要配对（mapping_enabled=false），但姿势图照样逐张读：回每张的人数，
	// 前端据此提示「姿势图里多出的人物会被忽略」。default_mapping 只在需要配对时回。
	out.MappingEnabled = source.Source.PeopleCount() >= 2

	items := make([]*studio.PosePreviewItem, len(in.PoseImageUrls))
	next := make(chan int, len(items))
	for i := range items {
		items[i] = &studio.PosePreviewItem{PoseIndex: int32(i)}
		next <- i // 按姿势序号顺序处理：时间不够时超时的是靠后的几张
	}
	close(next)
	workers := len(items)
	if p.slots != nil {
		workers = min(workers, cap(p.slots))
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				p.previewPose(bctx, source, items[i], strings.TrimSpace(in.PoseImageUrls[i]))
			}
		}()
	}
	wg.Wait()
	out.Poses = items
	return out, nil
}

// previewPose 读一张姿势图写进 it；失败只写 it.Error。
func (p *posePreviewer) previewPose(ctx context.Context, source *reverse.PoseSource, it *studio.PosePreviewItem, url string) {
	if ctx.Err() != nil {
		it.Error = errText(previewErr("姿势图", context.DeadlineExceeded))
		return
	}
	im := pipeline.Image{Key: fmt.Sprintf("p-%d", it.PoseIndex+1), Kind: pipeline.ImagePose, URL: url}
	if err := prepareImage(ctx, p.media, &im, 1400, true); err != nil {
		it.Error = errText(previewErr("姿势图", err))
		return
	}
	var pp *reverse.PosePrompt
	err := p.withSlot(ctx, func(cctx context.Context) error {
		var e error
		pp, e = p.rev.PosePrompt(cctx, source, im.Data, "", nil)
		return e
	})
	if err != nil {
		it.Error = errText(previewErr("姿势图", err))
		return
	}
	it.Pose = posePreview(pp.Pose)
	if source.Source.PeopleCount() >= 2 {
		it.DefaultMapping = toInt32s(pp.Mapping)
	}
}

// withSlot 占一个预览名额后调 sidecar：单次时限从占到名额时起算，取 timeout 与内部截止时间剩余的较小者；
// 剩余时间不够 previewMinCall 时不发，按超时处理。等名额也受内部截止时间约束。
func (p *posePreviewer) withSlot(ctx context.Context, call func(ctx context.Context) error) error {
	if p.slots != nil {
		select {
		case p.slots <- struct{}{}:
			defer func() { <-p.slots }()
		case <-ctx.Done():
			return context.DeadlineExceeded
		}
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < previewMinCall {
		return context.DeadlineExceeded
	}
	cctx, cancel := context.WithTimeout(ctx, p.timeoutOr())
	defer cancel()
	return call(cctx)
}

// saveSource 原图读图结果按 (用户, 原图 sha1) 存进 afv_studio_prompt_cache（batch_id = -用户号，见 previewSourceBatchID），
// 编排 StepSource 在 previewSourceTTL 内直接复用。写失败只记日志（编排会重新读图并按人数校验配对）。
func (p *posePreviewer) saveSource(ctx context.Context, userID int64, sha1 string, s *reverse.PoseSource) {
	if p.cache == nil || s == nil || sha1 == "" {
		return
	}
	raw := s.Raw
	if len(raw) == 0 {
		raw, _ = json.Marshal(s.Source)
	}
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	body, err := json.Marshal(previewSourceCache{Source: raw, Meta: s.Meta, CachedAt: now().Unix()})
	if err == nil {
		// 不随请求取消（读图已经花了钱，结果要留给编排），但单独限时，别让写库拖住预览返回
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), previewSaveTimeout)
		err = p.cache.Put(wctx, previewSourceBatchID(userID), pipeline.SourceKey(sha1), model.CacheSource, body)
		cancel()
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 用户 %d 存配对预览原图读图结果失败: %v", userID, err)
	}
}

func (p *posePreviewer) timeoutOr() time.Duration {
	if p.timeout > 0 {
		return p.timeout
	}
	return pipeline.AnalyzeTimeout
}

// previewErr 预览错误 → 给用户的中文原因（不退款口径，区别于编排的 classify）。
func previewErr(what string, err error) error {
	var ce *xerr.CodeError
	if errors.As(err, &ce) {
		return err
	}
	var re *reverse.Error
	switch {
	case reverse.IsNotConfigured(err):
		return xerr.New(xerr.NotReady, "反推服务未配置，暂时不能预览配对")
	case reverse.IsRejected(err):
		if t, ok := noPersonText(err); ok {
			return xerr.Param(t)
		}
		msg := ""
		if errors.As(err, &re) {
			msg = strings.TrimSpace(re.Message)
		}
		if msg == "" {
			msg = what + "无法识别"
		}
		return xerr.Param(msg)
	case errors.Is(err, context.DeadlineExceeded):
		return xerr.New(xerr.ThirdPartyError, what+"识别超时，请稍后重试")
	case reverse.IsRetryable(err):
		return xerr.New(xerr.ThirdPartyError, "反推服务暂不可用，请稍后重试")
	}
	return xerr.New(xerr.ThirdPartyError, what+"下载或识别失败，请稍后重试")
}

func sourcePreview(d reverse.PoseSourceData) *studio.PosePreviewImage {
	out := &studio.PosePreviewImage{PersonCount: int32(d.PeopleCount()), Group: d.Group, Interaction: d.Interaction, PeopleTruncated: d.PeopleTruncated}
	for _, pp := range d.People {
		out.People = append(out.People, &studio.PosePreviewPerson{
			Label: pp.Label, Position: pp.Position, Primary: pp.Primary, GenderAge: pp.GenderAge,
			Summary: strings.Join(pp.Outfit, "；"),
		})
	}
	if len(out.People) == 0 { // 旧版 sidecar 没有 people：按单人补一位
		out.People = []*studio.PosePreviewPerson{{Label: "P1", Position: "画面中间", Primary: true, GenderAge: d.GenderAge, Summary: strings.Join(d.Outfit, "；")}}
	}
	return out
}

func posePreview(d reverse.PoseData) *studio.PosePreviewImage {
	out := &studio.PosePreviewImage{PersonCount: int32(d.PeopleCount()), Group: d.Group, Interaction: d.Interaction, PeopleTruncated: d.PeopleTruncated}
	for _, pp := range d.People {
		out.People = append(out.People, &studio.PosePreviewPerson{Label: pp.Label, Position: pp.Position, Primary: pp.Primary, Summary: pp.Outline})
	}
	if len(out.People) == 0 {
		out.People = []*studio.PosePreviewPerson{{Label: "P1", Position: "画面中间", Primary: true, Summary: d.Outline}}
	}
	return out
}

func toInt32s(v []int) []int32 {
	if v == nil {
		return nil
	}
	out := make([]int32, len(v))
	for i, x := range v {
		out[i] = int32(x)
	}
	return out
}

func trimAll(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

// sidecar 「没有识别到人物」（422）给用户的固定文案。sidecar 原文带后缀（如「…请换一张有模特的照片」），统一成这两句。
const (
	noPersonSource = "原图中没有识别到人物，请换一张"
	noPersonPose   = "姿势参考图中没有识别到人物，请换一张"
)

// noPersonMarks sidecar 无人物报错的识别片段（pose_source / pose_prompt 的 ValueError 文案；英文兜底兼容旧镜像）。
var noPersonMarks = []string{"没有识别到人物", "未识别到人物", "没有检测到人物", "未检测到人物", "no person"}

// noPersonText sidecar 拒收（4xx）且原因是「没有识别到人物」时，按是哪张图返回固定文案。
// 判哪张图：文案里写了「原图」或来自 pose/source 的算原图，否则（pose/prompt）算姿势参考图。
func noPersonText(err error) (string, bool) {
	var re *reverse.Error
	if !errors.As(err, &re) || re.Kind != reverse.KindInvalid {
		return "", false
	}
	msg := strings.ToLower(re.Message)
	hit := false
	for _, m := range noPersonMarks {
		if strings.Contains(msg, m) {
			hit = true
			break
		}
	}
	if !hit {
		return "", false
	}
	if strings.Contains(re.Message, "原图") || (re.Path == reverse.PathPoseSource && !strings.Contains(re.Message, "姿势")) {
		return noPersonSource, true
	}
	return noPersonPose, true
}
