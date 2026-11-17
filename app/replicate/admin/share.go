package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 用户投稿管理（CONTRACT §T，/api/v1/replicate/admin/shares/**，权限：读 replicate:view，写 replicate:sample）。
//
//   - 列表：每行带成片 rid（handler 现签预览地址）、改法摘要（只有项名与处理方式，不含用户的参考图 / 备注 / 台词）、遮掩后的用户号、
//     本月已发放奖励次数与 rewardAllowed。
//   - 通过：用投稿的共享分析（整行复制成系统身份的一行）+ 改法（去掉用户内容）+ 成片（复制进素材库桶，媒体服务没配 / 失败则整条通过失败，
//     不退而用用户私有桶里的原 rid）生成一条启用的示例，复用 repo 的示例表与同一套示例读取路径；事务内锁投稿行，重复点击只通过一次。
//   - 驳回：原因必填，展示给用户。
//   - 奖励发放（两阶段，积分只在 Java）：claim（占位：同一事务内锁用户投稿行，校验开关与「同一用户每月最多 N 次」，
//     none → granting 并定下点数快照）→ 管理端调 Java 积分调整接口（幂等键 replicate-share-reward:{submissionId}，点数 = 快照）
//     → mark（granting → granted，无条件，不再看开关 / 上限）。Java 明确失败且不再重试时用 release 取消占位（granting → none）。
//     这样「开关关了 / 月上限被并发占满」只会挡在 claim 之前，不会出现 Java 已加点而 Go 拒收的「已发放未标记」。
// ---------------------------------------------------------------------------------------------

const (
	// ReasonShareNotPending 投稿不是待审核状态（已通过 / 已驳回 / 已被用户撤回）。
	ReasonShareNotPending = "SHARE_NOT_PENDING"
	// ReasonShareNotApproved 投稿没通过审核，不能发奖励。
	ReasonShareNotApproved = "SHARE_NOT_APPROVED"
	// ReasonRewardMonthlyLimit 同一用户本月奖励次数已达上限。
	ReasonRewardMonthlyLimit = "REWARD_MONTHLY_LIMIT"

	// ReasonRewardDisabled 分享奖励已在后台关闭（CONTRACT §U）；只在 claim 时判定。
	ReasonRewardDisabled = "REWARD_DISABLED"
	// ReasonRewardNotClaimed 标记发放时这条投稿还没占位（没走 claim）。
	ReasonRewardNotClaimed = "REWARD_NOT_CLAIMED"
	// ReasonRewardAlreadyGranted 取消占位时这条已经发放完成，不能取消。
	ReasonRewardAlreadyGranted = "REWARD_ALREADY_GRANTED"

	// RewardKeyPrefix 管理端调 Java 积分接口的幂等键前缀：replicate-share-reward:{submissionId}。
	RewardKeyPrefix = "replicate-share-reward:"

	maxRejectReasonRunes = 200
	maxSampleTitleRunes  = 40
)

// ShareStore 投稿管理用到的读写（*repo.Repo 直接满足）。
type ShareStore interface {
	GetShare(ctx context.Context, id int64) (*model.Share, error)
	AdminListShares(ctx context.Context, f repo.ShareFilter, offset, limit int) ([]*model.Share, int64, error)
	RewardCounts(ctx context.Context, userIDs []int64, from, to time.Time) (map[int64]int, error)
	ApproveShare(ctx context.Context, id, adminID int64, a *model.Analysis, smp *model.Sample) (*model.Share, bool, error)
	RejectShare(ctx context.Context, id, adminID int64, reason string) (bool, error)
	ClaimShareReward(ctx context.Context, id int64, p repo.ClaimParams) (*model.Share, repo.ClaimOutcome, error)
	MarkShareRewardGranted(ctx context.Context, id int64, now time.Time) (*model.Share, repo.MarkOutcome, error)
	ReleaseShareReward(ctx context.Context, id int64) (*model.Share, repo.ReleaseOutcome, error)
	GetAnalysis(ctx context.Context, id int64) (*model.Analysis, error)
}

var _ ShareStore = (*repo.Repo)(nil)

// ShareMedia 通过投稿时把成片 / 封面复制进素材库桶（*mediasvc.Client 满足）。
type ShareMedia interface {
	CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error)
}

var _ ShareMedia = (*mediasvc.Client)(nil)

// SetShareMedia 装配通过投稿用的媒体服务；不装配时「通过」回「媒体服务未配置」（宁可不通过，也不让示例引用用户私有桶里的成片）。
func (s *Service) SetShareMedia(m ShareMedia) { s.shareMedia = m }

func (s *Service) shareStore() (ShareStore, error) {
	ss, ok := s.store.(ShareStore)
	if !ok || ss == nil {
		return nil, xerr.New(xerr.NotReady, "功能暂未开放")
	}
	return ss, nil
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

var cstLoc = time.FixedZone("CST", 8*3600)

// monthBounds 北京时间 t 所在自然月的 [起, 止)。
func monthBounds(t time.Time) (time.Time, time.Time) {
	t = t.In(cstLoc)
	from := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, cstLoc)
	return from, from.AddDate(0, 1, 0)
}

// maskUserID 用户号遮掩：12345678 → 12****78；太短的只留首位。
func maskUserID(id int64) string {
	s := strconv.FormatInt(id, 10)
	if len(s) <= 4 {
		return s[:1] + strings.Repeat("*", len(s)-1)
	}
	return s[:2] + "****" + s[len(s)-2:]
}

// ShareRow 管理端一条投稿。
type ShareRow struct {
	ID int64 `json:"id"`
	// UserID 投稿用户（发奖励调 Java 积分接口要用；展示用 UserIDMasked）
	UserID       int64  `json:"userId"`
	UserIDMasked string `json:"userIdMasked"`
	JobID        int64  `json:"jobId"`
	PlanID       int64  `json:"planId"`
	PlanNo       string `json:"planNo,omitempty"`
	Status       string `json:"status"`
	StatusLabel  string `json:"statusLabel"`
	RejectReason string `json:"rejectReason,omitempty"`
	SampleID     int64  `json:"sampleId,omitempty"`
	// Change 方案改法摘要（只有项名与处理方式）
	Change          core.ShareChange `json:"change"`
	PortraitConsent bool             `json:"portraitConsent"`
	Industry        string           `json:"industry,omitempty"`
	// VideoRid / CoverRid 成片与封面的媒体 rid；VideoURL / CoverURL 由接口层现签的短时有效地址（签不出来为空）
	VideoRid string `json:"videoRid,omitempty"`
	CoverRid string `json:"coverRid,omitempty"`
	VideoURL string `json:"videoUrl,omitempty"`
	CoverURL string `json:"coverUrl,omitempty"`
	// 奖励：RewardStatus none / granting（发放中）/ granted；RewardAllowed = 奖励开着 + 已通过 + 还没占位 + 该用户本月没到上限（前端据此置灰「发放奖励」）；
	// MonthRewardCount 该用户本月已发放 + 发放中几次；RewardKey = 调 Java 积分接口用的幂等键
	RewardStatus     string     `json:"rewardStatus"`
	RewardAt         *time.Time `json:"rewardAt,omitempty"`
	RewardAllowed    bool       `json:"rewardAllowed"`
	MonthRewardCount int        `json:"monthRewardCount"`
	// RewardLimit 当前配置的每人每月上限（0 = 不限）；RewardPoints：已占位（granting / granted 且有快照）时是占位时的点数快照，
	// 否则是当前配置的每条奖励点数（管理端调 Java 积分接口以占位返回的快照为准）
	RewardLimit  int    `json:"rewardLimit"`
	RewardPoints int    `json:"rewardPoints"`
	RewardKey    string `json:"rewardKey"`
	// RewardReplay 仅「标记奖励已发放」返回：true = 之前已经标记过，这次没有变化
	RewardReplay bool       `json:"rewardReplay,omitempty"`
	ReviewedBy   int64      `json:"reviewedBy,omitempty"`
	ReviewedAt   *time.Time `json:"reviewedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// ShareList 投稿列表。
type ShareList struct {
	Items []ShareRow `json:"items"`
	Total int64      `json:"total"`
	Page  int        `json:"page"`
	Size  int        `json:"size"`
}

// ShareListReq 投稿检索。
type ShareListReq struct {
	Status string // pending / approved / rejected / withdrawn；空 = 全部
	UserID int64
	Page   int
	Size   int
}

func validShareStatus(s string) bool {
	switch s {
	case "", model.SharePending, model.ShareApproved, model.ShareRejected, model.ShareWithdrawn:
		return true
	}
	return false
}

func (s *Service) shareRow(ctx context.Context, sh *model.Share, monthCount int, rs RewardSettings) ShareRow {
	row := ShareRow{ID: sh.ID, UserID: sh.UserID, UserIDMasked: maskUserID(sh.UserID), JobID: sh.JobID, PlanID: sh.PlanID,
		Status: sh.Status, StatusLabel: core.ShareStatusLabel(sh.Status), RejectReason: sh.RejectReason, PortraitConsent: sh.PortraitConsent,
		Industry: sh.Industry, RewardStatus: sh.RewardStatus, RewardAt: sh.RewardAt, MonthRewardCount: monthCount, RewardLimit: rs.MonthlyLimit, RewardPoints: rs.Points,
		RewardKey: RewardKeyPrefix + strconv.FormatInt(sh.ID, 10), ReviewedBy: sh.ReviewedBy, ReviewedAt: sh.ReviewedAt, CreatedAt: sh.CreatedAt,
		UpdatedAt: sh.UpdatedAt, Change: core.ShareChangeSummary(nil)}
	if sh.SampleID != nil {
		row.SampleID = *sh.SampleID
	}
	if sh.RewardPoints > 0 && (sh.RewardStatus == model.RewardGranting || sh.RewardStatus == model.RewardGranted) {
		row.RewardPoints = sh.RewardPoints
	}
	row.RewardAllowed = rs.Enabled && sh.Status == model.ShareApproved && sh.RewardStatus != model.RewardGranted && sh.RewardStatus != model.RewardGranting &&
		(rs.MonthlyLimit == 0 || monthCount < rs.MonthlyLimit)
	if p, err := s.store.AdminGetPlan(ctx, sh.PlanID); err == nil && p != nil {
		row.PlanNo, row.Change = p.PlanNo, core.ShareChangeSummary(p.Annotations)
	}
	if j, err := s.store.GetJob(ctx, sh.JobID); err == nil && j != nil && len(j.ResultDocument) > 0 {
		var doc core.ResultDoc
		if json.Unmarshal(j.ResultDocument, &doc) == nil {
			row.VideoRid, row.CoverRid = doc.VideoRid, doc.CoverRid
		}
	}
	return row
}

// ListShares 投稿列表（id 倒序）。
func (s *Service) ListShares(ctx context.Context, req ShareListReq) (*ShareList, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	req.Status = strings.TrimSpace(req.Status)
	if !validShareStatus(req.Status) {
		return nil, xerr.Param("status 只能是 pending / approved / rejected / withdrawn")
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Size <= 0 {
		req.Size = 20
	}
	req.Size = min(req.Size, 100)
	list, total, err := ss.AdminListShares(ctx, repo.ShareFilter{Status: req.Status, UserID: req.UserID}, (req.Page-1)*req.Size, req.Size)
	if err != nil {
		return nil, err
	}
	counts, err := s.monthCounts(ctx, ss, list)
	if err != nil {
		return nil, err
	}
	rs := s.rewardSettings(ctx)
	out := &ShareList{Items: make([]ShareRow, 0, len(list)), Total: total, Page: req.Page, Size: req.Size}
	for _, sh := range list {
		out.Items = append(out.Items, s.shareRow(ctx, sh, counts[sh.UserID], rs))
	}
	return out, nil
}

func (s *Service) monthCounts(ctx context.Context, ss ShareStore, list []*model.Share) (map[int64]int, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, sh := range list {
		if !seen[sh.UserID] {
			seen[sh.UserID] = true
			ids = append(ids, sh.UserID)
		}
	}
	from, to := monthBounds(s.clock())
	return ss.RewardCounts(ctx, ids, from, to)
}

// loadShare 读一条投稿并拼成行（带本月奖励次数）。
func (s *Service) loadShare(ctx context.Context, ss ShareStore, id int64) (ShareRow, *model.Share, error) {
	sh, err := ss.GetShare(ctx, id)
	if err != nil {
		return ShareRow{}, nil, err
	}
	if sh == nil {
		return ShareRow{}, nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	}
	counts, err := s.monthCounts(ctx, ss, []*model.Share{sh})
	if err != nil {
		return ShareRow{}, nil, err
	}
	return s.shareRow(ctx, sh, counts[sh.UserID], s.rewardSettings(ctx)), sh, nil
}

// GetShare 一条投稿。
func (s *Service) GetShare(ctx context.Context, id int64) (*ShareRow, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	row, _, err := s.loadShare(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ApproveShare 通过投稿：生成示例并上线。title 可空（缺省取分析摘要的前 40 字）。已通过的重复调用直接返回。
func (s *Service) ApproveShare(ctx context.Context, id, adminID int64, title string) (*ShareRow, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	sh, err := ss.GetShare(ctx, id)
	if err != nil {
		return nil, err
	}
	if sh == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	}
	if sh.Status == model.ShareApproved {
		return s.GetShare(ctx, id)
	}
	if sh.Status != model.SharePending {
		return nil, notPending(sh.Status)
	}
	if s.shareMedia == nil {
		return nil, xerr.New(xerr.NotReady, "媒体服务未配置，不能通过投稿（成片要复制进素材库才能公开展示）")
	}
	j, err := s.store.GetJob(ctx, sh.JobID)
	if err != nil {
		return nil, err
	}
	var doc core.ResultDoc
	if j == nil || j.Status != model.JobSucceeded || len(j.ResultDocument) == 0 || json.Unmarshal(j.ResultDocument, &doc) != nil ||
		doc.Status != core.ResultReady || doc.Kind != core.ResultFull || doc.VideoRid == "" {
		return nil, xerr.NewReason(xerr.Conflict, core.ReasonJobNotDelivered, "这条成片已经不是可用的完整版，不能通过")
	}
	p, err := s.store.AdminGetPlan(ctx, sh.PlanID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, xerr.New(xerr.NotFound, "找不到投稿对应的方案")
	}
	src, err := ss.GetAnalysis(ctx, p.AnalysisID)
	if err != nil {
		return nil, err
	}
	if src == nil || src.Status != model.AnalysisSucceeded || deref(src.SourceRid) == "" {
		return nil, xerr.New(xerr.Conflict, "这条投稿的分析结果或原片已经不在了，不能通过")
	}
	resultRid, err := s.shareCopy(ctx, doc.VideoRid, fmt.Sprintf("replicate-share:%d:result", id))
	if err != nil {
		return nil, err
	}
	var coverRid *string
	if doc.CoverRid != "" { // 封面只是装饰：复制失败不挡通过（示例照常上线，运营可在示例管理里补封面）
		if c, err := s.shareCopy(ctx, doc.CoverRid, fmt.Sprintf("replicate-share:%d:cover", id)); err != nil {
			logx.WithContext(ctx).Errorf("replicate-admin: 投稿封面复制失败（不挡通过）判据=share_cover_copy_failed share=%d: %v", id, err)
		} else {
			coverRid = &c
		}
	}
	now := s.clock()
	smp := &model.Sample{Title: s.shareTitle(ctx, strings.TrimSpace(title), src, id), Industry: sh.Industry, SourceRid: deref(src.SourceRid),
		CoverRid: coverRid, ResultRid: &resultRid, Annotations: core.ShareSampleAnnotations(p.Annotations), Enabled: true,
		AnalyzedAt: &now, CreatedBy: adminID, UpdatedBy: adminID}
	got, approved, err := ss.ApproveShare(ctx, id, adminID, core.NewSharedAnalysis(id, src), smp)
	if err != nil {
		return nil, err
	}
	if got == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	}
	if !approved && got.Status != model.ShareApproved { // 并发里被用户撤回 / 别的运营驳回
		return nil, notPending(got.Status)
	}
	if approved {
		logx.WithContext(ctx).Infof("replicate-admin: 通过投稿 share=%d sample=%d admin=%d user=%d", id, smp.ID, adminID, sh.UserID)
	}
	return s.GetShare(ctx, id)
}

func notPending(status string) error {
	return xerr.NewReason(xerr.Conflict, ReasonShareNotPending, "这条投稿现在是「"+core.ShareStatusLabel(status)+"」，不能再审核")
}

func (s *Service) shareCopy(ctx context.Context, rid, idem string) (string, error) {
	res, err := s.shareMedia.CopyToLibrary(ctx, rid, mediasvc.PurposeAssetLibrary, idem)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate-admin: 投稿素材复制进素材库失败 判据=share_media_copy_failed rid=%s: %v", rid, err)
		return "", xerr.New(xerr.ServerError, "素材复制失败，请稍后重试")
	}
	if res.RID == "" {
		return "", xerr.New(xerr.ServerError, "素材复制失败，请稍后重试")
	}
	return res.RID, nil
}

// shareTitle 示例标题：运营填的优先（≤40 字，超了截断）；缺省取分析摘要；再缺省用投稿号。
func (s *Service) shareTitle(ctx context.Context, given string, a *model.Analysis, id int64) string {
	t := given
	if t == "" {
		if an, err := s.resolveAnalysis(ctx, a); err == nil && an != nil {
			t = strings.TrimSpace(an.Summary)
		}
	}
	if t == "" {
		t = fmt.Sprintf("用户分享示例 #%d", id)
	}
	if r := []rune(t); len(r) > maxSampleTitleRunes {
		t = string(r[:maxSampleTitleRunes])
	}
	return t
}

// RejectShare 驳回投稿：原因必填（≤200 字，展示给用户）。已驳回且原因相同的重复调用直接返回。
func (s *Service) RejectShare(ctx context.Context, id, adminID int64, reason string) (*ShareRow, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, xerr.New(xerr.ParamMissing, "请填写驳回原因")
	}
	if len([]rune(reason)) > maxRejectReasonRunes {
		return nil, xerr.Param("驳回原因写短一点（200 字以内）")
	}
	ok, err := ss.RejectShare(ctx, id, adminID, reason)
	if err != nil {
		return nil, err
	}
	row, sh, err := s.loadShare(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	if !ok && !(sh.Status == model.ShareRejected && sh.RejectReason == reason) {
		return nil, notPending(sh.Status)
	}
	if ok {
		logx.WithContext(ctx).Infof("replicate-admin: 驳回投稿 share=%d admin=%d 原因=%q", id, adminID, reason)
	}
	return &row, nil
}

// RewardClaim claim 的结果：管理端据此调 Java 积分接口（amount = RewardPoints，idempotencyKey = RewardKey）。
type RewardClaim struct {
	RewardKey    string `json:"rewardKey"`
	RewardPoints int    `json:"rewardPoints"`
	RewardStatus string `json:"rewardStatus"` // granting / granted
	// Granted = 已经发放完成（无需再调 Java / mark）
	Granted bool `json:"granted"`
}

// ClaimReward 占位奖励发放（两阶段第一阶段，管理端在调 Java 积分接口之前调；幂等，重试返回同一份点数快照）。
// 判据（拒收均 409 且带 reason）：不是已通过 → SHARE_NOT_APPROVED；奖励已关闭 → REWARD_DISABLED；本月（发放 + 发放中）已达上限 → REWARD_MONTHLY_LIMIT。
// 已是发放中 / 已发放的重复调用不再看开关与上限（钱可能已在路上）。
func (s *Service) ClaimReward(ctx context.Context, id, adminID int64) (*RewardClaim, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	rs := s.rewardSettings(ctx)
	now := s.clock()
	from, to := monthBounds(now)
	sh, outcome, err := ss.ClaimShareReward(ctx, id, repo.ClaimParams{Enabled: rs.Enabled, Points: rs.Points, Limit: rs.MonthlyLimit, From: from, To: to, Now: now, AdminID: adminID})
	if err != nil {
		return nil, err
	}
	switch outcome {
	case repo.ClaimNotFound:
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	case repo.ClaimNotApproved:
		return nil, xerr.NewReason(xerr.Conflict, ReasonShareNotApproved, "这条投稿还没通过审核（或已被用户撤回），不能发放奖励")
	case repo.ClaimDisabled:
		logx.WithContext(ctx).Infof("replicate-admin: 奖励占位拒收 判据=REWARD_DISABLED share=%d 分享奖励已在后台关闭", id)
		return nil, xerr.NewReason(xerr.Conflict, ReasonRewardDisabled, "分享奖励已关闭，不能发放")
	case repo.ClaimLimitHit:
		logx.WithContext(ctx).Infof("replicate-admin: 奖励占位拒收 判据=REWARD_MONTHLY_LIMIT share=%d user=%d 本月已达上限 %d", id, sh.UserID, rs.MonthlyLimit)
		return nil, xerr.NewReason(xerr.Conflict, ReasonRewardMonthlyLimit, fmt.Sprintf("该用户本月已发放 %d 次奖励，达到上限", rs.MonthlyLimit))
	}
	if outcome == repo.ClaimOK {
		logx.WithContext(ctx).Infof("replicate-admin: 奖励占位 share=%d user=%d admin=%d points=%d", id, sh.UserID, adminID, sh.RewardPoints)
	}
	points := sh.RewardPoints
	if points <= 0 { // 老数据（迁移前已发放的行没有快照）：回显当前配置，仅作展示
		points = rs.Points
	}
	return &RewardClaim{RewardKey: RewardKeyPrefix + strconv.FormatInt(sh.ID, 10), RewardPoints: points, RewardStatus: sh.RewardStatus,
		Granted: sh.RewardStatus == model.RewardGranted}, nil
}

// MarkRewardGranted 标记「奖励已发放」（两阶段第三阶段，Java 积分接口成功之后调）：granting → granted，无条件——
// 积分已经发出，不再校验开关 / 月上限。已发放 → 幂等（RewardReplay=true）；没占位（none）→ 409 REWARD_NOT_CLAIMED。
func (s *Service) MarkRewardGranted(ctx context.Context, id, adminID int64) (*ShareRow, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	sh, outcome, err := ss.MarkShareRewardGranted(ctx, id, s.clock())
	if err != nil {
		return nil, err
	}
	switch outcome {
	case repo.MarkNotFound:
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	case repo.MarkNotClaimed:
		logx.WithContext(ctx).Infof("replicate-admin: 奖励标记拒收 判据=REWARD_NOT_CLAIMED share=%d 还没占位（先调 reward/claim）", id)
		return nil, xerr.NewReason(xerr.Conflict, ReasonRewardNotClaimed, "这条投稿还没占位发放，请先占位再标记")
	}
	row, _, err := s.loadShare(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	row.RewardReplay = outcome == repo.MarkAlready
	if outcome == repo.MarkDone {
		logx.WithContext(ctx).Infof("replicate-admin: 标记奖励已发放 share=%d user=%d admin=%d points=%d", id, sh.UserID, adminID, sh.RewardPoints)
	}
	return &row, nil
}

// ReleaseReward 取消占位：granting → none（幂等；本来就是 none 直接返回）。只在 Java 明确没有加点（调用没发出 / 明确不可重试的失败）时用；
// 已发放 → 409 REWARD_ALREADY_GRANTED。
func (s *Service) ReleaseReward(ctx context.Context, id, adminID int64) (*ShareRow, error) {
	ss, err := s.shareStore()
	if err != nil {
		return nil, err
	}
	sh, outcome, err := ss.ReleaseShareReward(ctx, id)
	if err != nil {
		return nil, err
	}
	switch outcome {
	case repo.ReleaseNotFound:
		return nil, xerr.New(xerr.NotFound, "找不到这条投稿")
	case repo.ReleaseGranted:
		return nil, xerr.NewReason(xerr.Conflict, ReasonRewardAlreadyGranted, "这条奖励已经发放完成，不能取消")
	}
	if outcome == repo.ReleaseDone {
		logx.WithContext(ctx).Infof("replicate-admin: 取消奖励占位 share=%d user=%d admin=%d", id, sh.UserID, adminID)
	}
	row, _, err := s.loadShare(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	return &row, nil
}
