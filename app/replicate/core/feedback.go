package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：成片反馈（CONTRACT §Z）。用户对一条成片（segIndex=0）或某一段说「满意 / 不满意」，可选原因、段号、备注。
//
//   - 同一用户同一任务同一段只留一行，再次提交覆盖（改主意不算两条）；
//   - 原因枚举固定（见 FeedbackReasons），只有「不满意」才收原因；备注 ≤ 200 字，过文本审核（同精修备注口径）；
//   - 记录任务当时的画面连贯度（continuity），供「连贯 vs 更快」满意度对比；
//   - 漏斗阶段 feedback_up / feedback_down（同一段同一评价只记一次，改主意换了评价才再记）；
//   - 入口（REST / 智能体）不受运行时总开关影响：暂停新提交时用户仍能给已做好的成片反馈。

// 原因枚举。
const (
	FeedbackFaceMismatch  = "face_mismatch"    // 人不像
	FeedbackProductDeform = "product_deformed" // 商品变形
	FeedbackMotionWeird   = "motion_weird"     // 动作奇怪
	FeedbackSeamJump      = "seam_jump"        // 接缝跳变
	FeedbackTextGarbled   = "text_garbled"     // 画面有乱码
	FeedbackOther         = "other"
	feedbackMaxReasons    = 6
	FeedbackNoteMaxRunes  = 200
	ReasonJobNotFinished  = "JOB_NOT_FINISHED"
	feedbackThanks        = "谢谢，已记录"
	FeedbackRatingUp      = "up"
	FeedbackRatingDown    = "down"
)

// FeedbackReasons 原因枚举（顺序即展示顺序）与用户口径文案。
var FeedbackReasons = []struct{ Code, Label string }{
	{FeedbackFaceMismatch, "人不像"}, {FeedbackProductDeform, "商品变形"}, {FeedbackMotionWeird, "动作奇怪"},
	{FeedbackSeamJump, "接缝跳变"}, {FeedbackTextGarbled, "画面有乱码"}, {FeedbackOther, "其他"},
}

func validFeedbackReason(c string) bool {
	for _, r := range FeedbackReasons {
		if r.Code == c {
			return true
		}
	}
	return false
}

// FeedbackStore 反馈读写（生产实现 app/replicate/repo；不装配时反馈回 7003）。
type FeedbackStore interface {
	// UpsertFeedback 按 (user_id, job_id, seg_index) 覆盖写；created=true 表示第一次反馈。
	UpsertFeedback(ctx context.Context, f *model.Feedback) (created bool, err error)
	GetFeedback(ctx context.Context, userID, jobID int64, segIndex int) (*model.Feedback, error)
}

// WithFeedback 装配成片反馈。不装配时 SubmitFeedback 回 7003。
func WithFeedback(f FeedbackStore) Option { return func(s *Service) { s.feedback = f } }

// FeedbackReq POST /jobs/{id}/feedback。
type FeedbackReq struct {
	UserID   int64
	JobID    int64
	Rating   string   // up / down
	Reasons  []string // 只在 down 时有效
	SegIndex int      // 0 = 整条成片
	Note     string
}

// FeedbackView 反馈回执。
type FeedbackView struct {
	JobID      int64    `json:"jobId"`
	SegIndex   int      `json:"segIndex"`
	Rating     string   `json:"rating"`
	Reasons    []string `json:"reasons"`
	Note       string   `json:"note,omitempty"`
	Continuity string   `json:"continuity"`
	// Updated true = 覆盖了之前的反馈（不是第一次）
	Updated bool `json:"updated"`
	// NoteDropped 备注没能保存（审核服务故障）；评价与原因照常记录
	NoteDropped bool   `json:"noteDropped,omitempty"`
	Message     string `json:"message"`
}

// cleanFeedbackNote 去控制字符、空白压成单个空格。
func cleanFeedbackNote(raw string) string {
	n := strings.Join(strings.FieldsFunc(raw, unicode.IsSpace), " ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n)
}

// SubmitFeedback 记录（或覆盖）本人对一条成片 / 一段的反馈。
func (s *Service) SubmitFeedback(ctx context.Context, req FeedbackReq) (*FeedbackView, error) {
	if s.feedback == nil {
		return nil, xerr.New(xerr.NotReady, "反馈功能暂未开放")
	}
	rating := strings.ToLower(strings.TrimSpace(req.Rating))
	if rating != FeedbackRatingUp && rating != FeedbackRatingDown {
		logx.WithContext(ctx).Infof("replicate: 反馈拒收 判据=FEEDBACK_RATING_INVALID user=%d job=%d 值=%q", req.UserID, req.JobID, req.Rating)
		return nil, xerr.Param("请选择 满意 / 不满意")
	}
	var reasons []string
	if rating == FeedbackRatingDown {
		seen := map[string]bool{}
		for _, r := range req.Reasons {
			r = strings.TrimSpace(r)
			if !validFeedbackReason(r) {
				logx.WithContext(ctx).Infof("replicate: 反馈拒收 判据=FEEDBACK_REASON_INVALID user=%d job=%d 值=%q", req.UserID, req.JobID, r)
				return nil, xerr.Param("不满意的原因不在可选范围内，请重新选择")
			}
			if !seen[r] {
				seen[r] = true
				reasons = append(reasons, r)
			}
		}
		if len(reasons) > feedbackMaxReasons {
			return nil, xerr.Param("原因选得太多了")
		}
	}
	note := cleanFeedbackNote(req.Note)
	if c := len([]rune(note)); c > FeedbackNoteMaxRunes {
		logx.WithContext(ctx).Infof("replicate: 反馈拒收 判据=FEEDBACK_NOTE_TOO_LONG user=%d job=%d 字数=%d 上限=%d", req.UserID, req.JobID, c, FeedbackNoteMaxRunes)
		return nil, xerr.Param(fmt.Sprintf("备注最多 %d 个字，请精简后再试", FeedbackNoteMaxRunes))
	}
	j, err := s.ownJob(ctx, req.UserID, req.JobID)
	if err != nil {
		return nil, err
	}
	if req.SegIndex < 0 || req.SegIndex > j.SegTotal {
		logx.WithContext(ctx).Infof("replicate: 反馈拒收 判据=FEEDBACK_SEG_INVALID user=%d job=%d 段=%d 总段数=%d", req.UserID, j.ID, req.SegIndex, j.SegTotal)
		return nil, xerr.Param(fmt.Sprintf("这个任务一共 %d 段，没有第 %d 段", j.SegTotal, req.SegIndex))
	}
	if j.SegDone == 0 && j.Status != model.JobSucceeded && j.Status != model.JobPartial {
		return nil, xerr.NewReason(xerr.Conflict, ReasonJobNotFinished, "成片还没做好，做好之后再来评价")
	}
	view := &FeedbackView{JobID: j.ID, SegIndex: req.SegIndex, Rating: rating, Reasons: nonNilStrings(reasons), Message: feedbackThanks}
	if note != "" {
		if err := s.moderateJobNote(ctx, req.UserID, j.ID, note, "feedback", "replicate.feedback_note"); err != nil {
			if xerr.ReasonOf(err) != ReasonModerationUnavailable {
				return nil, err // 违规：让用户改
			}
			logx.WithContext(ctx).Errorf("replicate: 反馈备注审核服务故障，只记评价不记备注 判据=FEEDBACK_NOTE_DROPPED user=%d job=%d", req.UserID, j.ID)
			note, view.NoteDropped = "", true
		}
	}
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	cont := ContinuityFast
	if in.smooth() {
		cont = ContinuitySmooth
	}
	row := &model.Feedback{UserID: req.UserID, JobID: j.ID, SegIndex: req.SegIndex, Rating: rating, Note: note, Continuity: cont}
	if len(reasons) > 0 {
		row.Reasons, _ = json.Marshal(reasons)
	}
	prev, err := s.feedback.GetFeedback(ctx, req.UserID, j.ID, req.SegIndex)
	if err != nil {
		return nil, err
	}
	created, err := s.feedback.UpsertFeedback(ctx, row)
	if err != nil {
		return nil, err
	}
	view.Note, view.Continuity, view.Updated = note, cont, !created
	stage := StageFeedbackUp
	if rating == FeedbackRatingDown {
		stage = StageFeedbackDown
	}
	if prev == nil || prev.Rating != rating { // 同一段同一评价只记一次；改主意换了评价才再记一条
		s.funnel(ctx, funnelEvent{Stage: stage, UserID: req.UserID, PlanID: j.PlanID, JobID: j.ID, SegIndex: req.SegIndex, Continuity: cont,
			Dedupe: fmt.Sprintf("fb:%d:%d:%d:%s", req.UserID, j.ID, req.SegIndex, rating)})
	}
	logx.WithContext(ctx).Infof("replicate: 成片反馈 user=%d job=%d 段=%d 评价=%s 原因=%v 连贯度=%s 覆盖=%v", req.UserID, j.ID, req.SegIndex, rating, reasons, cont, !created)
	return view, nil
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
