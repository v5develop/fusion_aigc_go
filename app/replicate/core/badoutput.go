package core

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 坏片判定（CONTRACT §J4）。
//
// 位置：段视频已由网关落桶（DELIVERING，有 videoRid）、确认扣点之前（deliver 入口）。命中 → 该段步骤置失败 BAD_OUTPUT、
// 退回这一段的预留（用户没有被收费）、不走 #r1 自动重试（Transient=false：重试只会再花平台的钱）；是否重做由用户付费决定
// （不做「系统判定不合格即免费重做」，见 DECISIONS）。精修步骤命中同样按失败：链尾失败 + 更早有成功步骤，segResult 仍给旧视频。
//
// 判据（媒体服务 video/inspect，整段 blackdetect + freezedetect + 完整解码）：
//   - 解不开（容器损坏 / 完整解码报错）；
//   - 黑屏占时长比例 ≥ BadOutputBlackRatio（默认 0.6）；
//   - 定格占时长比例 ≥ BadOutputFreezeRatio（默认 0.8）。
//
// fail-open：检测器自己不可用（没装配媒体服务、调用报错、超时）一律放行，当作成功继续确认扣点——供应商已经收了钱，
// 产物多半是好的，不能因为我们自己的检测服务故障让用户的好片被判失败。放行时打 判据=bad_output_check_unavailable 供排查。

const (
	// FailBadOutput 出片成功但画面是坏的（黑屏 / 定格 / 解不开）。
	FailBadOutput = "BAD_OUTPUT"
	// badOutputCheckTimeout 单次检测的上限：整段解码，80 秒内的视频通常几秒；超时按「检测不可用」放行。
	badOutputCheckTimeout = 90 * time.Second
)

// badOutputVerdict 按阈值判定。返回是否坏片与命中的判据说明（日志用，不给用户）。
func badOutputVerdict(r mediasvc.VideoInspect, blackMax, freezeMax float64) (bool, string) {
	switch {
	case !r.Decodable:
		return true, "解不开（文件损坏）"
	case r.BlackRatio >= blackMax:
		return true, fmt.Sprintf("黑屏占比 %.2f ≥ %.2f", r.BlackRatio, blackMax)
	case r.FreezeRatio >= freezeMax:
		return true, fmt.Sprintf("定格占比 %.2f ≥ %.2f", r.FreezeRatio, freezeMax)
	}
	return false, ""
}

// checkBadOutput 对步骤产物做坏片检查。返回 true = 判坏片（调用方负责失败 + 退点）；检测不可用返回 false（放行）。
func (s *Service) checkBadOutput(ctx context.Context, j *model.Job, st *model.Step) bool {
	if s.cfg.BadOutputOff || s.media == nil {
		return false
	}
	var out struct {
		VideoRid string `json:"videoRid"`
		CostCNY  string `json:"costCny"`
	}
	if json.Unmarshal(st.Output, &out) != nil || out.VideoRid == "" {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, badOutputCheckTimeout)
	defer cancel()
	res, err := s.media.InspectVideo(cctx, out.VideoRid)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 坏片检测不可用，放行 判据=bad_output_check_unavailable job=%d step=%s rid=%s: %v", j.ID, st.StepKey, out.VideoRid, err)
		return false
	}
	bad, why := badOutputVerdict(res, s.cfg.BadOutputBlackRatio, s.cfg.BadOutputFreezeRatio)
	if !bad {
		return false
	}
	logx.WithContext(ctx).Errorf("replicate: 坏片 判据=bad_output job=%d step=%s rid=%s 原因=%s 时长=%.1fs 平台承担成本=%s",
		j.ID, st.StepKey, out.VideoRid, why, res.DurationS, out.CostCNY)
	return true
}
