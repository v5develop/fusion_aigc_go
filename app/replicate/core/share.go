package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 成片分享进示例库（CONTRACT §T）：用户在成片卡上点「分享为示例」→ 智能体出授权表单 → replicate_share_sample 建一条待审核投稿。
//
//   - 免费、不动账：只收「完整版」成片（任务成功且 result.kind=full、成片可用；预览版 / 部分成片都不收）；一个成片一条投稿
//     （(job_id) 唯一）：重复调用回放已有投稿；用户撤回后可重新投（同一行改回待审核）；被运营驳回的不能再投。
//   - 肖像授权：方案替换了人物（dims.model.mode=replace）时必须有用户的肖像授权。授权是 Java 在对话里收集的（授权表单），Go 看不到，
//     所以智能体在用户勾选后调工具时带 portrait_consent=true，这里校验并留一份记录（afv_replicate_share.portrait_consent）；没带就拒收
//     PORTRAIT_CONSENT_REQUIRED，不建投稿。没替换人物的方案不要求（带了也只是记录）。
//   - 审核通过由管理端完成（app/replicate/admin/share.go）：用投稿的共享分析 + 改法 + 成片生成一条示例；
//     用户随时可撤回（replicate_withdraw_sample）：待审核的直接作废，已通过的示例立即下架（逻辑删除 + 停用，同后台删除示例）。
//   - 奖励不在这里：积分只在 Java，运营在管理端点「发放奖励」，Go 只记发放状态与月度次数（见 admin/share.go）。
// ---------------------------------------------------------------------------------------------

const (
	// ReasonPortraitConsentRequired 方案替换了人物但没带肖像授权。
	ReasonPortraitConsentRequired = "PORTRAIT_CONSENT_REQUIRED"
	// ReasonShareRejected 这条成片的投稿已被驳回，不能再投。
	ReasonShareRejected = "SHARE_REJECTED"
)

// ShareStore 投稿读写（生产实现 app/replicate/repo；不装配时分享 / 撤回回 7003）。
type ShareStore interface {
	GetShare(ctx context.Context, id int64) (*model.Share, error)
	GetShareByJob(ctx context.Context, jobID int64) (*model.Share, error)
	// InsertShare 新建投稿；(job_id) 已有投稿返回 inserted=false（并发重复提交）。
	InsertShare(ctx context.Context, sh *model.Share) (inserted bool, err error)
	// ReopenShare 撤回过的投稿重新投：withdrawn → pending（CAS），刷新授权记录与行业。没抢到返回 false。
	ReopenShare(ctx context.Context, id int64, consent bool, industry string) (bool, error)
	// WithdrawShare 本人撤回（同一事务）：pending / approved → withdrawn；approved 的还要把生成的示例下架。
	// 返回撤回前的投稿行（找不到 / 不是本人返回 nil）与这次是否真的改了状态（已撤回 / 已驳回 = false）。
	WithdrawShare(ctx context.Context, userID, id int64) (prev *model.Share, changed bool, err error)
	// UserIndustry 用户在广场设置的主行业 code（只读复用；没有返回空串）。
	UserIndustry(ctx context.Context, userID int64) (string, error)
}

// WithShare 装配成片分享。不装配时分享 / 撤回回 7003。
func WithShare(st ShareStore) Option { return func(s *Service) { s.shares = st } }

var shareStatusLabel = map[string]string{
	model.SharePending: "待审核", model.ShareApproved: "已上线", model.ShareRejected: "未通过", model.ShareWithdrawn: "已撤回",
}

// ShareStatusLabel 投稿状态的中文名。
func ShareStatusLabel(status string) string { return shareStatusLabel[status] }

// ShareView replicate_share_sample 的结果。
type ShareView struct {
	// SubmissionID 投稿号（撤回时用）
	SubmissionID int64  `json:"submissionId"`
	JobID        int64  `json:"jobId"`
	Status       string `json:"status"` // pending / approved / rejected / withdrawn
	StatusLabel  string `json:"statusLabel"`
	// PortraitConsent 这条投稿记录了肖像授权
	PortraitConsent bool `json:"portraitConsent"`
	// Replay true = 这条成片早就投过，返回的是已有投稿（没有新建）
	Replay bool   `json:"replay,omitempty"`
	Text   string `json:"text"`
}

func shareText(status string, replay bool) string {
	switch status {
	case model.SharePending:
		if replay {
			return "这条成片已经投稿过了，正在等运营审核。可以随时撤回。"
		}
		return "已提交，等运营审核，通过后会作为公开示例展示（不展示你的账号信息），可以随时撤回。"
	case model.ShareApproved:
		return "这条成片已经作为公开示例上线了，可以随时撤回。"
	case model.ShareRejected:
		return "这条成片的投稿没有通过审核，不能再次投稿。"
	}
	return "已撤回。"
}

func shareView(sh *model.Share, replay bool) *ShareView {
	return &ShareView{SubmissionID: sh.ID, JobID: sh.JobID, Status: sh.Status, StatusLabel: ShareStatusLabel(sh.Status),
		PortraitConsent: sh.PortraitConsent, Replay: replay, Text: shareText(sh.Status, replay)}
}

// replacesPortrait 方案是否把原片里的人物换成了别人（人物项 mode=replace）。
func replacesPortrait(ann Annotations) bool {
	return ann.Dims["model"].Mode == ModeReplace
}

// ShareSample 把一条完整成片投稿成公开示例（待审核）。consent = 用户已勾选肖像授权（智能体在授权表单确认后传）。
func (s *Service) ShareSample(ctx context.Context, userID, jobID int64, consent bool) (*ShareView, error) {
	if s.shares == nil {
		return nil, xerr.New(xerr.NotReady, "分享示例暂未开放")
	}
	j, doc, err := s.deliveredFinal(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	if j.Status != model.JobSucceeded || doc.Kind != ResultFull { // 只收完整版：部分成功 / 部分成片都不收
		return nil, notDelivered()
	}
	cur, err := s.shares.GetShareByJob(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	if cur != nil && cur.Status != model.ShareWithdrawn {
		if cur.Status == model.ShareRejected {
			return nil, xerr.NewReason(xerr.Conflict, ReasonShareRejected, shareText(cur.Status, true))
		}
		return shareView(cur, true), nil // 待审核 / 已通过：回放，不重复投
	}
	p, err := s.store.GetPlan(ctx, j.PlanID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个任务的方案")
	}
	var ann Annotations
	if err := json.Unmarshal(p.Annotations, &ann); err != nil {
		return nil, xerr.New(xerr.ServerError, "方案读取失败，请稍后再试")
	}
	if replacesPortrait(ann) && !consent {
		logx.WithContext(ctx).Infof("replicate: 分享示例拒收 判据=PORTRAIT_CONSENT_REQUIRED user=%d job=%d 方案替换了人物但没带肖像授权", userID, j.ID)
		return nil, xerr.NewReason(xerr.Conflict, ReasonPortraitConsentRequired,
			"这条成片替换了人物，分享为公开示例前需要你确认肖像授权（同意作为公开示例展示、可随时撤回）")
	}
	industry, err := s.shares.UserIndustry(ctx, userID)
	if err != nil { // 行业只是给新示例归类用，读不到不挡投稿
		logx.WithContext(ctx).Errorf("replicate: 读用户行业偏好失败（投稿行业留空）user=%d: %v", userID, err)
		industry = ""
	}
	if cur != nil { // 撤回过：同一行改回待审核
		ok, err := s.shares.ReopenShare(ctx, cur.ID, consent, industry)
		if err != nil {
			return nil, err
		}
		if !ok { // 被别的请求先动了：按最新状态回放
			if cur, err = s.shares.GetShare(ctx, cur.ID); err != nil || cur == nil {
				return nil, err
			}
			return shareView(cur, true), nil
		}
		cur.Status, cur.PortraitConsent, cur.Industry = model.SharePending, consent, industry
		logx.WithContext(ctx).Infof("replicate: 重新投稿 submission=%d job=%d user=%d", cur.ID, j.ID, userID)
		return shareView(cur, false), nil
	}
	sh := &model.Share{UserID: userID, JobID: j.ID, PlanID: j.PlanID, Status: model.SharePending, PortraitConsent: consent,
		Industry: industry, RewardStatus: model.RewardNone}
	inserted, err := s.shares.InsertShare(ctx, sh)
	if err != nil {
		return nil, err
	}
	if !inserted { // 并发重复提交：回放先到的那条
		if cur, err = s.shares.GetShareByJob(ctx, j.ID); err != nil || cur == nil {
			return nil, err
		}
		return shareView(cur, true), nil
	}
	s.funnel(ctx, funnelEvent{Stage: StageShareSample, UserID: userID, PlanID: j.PlanID, JobID: j.ID, Dedupe: "ss:" + strconv.FormatInt(j.ID, 10)})
	logx.WithContext(ctx).Infof("replicate: 新投稿 submission=%d job=%d user=%d 肖像授权=%v 行业=%q", sh.ID, j.ID, userID, consent, industry)
	return shareView(sh, false), nil
}

// WithdrawView replicate_withdraw_sample 的结果。
type WithdrawView struct {
	SubmissionID int64  `json:"submissionId"`
	Status       string `json:"status"`
	StatusLabel  string `json:"statusLabel"`
	// Unpublished true = 这条投稿已经上线，示例刚刚被下架
	Unpublished bool `json:"unpublished"`
	// Replay true = 早就撤回过 / 本来就没通过，这次没有变化
	Replay bool   `json:"replay,omitempty"`
	Text   string `json:"text"`
}

// WithdrawSample 用户撤回自己的投稿：待审核的作废，已上线的示例立即下架。已撤回 / 已驳回的重复调用不报错（回放）。
func (s *Service) WithdrawSample(ctx context.Context, userID, submissionID int64) (*WithdrawView, error) {
	if s.shares == nil {
		return nil, xerr.New(xerr.NotReady, "分享示例暂未开放")
	}
	prev, changed, err := s.shares.WithdrawShare(ctx, userID, submissionID)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	}
	v := &WithdrawView{SubmissionID: prev.ID}
	if !changed {
		v.Status, v.Replay = prev.Status, true
		v.StatusLabel = ShareStatusLabel(v.Status)
		if prev.Status == model.ShareRejected {
			v.Text = "这条投稿没有通过审核，没有上线，不需要撤回。"
		} else {
			v.Text = "这条投稿早已撤回。"
		}
		return v, nil
	}
	v.Status, v.StatusLabel = model.ShareWithdrawn, ShareStatusLabel(model.ShareWithdrawn)
	v.Unpublished = prev.Status == model.ShareApproved
	if v.Unpublished {
		v.Text = "已撤回，公开示例已下架。"
	} else {
		v.Text = "已撤回，这条投稿不会再被审核。"
	}
	s.funnel(ctx, funnelEvent{Stage: StageWithdrawSample, UserID: userID, PlanID: prev.PlanID, JobID: prev.JobID, Dedupe: "ws:" + strconv.FormatInt(prev.ID, 10)})
	var sampleID int64
	if prev.SampleID != nil {
		sampleID = *prev.SampleID
	}
	logx.WithContext(ctx).Infof("replicate: 撤回投稿 submission=%d job=%d user=%d 示例下架=%v sample=%d", prev.ID, prev.JobID, userID, v.Unpublished, sampleID)
	return v, nil
}

// ---------------------------------------------------------------------------------------------
// 给管理端用的纯函数：改法摘要、公开示例用的改法（去掉一切用户内容）
// ---------------------------------------------------------------------------------------------

// ShareChange 投稿的改法摘要（管理端列表展示；不含参考图 / 备注 / 台词 / 叠字）。
type ShareChange struct {
	Label string        `json:"label"` // 如「换模特 + 保留原声」；没改任何项是「未做改动」
	Dims  []TrendingDim `json:"dims"`
	Audio string        `json:"audio,omitempty"`
}

// ShareChangeSummary 把方案改法翻成摘要。读不出来返回空摘要（不报错）。
func ShareChangeSummary(raw json.RawMessage) ShareChange {
	out := ShareChange{Label: "未做改动", Dims: []TrendingDim{}}
	var ann Annotations
	if len(raw) == 0 || json.Unmarshal(raw, &ann) != nil {
		return out
	}
	_, dims, audio, ok := comboOf(ann)
	if !ok {
		return out
	}
	out.Dims, out.Label = dims, trendingLabel(dims, audio)
	if audio != AudioRegenerate {
		out.Audio = audio
	}
	return out
}

// ShareSampleAnnotations 投稿通过后写进示例的改法：只留每一项的处理方式与声音策略，去掉参考图序号 / 备注 / 逐套换装 / 叠字 / 配音台词
// （这些都是用户自己的内容，示例是公开的）。换声音要带音色与台词才有效，示例里退回默认的重新配乐。
func ShareSampleAnnotations(raw json.RawMessage) json.RawMessage {
	var ann Annotations
	if len(raw) == 0 || json.Unmarshal(raw, &ann) != nil {
		return nil
	}
	clean := Annotations{Dims: map[string]DimEdit{}, Audio: ann.Audio}
	for _, k := range DimKeys {
		if e, ok := ann.Dims[k]; ok && (e.Mode == ModeReplace || e.Mode == ModeDescribe) {
			clean.Dims[k] = DimEdit{Mode: e.Mode}
		}
	}
	if clean.Audio == AudioRevoice {
		clean.Audio = AudioRegenerate
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return b
}

// NewSharedAnalysis 投稿通过时给示例用的分析行：把用户那次已成功的分析整行复制成系统身份（user_id=0）的一行
// （结果、阶段状态一并带上：阶段状态里有接缝用的运动量曲线，用示例出方案时要读），这样示例走与运营登记的示例完全相同的读取路径。
// source_key = sha256(share:<投稿号>)：同一投稿重复通过得到同一行（(user_id, source_key) 唯一，仓储层撞键时复用已有那行）。
func NewSharedAnalysis(shareID int64, src *model.Analysis) *model.Analysis {
	sum := sha256.Sum256([]byte("share:" + strconv.FormatInt(shareID, 10)))
	return &model.Analysis{AnalysisNo: newNo("VRA"), UserID: model.SystemUserID, SourceKey: hex.EncodeToString(sum[:]),
		SourceURL: src.SourceURL, SourceRid: src.SourceRid, Status: model.AnalysisSucceeded, Stage: model.StageDone, Attempt: src.Attempt,
		State: src.State, Result: src.Result, FinishedAt: src.FinishedAt}
}
