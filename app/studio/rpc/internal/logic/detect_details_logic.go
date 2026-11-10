package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type DetectDetailsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDetectDetailsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DetectDetailsLogic {
	return &DetectDetailsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DetectDetails 商拍「AI 找细节」（验收 F10 / F33，免费、不建任务、不落库）：服装图下载压缩（与编排同为长边 1600）→ sidecar crop-plan
// → 按编排同一套规则校验（框合法、部位名带单品前缀、同图去重）→ 回归一化框。前端按框在原图上显示裁切缩略图（不另调裁图接口），
// 用户删减 / 重新裁切后把选中的框带回 SubmitTryon.detail_crops（detail_crops_manual=true）。
// 限流同姿态配对预览：每用户同时一个（进程内），sidecar 调用占全进程共享的预览读图名额（PreviewSlots）。
func (l *DetectDetailsLogic) DetectDetails(in *studio.DetectDetailsReq) (*studio.DetectDetailsResp, error) {
	s := l.svcCtx
	dp := newDeps(s)
	d := &detailDetector{
		owns:       dp.owns,
		ownsSigned: dp.ownsSigned,
		timeout:    time.Duration(s.Config.Timeouts.Crop) * time.Second,
		slots:      s.PreviewSlots,
	}
	if s.MediaSvc != nil {
		d.media = s.MediaSvc
	}
	if s.Reverse != nil && s.Reverse.Enabled() {
		d.rev = s.Reverse
	}
	return d.detect(l.ctx, in)
}

// ownsInput 输入图归属，与提交同口径：本存储签发 / 素材库公有桶，或本人成品的签名水印地址。
func (d *detailDetector) ownsInput(ctx context.Context, userID int64, u string) bool {
	if d.owns != nil && d.owns(u) {
		return true
	}
	return d.ownsSigned != nil && d.ownsSigned(ctx, userID, u)
}

// detectInflight 每个用户同一时间只跑一次「AI 找细节」（读图不计费，防止连点把 sidecar 打满）。进程内去重，多实例各自一份。
var detectInflight sync.Map

type detailDetector struct {
	media pipeline.Media
	rev   pipeline.Reverse
	owns  func(url string) bool
	// ownsSigned 用户自己成品的签名水印地址（免费档结果），与提交同口径（submit_common: owns || ownsSigned）
	ownsSigned func(ctx context.Context, userID int64, url string) bool
	timeout    time.Duration // crop-plan 单次时限（Timeouts.Crop，0 = pipeline.CropTimeout）
	slots      chan struct{} // 共享读图名额；nil 只在单测里出现（不限）
}

func (d *detailDetector) detect(ctx context.Context, in *studio.DetectDetailsReq) (*studio.DetectDetailsResp, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	items, err := CheckItems(in.Items)
	if err != nil {
		return nil, err
	}
	gs, err := CheckGarments(in.GarmentImages)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, xerr.New(xerr.ParamMissing, "先上传服装图")
	}
	for i, g := range gs {
		if !d.ownsInput(ctx, in.UserId, g.Url) {
			return nil, xerr.Param(fmt.Sprintf("服装图第 %d 张 不是本服务上传的地址", i+1))
		}
	}
	if err := checkPromptLen("补充说明", in.Prompt); err != nil {
		return nil, err
	}
	if err := checkPromptLen("面料", in.Fabric); err != nil {
		return nil, err
	}
	if d.media == nil || d.rev == nil {
		return nil, xerr.New(xerr.NotReady, "AI 找细节暂未开放，请稍后再试")
	}
	if _, busy := detectInflight.LoadOrStore(in.UserId, struct{}{}); busy {
		return nil, xerr.New(xerr.Conflict, "上一次 AI 找细节还在进行中，请稍候")
	}
	defer detectInflight.Delete(in.UserId)

	inputs := make([]pipeline.GarmentInput, 0, len(gs))
	for _, g := range gs {
		inputs = append(inputs, pipeline.GarmentInput{URL: g.Url, Role: g.Role, Angle: g.Angle})
	}
	imgs, _ := pipeline.NumberGarments(items, inputs)
	if len(imgs) == 0 {
		return nil, xerr.Param("服装图与所选单品对不上，请检查单品")
	}
	if err := prepareGarments(ctx, d.media, imgs); err != nil {
		return nil, detectErr(err)
	}
	req := reverse.CropPlanReq{Category: pipeline.InferCategory(items), Items: items, Fabric: in.Fabric, Notes: strings.TrimSpace(in.Prompt)}
	for _, g := range imgs {
		req.Images = append(req.Images, reverse.CropPlanImage{Data: g.Data, Angle: g.Angle, Role: g.Role})
	}
	var resp *reverse.CropPlanResp
	err = d.withSlot(ctx, func(cctx context.Context) error {
		var e error
		resp, e = d.rev.CropPlan(cctx, req)
		return e
	})
	if err != nil {
		return nil, detectErr(err)
	}
	return detailResp(imgs, len(gs), resp.Plans, items), nil
}

// prepareGarments 并发（≤4）下载压缩服装图并取字节（与编排 ingest 同参数：长边 1600、JPEG）。
func prepareGarments(ctx context.Context, media pipeline.Media, imgs []pipeline.Image) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, 4)
	)
	for i := range imgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(im *pipeline.Image) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := prepareImage(ctx, media, im, 1600, true); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(&imgs[i])
	}
	wg.Wait()
	return first
}

func (d *detailDetector) withSlot(ctx context.Context, call func(ctx context.Context) error) error {
	if d.slots != nil {
		select {
		case d.slots <- struct{}{}:
			defer func() { <-d.slots }()
		case <-ctx.Done():
			return context.DeadlineExceeded
		}
	}
	timeout := d.timeout
	if timeout <= 0 {
		timeout = pipeline.CropTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return call(cctx)
}

// detailResp crop-plan 结果 → 响应：每张提交的服装图一项（按 garment_index），框走编排同一套校验（ValidNormBox / NormalizeCropLabel / 同图去重），
// 每张最多 MaxUserCropsPerImage 个、整批最多 MaxUserCrops 个（与提交校验一致，用户原样带回不会被拒）。
func detailResp(imgs []pipeline.Image, garments int, plans []reverse.CropPlan, items []string) *studio.DetectDetailsResp {
	out := &studio.DetectDetailsResp{Plans: make([]*studio.DetailPlan, garments)}
	for i := range out.Plans {
		out.Plans[i] = &studio.DetailPlan{GarmentIndex: int32(i), Angle: "unknown"}
	}
	for _, g := range imgs {
		if g.Source >= 0 && g.Source < garments && g.Angle != "" {
			out.Plans[g.Source].Angle = g.Angle
		}
	}
	for _, p := range plans {
		if p.Index < 0 || p.Index >= len(imgs) {
			continue
		}
		src := imgs[p.Index]
		if src.Source < 0 || src.Source >= garments {
			continue
		}
		dp := out.Plans[src.Source]
		if a := strings.TrimSpace(p.Angle); a != "" && a != "unknown" && (dp.Angle == "" || dp.Angle == "unknown") {
			dp.Angle = a
		}
		for _, c := range p.Crops {
			if len(dp.Crops) >= pipeline.MaxUserCropsPerImage || int(out.Total) >= pipeline.MaxUserCrops {
				break
			}
			if !pipeline.ValidNormBox(c.Box) {
				continue
			}
			label, ok := pipeline.NormalizeCropLabel(c.Label, src.Item, items)
			if !ok || len([]rune(label)) > pipeline.MaxCropLabelRunes || hasCropLabel(dp.Crops, label) {
				continue
			}
			dp.Crops = append(dp.Crops, &studio.DetailCrop{
				GarmentIndex: int32(src.Source), Label: label, Box: clampBox(c.Box), Confidence: round4(c.Confidence),
			})
			out.Total++
		}
	}
	var missing []string
	for _, dp := range out.Plans {
		if len(dp.Crops) == 0 {
			missing = append(missing, fmt.Sprint(dp.GarmentIndex+1))
		}
	}
	switch {
	case out.Total == 0:
		out.Warnings = append(out.Warnings, "没有找到可用的细节，可以点「重新裁切」再试，或不用细节图直接提交")
	case len(missing) > 0:
		out.Warnings = append(out.Warnings, fmt.Sprintf("第 %s 张服装图没有找到细节，可以点「重新裁切」再试", strings.Join(missing, "、")))
	}
	return out
}

func hasCropLabel(list []*studio.DetailCrop, label string) bool {
	for _, c := range list {
		if c.Label == label {
			return true
		}
	}
	return false
}

// clampBox 4 位小数，并把 sidecar 舍入造成的 x+w / y+h 略超 1 收回图内（提交校验同样有 1e-3 容差）。
func clampBox(b []float64) []float64 {
	x, y, w, h := round4(b[0]), round4(b[1]), round4(b[2]), round4(b[3])
	if x+w > 1 {
		w = round4(1 - x)
	}
	if y+h > 1 {
		h = round4(1 - y)
	}
	return []float64{x, y, w, h}
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// detectErr 外部错误 → 用户可读的错误（不透出 sidecar / 网关原文）。
func detectErr(err error) error {
	var ce *xerr.CodeError
	if errors.As(err, &ce) {
		return err
	}
	var re *reverse.Error
	switch {
	case reverse.IsNotConfigured(err):
		return xerr.New(xerr.NotReady, "AI 找细节暂未开放，请稍后再试")
	case reverse.IsRejected(err):
		msg := ""
		if errors.As(err, &re) {
			msg = strings.TrimSpace(re.Message)
		}
		if !userSafeText(msg) {
			msg = "服装图无法识别，请换一张更清晰的图"
		}
		return xerr.Param(msg)
	case errors.Is(err, context.DeadlineExceeded):
		return xerr.New(xerr.ThirdPartyError, "AI 找细节超时，请稍后重试")
	case reverse.IsRetryable(err):
		return xerr.New(xerr.ThirdPartyError, "AI 找细节暂时不可用，请稍后重试")
	}
	return xerr.New(xerr.ThirdPartyError, "服装图下载或识别失败，请稍后重试")
}
