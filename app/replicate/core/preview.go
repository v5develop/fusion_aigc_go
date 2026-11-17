package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：定妆图（CONTRACT §J2）。方案确认前，用原片第 1 段的代表帧 + 方案参考图 + 改法，经网关 image.edit
// 出一张「替换后的第 1 帧」让用户先看效果。免费、每人每天 N 次（默认 3）、只在成功出图时占额度；同一方案指纹只出一张，重复调用回放。
//
// 输入帧：分析阶段已抽的均匀帧（AnalysisState.Frames，每帧带原片秒数），取离「第 1 段中点」最近的一帧；
// 不另调媒体服务抽帧（媒体服务只支持均匀抽帧，不支持指定时间点，见 pkg/mediasvc.Frames）。
// 内容安全：方案的参考图与分析帧在生成 / 报价方案时已过审核（moderation.go）；这里出图前再过一次同一道门（gateModeration，
// 与确认同口径：不通过 / 审核服务故障 fail-close 都不出图）。出图结果本身不另做审核（输入已审，且只给本人看，不入素材库、不外发）。

// 额度与重试用到的 reason（工具错误 error_code 原样取它们）。
const (
	ReasonPreviewQuotaExhausted = "PREVIEW_QUOTA_EXHAUSTED"
	reasonInProgress            = "IN_PROGRESS"
	reasonThirdParty            = "THIRD_PARTY_ERROR"
)

// 出图中占坑的回收时限：超过它没动静（进程崩溃）才允许别的调用接手。须大于 PreviewWait。
const previewClaimStale = 3 * time.Minute

var cstZone = time.FixedZone("CST", 8*3600)

// PreviewStore 定妆图的读写与每日额度（生产实现 app/replicate/repo；不装配时定妆图回 7003）。
type PreviewStore interface {
	GetPreview(ctx context.Context, userID int64, planHash string) (*model.Preview, error)
	// ClaimPreview 占坑（(user_id, plan_hash) 唯一）：占到返回 true 并回填 p.ID；已有完成的 / 别人正在出图 / 占坑未超过 staleAfter 返回 false。
	ClaimPreview(ctx context.Context, p *model.Preview, staleAfter time.Duration) (bool, error)
	// FinishPreview 出图中 → 已出图（写 image_rid）。
	FinishPreview(ctx context.Context, id int64, imageRid, callID string) (bool, error)
	// DeletePreviewClaim 出图失败释放占坑（只删出图中的）。
	DeletePreviewClaim(ctx context.Context, id int64) error
	// AcquirePreviewQuota 原子占一次当日额度：已用 < total 才 +1。返回占后的已用数与是否占到。
	AcquirePreviewQuota(ctx context.Context, userID int64, day string, total int) (used int, ok bool, err error)
	// ReleasePreviewQuota 退回一次（出图没成功）。
	ReleasePreviewQuota(ctx context.Context, userID int64, day string) error
	PreviewQuotaUsed(ctx context.Context, userID int64, day string) (int, error)
}

// WithPreview 装配定妆图。不装配时 PreviewFrame 回 7003。
func WithPreview(p PreviewStore) Option { return func(s *Service) { s.previews = p } }

// PreviewView 定妆图（agent 工具 replicate_preview_frame 的结果；Java 帧 entity {type:"preview", id: planNo}）。
type PreviewView struct {
	PlanNo     string  `json:"planNo"`
	ImageURL   string  `json:"imageUrl,omitempty"` // 定妆图（现签短时地址，过期后重调工具即可：回放不占额度）
	ImageRid   string  `json:"imageRid"`
	FrameAtSec float64 `json:"frameAtSec"`
	// FrameURL 原片帧（并排对比用；只增字段）
	FrameURL     string     `json:"frameUrl,omitempty"`
	QuotaUsed    int        `json:"quotaUsed"`
	QuotaTotal   int        `json:"quotaTotal"`
	URLExpiresAt *time.Time `json:"urlExpiresAt,omitempty"`
	// Replay true = 这个方案已经出过图，这次直接返回（没占额度）
	Replay bool `json:"replay,omitempty"`
}

func (s *Service) previewDay() string { return s.now().In(cstZone).Format("2006-01-02") }

// PreviewFrame 给方案出（或回放）一张定妆图。
func (s *Service) PreviewFrame(ctx context.Context, userID int64, planNo string) (*PreviewView, error) {
	if s.cfg.PreviewOff || s.previews == nil || s.gw == nil {
		return nil, xerr.New(xerr.NotReady, "定妆图暂未开放")
	}
	p, an, err := s.loadPlan(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	switch {
	case p.Status == model.PlanStale:
		return nil, errPlanStale()
	case p.Status == model.PlanReady && s.now().After(p.ExpiresAt):
		return nil, xerr.NewReason(xerr.Conflict, "PLAN_EXPIRED", "这个方案已过期。点「重新生成方案」，不用重新分析")
	}
	day, total := s.previewDay(), s.cfg.PreviewDailyQuota

	// 同一方案只生成一次：已有的直接回放，不占额度
	if cur, err := s.previews.GetPreview(ctx, userID, p.PlanHash); err != nil {
		return nil, err
	} else if cur != nil && cur.Status == model.PreviewDone {
		return s.previewView(ctx, p.PlanNo, cur, day, true)
	}

	edits := previewEdits(p)
	if len(edits) == 0 {
		return nil, xerr.Param("这个方案没有要改画面的地方（只改了台词、声音或叠字），不需要定妆图")
	}
	frame, err := s.previewFrameOf(ctx, p)
	if err != nil {
		return nil, err
	}
	// 出图前过内容检查（同确认口径）：参考图 / 帧不通过或审核服务故障都不出图，也不占额度
	if err := s.gateModeration(ctx, p); err != nil {
		return nil, err
	}

	used, ok, err := s.previews.AcquirePreviewQuota(ctx, userID, day, total)
	if err != nil {
		return nil, err
	}
	if !ok {
		logx.WithContext(ctx).Infof("replicate: 定妆图拒收 判据=PREVIEW_QUOTA_EXHAUSTED user=%d plan=%s 今日已用=%d/%d", userID, p.PlanNo, used, total)
		return nil, xerr.NewReason(xerr.Conflict, ReasonPreviewQuotaExhausted,
			fmt.Sprintf("今天的 %d 次免费定妆图已经用完了，明天 0 点恢复。现在也可以直接确认方案生成视频", total))
	}
	release := func() {
		if err := s.previews.ReleasePreviewQuota(context.WithoutCancel(ctx), userID, day); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 定妆图额度退回失败 判据=PREVIEW_QUOTA_RELEASE_FAILED user=%d day=%s: %v", userID, day, err)
		}
	}
	claim := &model.Preview{UserID: userID, PlanID: p.ID, PlanNo: p.PlanNo, PlanHash: p.PlanHash, FrameRid: frame.Rid, FrameAt: frame.T}
	claimed, err := s.previews.ClaimPreview(ctx, claim, previewClaimStale)
	if err != nil {
		release()
		return nil, err
	}
	if !claimed {
		release()
		// 别的调用刚好出完了：回放；否则它还在出图
		if cur, gerr := s.previews.GetPreview(ctx, userID, p.PlanHash); gerr == nil && cur != nil && cur.Status == model.PreviewDone {
			return s.previewView(ctx, p.PlanNo, cur, day, true)
		}
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "这张定妆图正在生成，几十秒后再看一下")
	}

	rid, callID, gerr := s.generatePreview(ctx, p, an, edits, frame)
	if gerr != nil {
		// 没成功：释放占坑、退额度（同一幂等键再来会接着用网关上的同一次调用，不会重复出图）
		if derr := s.previews.DeletePreviewClaim(context.WithoutCancel(ctx), claim.ID); derr != nil {
			logx.WithContext(ctx).Errorf("replicate: 定妆图占坑释放失败 判据=PREVIEW_CLAIM_RELEASE_FAILED user=%d plan=%s: %v", userID, p.PlanNo, derr)
		}
		release()
		return nil, gerr
	}
	if ok, err := s.previews.FinishPreview(context.WithoutCancel(ctx), claim.ID, rid, callID); err != nil || !ok {
		// 图出来了但没落成库（占坑被接手 / 库故障）：额度保留（图已生成并花了钱），本次仍把图给用户；下次调用会重新出图，不会丢额度语义
		logx.WithContext(ctx).Errorf("replicate: 定妆图落库失败 判据=PREVIEW_FINISH_FAILED user=%d plan=%s ok=%v: %v", userID, p.PlanNo, ok, err)
	}
	claim.Status, claim.ImageRid, claim.CallID = model.PreviewDone, rid, callID
	return s.previewView(ctx, p.PlanNo, claim, day, false)
}

// GetPreview 只读取方案已存的定妆图（现签新地址）：不出图、不占额度、不看总开关（已出过的图随时能看）。
// 没有（没出过 / 还在出图）→ 4001 RESOURCE_NOT_FOUND。智能体事件流会剥掉签名地址，聊天卡片靠它拿新地址。
func (s *Service) GetPreview(ctx context.Context, userID int64, planNo string) (*PreviewView, error) {
	p, err := s.store.GetPlanByNo(ctx, userID, planNo)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个方案")
	}
	if s.previews == nil {
		return nil, xerr.New(xerr.NotReady, "定妆图暂未开放")
	}
	cur, err := s.previews.GetPreview(ctx, userID, p.PlanHash)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.Status != model.PreviewDone {
		return nil, xerr.New(xerr.NotFound, "这个方案还没有定妆图")
	}
	return s.previewView(ctx, p.PlanNo, cur, s.previewDay(), true)
}

func (s *Service) previewView(ctx context.Context, planNo string, pv *model.Preview, day string, replay bool) (*PreviewView, error) {
	used, err := s.previews.PreviewQuotaUsed(ctx, pv.UserID, day)
	if err != nil {
		return nil, err
	}
	v := &PreviewView{PlanNo: planNo, ImageRid: pv.ImageRid, FrameAtSec: pv.FrameAt, QuotaUsed: used, QuotaTotal: s.cfg.PreviewDailyQuota, Replay: replay}
	if urls, exp := s.signRIDs(ctx, []string{pv.ImageRid, pv.FrameRid}); urls != nil {
		v.ImageURL, v.FrameURL, v.URLExpiresAt = urls[pv.ImageRid], urls[pv.FrameRid], exp
	}
	return v, nil
}

// previewFrameOf 选输入帧：分析抽的均匀帧里离第 1 段中点最近的一帧。
func (s *Service) previewFrameOf(ctx context.Context, p *model.Plan) (FrameOut, error) {
	segs, err := planSegments(p)
	if err != nil || len(segs) == 0 {
		return FrameOut{}, xerr.New(xerr.ServerError, "定妆图暂时出不了，请稍后再试")
	}
	a, err := s.store.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return FrameOut{}, err
	}
	var st AnalysisState
	if a != nil {
		_ = json.Unmarshal(a.State, &st)
	}
	f, ok := nearestFrame(st.Frames, (segs[0].T0+segs[0].T1)/2)
	if !ok {
		logx.WithContext(ctx).Errorf("replicate: 定妆图没有可用的原片帧 判据=PREVIEW_NO_FRAME plan=%s analysis=%d", p.PlanNo, p.AnalysisID)
		return FrameOut{}, xerr.New(xerr.ServerError, "这条视频的画面没有准备好，定妆图暂时出不了")
	}
	return f, nil
}

func nearestFrame(frames []FrameOut, t float64) (FrameOut, bool) {
	best, bestD, found := FrameOut{}, 0.0, false
	for _, f := range frames {
		if f.Rid == "" {
			continue
		}
		d := f.T - t
		if d < 0 {
			d = -d
		}
		if !found || d < bestD {
			best, bestD, found = f, d, true
		}
	}
	return best, found
}

// previewEdit 方案里一处要改画面的地方。
type previewEdit struct {
	Key  string
	Edit DimEdit
}

// previewEdits 画面上看得出的改动：除台词外、mode 为 replace / describe 的维度（按维度固定顺序，保证同一方案提示词稳定）。
func previewEdits(p *model.Plan) []previewEdit {
	var anno Annotations
	_ = json.Unmarshal(p.Annotations, &anno)
	var out []previewEdit
	for _, k := range DimKeys {
		e, ok := anno.Dims[k]
		if !ok || k == "line" || (e.Mode != ModeReplace && e.Mode != ModeDescribe) {
			continue
		}
		out = append(out, previewEdit{Key: k, Edit: e})
	}
	return out
}

// buildPreviewPrompt 图 1 = 原片帧，图 2.. = 方案参考图（图号 N → 第 N+1 张）。只写要改的，其余明确要求保持。
func buildPreviewPrompt(frame FrameOut, edits []previewEdit, an *AnalysisResult, nRefs int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "图1是一条视频第 %.1f 秒的画面。请在图1的基础上只做下面的修改：\n", frame.T)
	for i, e := range edits {
		label := DimLabel[e.Key]
		var line string
		switch e.Edit.Mode {
		case ModeReplace:
			var refs []string
			for _, r := range e.Edit.Refs {
				if r >= 1 && r <= nRefs {
					refs = append(refs, fmt.Sprintf("图%d", r+1))
				}
			}
			line = fmt.Sprintf("把画面里的%s换成%s里的", label, strings.Join(refs, "、"))
			if h := refHint[e.Key]; h != "" {
				line += "（" + h + "）"
			}
			if n := strings.TrimSpace(e.Edit.Note); n != "" {
				line += "；补充要求：" + n
			}
		default:
			line = fmt.Sprintf("把画面里的%s改成：%s", label, strings.TrimSpace(e.Edit.Note))
		}
		if an != nil {
			if d := an.Dims[e.Key]; d.Present && d.Desc != "" {
				line += fmt.Sprintf("（原画面里是：%s）", d.Desc)
			}
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, line)
	}
	if nRefs > 0 {
		var m []string
		for i := 1; i <= nRefs; i++ {
			m = append(m, fmt.Sprintf("图%d是参考图%d", i+1, i))
		}
		b.WriteString("参考图对应：" + strings.Join(m, "，") + "。\n")
	}
	b.WriteString("其余全部保持不变：构图与景别、人物的姿势与表情、光线、背景、画面比例。只输出一张图片，不要加任何文字、水印或边框。")
	return b.String()
}

// generatePreview 提交 image.edit 并等到终态（最多 PreviewWait）。超时 / 网关失败 → 可重试的 THIRD_PARTY_ERROR。
func (s *Service) generatePreview(ctx context.Context, p *model.Plan, an *AnalysisResult, edits []previewEdit, frame FrameOut) (rid, callID string, err error) {
	var refs []RefImage
	_ = json.Unmarshal(p.Refs, &refs)
	imgs := []aigateway.MediaRef{{URI: aigateway.MRScheme + frame.Rid}}
	for _, r := range refs {
		imgs = append(imgs, aigateway.MediaRef{URI: aigateway.MRScheme + r.Rid})
	}
	in := map[string]any{"prompt": buildPreviewPrompt(frame, edits, an, len(refs)), "images": imgs, "n": 1}
	if ar := strings.TrimSpace(p.AspectRatio); ar != "" {
		in["aspect_ratio"] = ar
	}
	req := aigateway.CallRequest{Operation: opImageEdit, ModelGroup: s.cfg.PreviewModelGroup, Input: in,
		Labels:       map[string]string{"caller_biz": "replicate", "kind": "preview", "plan": p.PlanNo},
		MediaPurpose: aigateway.MediaPurposeIntermediate}
	if mc := strings.TrimSpace(s.cfg.MaxCostCNY); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	wctx, cancel := context.WithTimeout(ctx, s.cfg.PreviewWait)
	defer cancel()
	fail := func(check string, cause error) (string, string, error) {
		logx.WithContext(ctx).Errorf("replicate: 定妆图出图失败 判据=%s user=%d plan=%s: %v", check, p.UserID, p.PlanNo, cause)
		return "", "", xerr.NewReason(xerr.ThirdPartyError, reasonThirdParty, "定妆图这次没出成，没有占用今天的次数，稍后再试一次")
	}
	call, err := s.gw.Submit(wctx, req, aigateway.SubmitOptions{IdempotencyKey: previewKey(s.cfg.KeyPrefix, p, s.cfg.PreviewModelGroup),
		Wait: min(s.cfg.PreviewWait, 60*time.Second), RetryFailed: true})
	if err != nil {
		return fail("PREVIEW_SUBMIT_FAILED", err)
	}
	for call != nil && !call.IsTerminal() {
		if wctx.Err() != nil {
			return fail("PREVIEW_TIMEOUT", fmt.Errorf("等待超过 %s call=%s", s.cfg.PreviewWait, call.ID))
		}
		if call, err = s.gw.Get(wctx, call.ID, min(25*time.Second, s.cfg.PreviewWait)); err != nil {
			return fail("PREVIEW_GET_FAILED", err)
		}
	}
	if call == nil {
		return fail("PREVIEW_EMPTY_RESPONSE", fmt.Errorf("网关返回空"))
	}
	if !call.Succeeded() || call.Result == nil {
		class := call.Status
		if call.Failure != nil && call.Failure.Class != "" {
			class = call.Failure.Class
		}
		return fail("PREVIEW_GATEWAY_"+class, fmt.Errorf("call=%s status=%s", call.ID, call.Status))
	}
	for _, m := range call.Result.Media() {
		if m.Rid != "" {
			return m.Rid, call.ID, nil
		}
	}
	return fail("PREVIEW_NO_OUTPUT", fmt.Errorf("call=%s 没有产物", call.ID))
}

// previewKey 网关幂等键：用户 + 方案指纹 + 模型组（换模型组即换请求体，须换键）。
func previewKey(prefix string, p *model.Plan, group string) string {
	h := p.PlanHash
	if len(h) > 16 {
		h = h[:16]
	}
	return fmt.Sprintf("%srpl-prev-%d-%s:%s", prefix, p.UserID, h, strings.NewReplacer(".", "-").Replace(group))
}

const opImageEdit = "image.edit"
