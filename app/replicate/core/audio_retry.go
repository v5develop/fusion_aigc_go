package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 免费重做声音（CONTRACT §Q）：result.audio = failed 的已交付成片，在现有成片画面上只重跑声音这一步。
//
//   - keep：把原片对应区间的原音轨重新贴回成片；revoice：重新合成配音再替换音轨（TTS 幂等键由「任务、句号、音色、语速、文字」决定，
//     网关回放同一次调用；不再占换声音额度——首次失败时额度已退回）。不重新生成画面，不动账。
//   - 成功：成片换新版本。结果文档 videoRid / audio 更新，成片入素材库的发件箱版本 = 精修代数 + 1000 × AudioGen（见 finalVersion），
//     旧版本的入库行保留；声音没变化（原片本来没声音，audio=none）不换版本。
//   - 每个成片最多重做 3 次（认领时 +1，成功失败都算）；状态全部存在 result_document 里（AudioRetries / AudioRetryCall / AudioClaim），
//     用 SwapResultDoc 整份 CAS 认领与落结果，不另建表；合成依据变了（某段重做成功）整份结果重建，次数随之清零。
//   - 幂等：同一个 call_id 重放返回上次处理的结果、不再重做也不再计次；并发 / 占位未过期 → IN_PROGRESS。
// ---------------------------------------------------------------------------------------------

const (
	// maxAudioRetries 同一成片最多免费重做几次声音。
	maxAudioRetries = 3
	// audioClaimStale 重做声音占位的回收时限：须大于一次重做的最坏耗时（逐句 TTS + 铺音轨 + 贴回）。
	audioClaimStale = 10 * time.Minute

	// ReasonAudioNotFailed 成片的声音没有失败（已成功 / 原片没声音 / 没选声音处理），没有可重做的。
	ReasonAudioNotFailed = "AUDIO_NOT_FAILED"
	// ReasonAudioRetryLimit 这条成片的免费重做次数已用完。
	ReasonAudioRetryLimit = "AUDIO_RETRY_LIMIT"

	// 发件箱里成片版本 = 精修代数 + audioVersionStep × AudioGen（精修代数不会到 1000，不冲突）。
	audioVersionStep = 1000
)

// audioClaim 重做声音的占位（存在结果文档里）。
type audioClaim struct {
	CallID string    `json:"callId,omitempty"`
	At     time.Time `json:"at"`
}

// finalVersion 成片入素材库发件箱的版本：精修代数 + 1000 × 声音重做换成片的次数。
func finalVersion(epoch int, doc *ResultDoc) int {
	if doc == nil {
		return epoch
	}
	return epoch + audioVersionStep*doc.AudioGen
}

// AudioRetryView replicate_retry_audio 的结果（免费重做声音）。
type AudioRetryView struct {
	JobID int64 `json:"jobId"`
	// Audio 重做后的声音状态：kept / revoiced / none（原片没有声音）/ failed（这次仍没成功）
	Audio     string `json:"audio"`
	AudioNote string `json:"audioNote,omitempty"`
	// Changed true = 成片换成了新版本（videoUrl 是新成片）
	Changed bool `json:"changed"`
	// RetriesLeft 这条成片还剩几次免费重做
	RetriesLeft int `json:"retriesLeft"`
	// CanRetryAudio 还能再重做（audio 仍是 failed 且有剩余次数）
	CanRetryAudio bool         `json:"canRetryAudio,omitempty"`
	VideoURL      string       `json:"videoUrl,omitempty"`
	URLExpiresAt  *time.Time   `json:"urlExpiresAt,omitempty"`
	Library       *LibraryView `json:"library,omitempty"`
	// Replay true = 同一个 call_id 已处理过，这次直接返回当时的结果（没有重做、没有计次）。add-only。
	Replay bool `json:"replay,omitempty"`
}

// audioRetryEnabled 合成器实现了贴回算子才开放（换声音另需 voiceMixer，缺则该次重做按失败处理）。
func (s *Service) audioRetryEnabled() bool {
	if s.composer == nil {
		return false
	}
	_, ok := s.composer.(AudioMuxer)
	return ok
}

// RetryAudio 免费重做成片的声音。callID 是调用方的幂等键（工具的 call_id），空串不做重放识别。
func (s *Service) RetryAudio(ctx context.Context, userID, jobID int64, callID string) (*AudioRetryView, error) {
	callID = strings.TrimSpace(callID)
	if !s.audioRetryEnabled() {
		return nil, xerr.New(xerr.NotReady, "重做声音暂未开放")
	}
	j, doc, err := s.deliveredFinal(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	if c := doc.AudioClaim; c != nil && s.now().Sub(c.At) < audioClaimStale {
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "声音正在重做，几十秒后再看一下")
	}
	if callID != "" && doc.AudioRetryCall == callID {
		return s.audioRetryView(ctx, j, doc, false, true), nil // 同 call_id 重放
	}
	if doc.Audio != AudioResultFailed {
		return nil, xerr.NewReason(xerr.Conflict, ReasonAudioNotFailed, "这条成片的声音没有出问题，不需要重做")
	}
	if doc.AudioRetries >= maxAudioRetries {
		return nil, xerr.NewReason(xerr.Conflict, ReasonAudioRetryLimit, fmt.Sprintf("这条成片已经免费重做过 %d 次声音了，次数用完了", maxAudioRetries))
	}
	var in JobInput
	if err := json.Unmarshal(j.Input, &in); err != nil || len(in.Segments) == 0 {
		return nil, fmt.Errorf("任务输入读取失败: %v", err)
	}
	steps, err := s.store.ListSteps(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	outs := s.outcomes(j, &in, steps)
	if basisOf(outs, in.Texts, strings.TrimSpace(s.cfg.FontRid)) != doc.Basis {
		// 有段刚重做成功、成片还没按新依据合成完：等新成片出来再重做声音（新成片会重新处理一遍声音）
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "成片正在更新，更新完再重做声音")
	}
	// 认领：整份结果 CAS（AudioRetries +1，占位）。别人先占了 / 读到的结果已变 = 没认到
	claimed := *doc
	claimed.AudioRetries++
	claimed.AudioClaim = &audioClaim{CallID: callID, At: s.now()}
	claimedRaw, _ := json.Marshal(claimed)
	if ok, err := s.store.SwapResultDoc(ctx, j.ID, j.ResultDocument, claimedRaw); err != nil {
		return nil, err
	} else if !ok {
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "声音正在重做，几十秒后再看一下")
	}

	idem := func(step string) string {
		return jobKey(in.KeyPrefix, j.ID, fmt.Sprintf("final/%s/audio-retry/%d/%s", doc.Basis, claimed.AudioRetries, step))
	}
	scratch := ResultDoc{AudioKind: doc.AudioKind}
	var newRid string
	switch in.Audio {
	case AudioKeep:
		newRid = s.keepOriginalAudio(ctx, j, &in, keepRanges(outs), doc.VideoRid, idem("mux"), &scratch)
	case AudioRevoice:
		scratch.AudioKind = AudioRevoice
		rid, why, rerr := s.revoiceTrack(ctx, j, &in, outs, doc.VideoRid, idem, &scratch)
		if rerr != nil {
			scratch.Audio, newRid = AudioResultFailed, doc.VideoRid
			logx.WithContext(ctx).Errorf("replicate: 重做声音失败 判据=revoice_retry_failed job=%d 第 %d 次 原因=%s: %v", j.ID, claimed.AudioRetries, why, rerr)
		} else {
			scratch.Audio, newRid = AudioResultRevoiced, rid
		}
	default:
		scratch.Audio, newRid = AudioResultFailed, doc.VideoRid
	}

	fin := claimed
	fin.AudioClaim, fin.AudioRetryCall = nil, callID
	fin.Audio = scratch.Audio
	fin.VoiceTrimmed, fin.Warnings = scratch.VoiceTrimmed, mergeWarnings(doc.Warnings, scratch.Warnings)
	changed := scratch.Audio != AudioResultFailed && newRid != "" && newRid != doc.VideoRid
	if changed {
		fin.VideoRid = newRid
		fin.AudioGen++
	}
	finRaw, _ := json.Marshal(fin)
	if ok, err := s.store.SwapResultDoc(context.WithoutCancel(ctx), j.ID, claimedRaw, finRaw); err != nil {
		return nil, err
	} else if !ok {
		// 占位过期被别的调用接手了：本次结果作废（媒体幂等键 + 网关回放，下次重做不会重复花钱）
		logx.WithContext(ctx).Errorf("replicate: 重做声音结果落库失败，占位已被接手 判据=AUDIO_RETRY_SWAP_FAILED job=%d", j.ID)
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "声音正在重做，几十秒后再看一下")
	}
	if changed {
		// 新成片入素材库（版本 = 精修代数 + 1000 × AudioGen，旧版本的入库行保留）；入队失败只打日志：下一次 finalize 扫描会按已有结果补入队
		if err := s.enqueueOnJobDone(ctx, j, &fin); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 重做声音后新成片入库入队失败（稍后补） 判据=AUDIO_RETRY_ENQUEUE_FAILED job=%d: %v", j.ID, err)
		}
		if fin.Audio == AudioResultRevoiced {
			s.funnel(ctx, funnelEvent{Stage: StageRevoice, UserID: j.UserID, PlanID: j.PlanID, JobID: j.ID, Dedupe: fmt.Sprintf("rv:%d", j.ID)})
		}
	}
	s.funnel(ctx, funnelEvent{Stage: StageRetryAudio, UserID: j.UserID, PlanID: j.PlanID, JobID: j.ID, Dedupe: fmt.Sprintf("ra:%d:%d", j.ID, fin.AudioRetries)})
	return s.audioRetryView(ctx, j, &fin, changed, false), nil
}

func mergeWarnings(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, w := range b {
		out = appendOnce(out, w)
	}
	return out
}

func (s *Service) audioRetryView(ctx context.Context, j *model.Job, doc *ResultDoc, changed, replay bool) *AudioRetryView {
	v := &AudioRetryView{JobID: j.ID, Audio: doc.Audio, Changed: changed, Replay: replay, RetriesLeft: max(0, maxAudioRetries-doc.AudioRetries)}
	rv := resultView(mustMarshal(doc))
	if rv != nil {
		v.AudioNote, v.CanRetryAudio = rv.AudioNote, rv.CanRetryAudio
	}
	if urls, exp := s.signRIDs(ctx, []string{doc.VideoRid}); urls != nil {
		v.VideoURL, v.URLExpiresAt = urls[doc.VideoRid], exp
	}
	v.Library = s.exportLibrary(ctx, j.ID, func(d *model.Delivery) bool { return d.Kind == model.DeliveryFinal && d.SrcRid == doc.VideoRid })
	return v
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
