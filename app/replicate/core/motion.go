package core

import (
	"context"
	"encoding/base64"
	"math"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/segment"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 接缝挪到画面最静处（CONTRACT §R）：分析阶段向媒体服务要整条原片的运动量曲线（video/motion-profile，4 fps），
// 压缩后存进分析状态（AnalysisState.Motion，供这条原片的所有方案复用，出方案时不再调媒体服务）；
// 分段（rules/segment.PlanWithMotion）在没有镜头切换 / 停顿切点时，在允许窗口里选运动量最小处。
// 全程 fail-open：没装配 / 关闭 / 单段视频 / 媒体服务出错 → 没有曲线，分段照旧均分，不影响分析。
// ---------------------------------------------------------------------------------------------

const (
	motionFPS     = 4.0
	motionWait    = 60 * time.Second
	motionQuantum = 8.0 // 量化：uint8 = round(分值 × 8)，分辨率 1/8（分值是 0~255 的平均绝对亮度差），32 以上饱和（已是剧烈运动，不影响找最小值）
)

// MotionState 压缩后的运动量曲线：FPS + 每个采样点一个字节（base64）。80 秒的片子约 430 字符。
type MotionState struct {
	FPS float64 `json:"fps"`
	Q   string  `json:"q"`
}

func quantizeMotion(scores []float64) string {
	b := make([]byte, len(scores))
	for i, v := range scores {
		switch {
		case math.IsNaN(v) || v < 0:
			v = 0
		case v*motionQuantum > 255:
			v = 255 / motionQuantum
		}
		b[i] = byte(math.Round(v * motionQuantum))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// toSegment 还原成分段用的曲线；nil / 解不开 = nil（退回原规则）。
func (m *MotionState) toSegment() *segment.Motion {
	if m == nil || m.FPS <= 0 || m.Q == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(m.Q)
	if err != nil || len(b) < 2 {
		return nil
	}
	sc := make([]float64, len(b))
	for i, v := range b {
		sc[i] = float64(v) / motionQuantum
	}
	return &segment.Motion{FPS: m.FPS, Scores: sc}
}

// computeMotion 分析阶段算一次整条原片的运动量曲线。只有需要拆成多段的视频（时长 > 单段上限）才算：单段没有接缝。
func (s *Service) computeMotion(ctx context.Context, a *model.Analysis, st *AnalysisState) {
	if s.cfg.MotionOff || st.Motion != nil || st.Probe == nil || st.Probe.DurationS <= float64(s.cfg.Limits.SegCapSec)+1e-6 {
		return
	}
	mp, ok := s.media.(MotionProfiler)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, motionWait)
	defer cancel()
	res, err := mp.VideoMotionProfile(cctx, deref(a.SourceRid), motionFPS, 0, 0)
	if err != nil || len(res.Scores) < 2 || res.FPS <= 0 {
		st.Warnings = appendOnce(st.Warnings, "motion_degraded") // 含下划线：内部降级标记，不给用户看
		logx.WithContext(ctx).Infof("replicate: 运动量曲线不可用，分段退回原规则 判据=MOTION_PROFILE_UNAVAILABLE analysis=%s 采样=%d: %v", a.AnalysisNo, len(res.Scores), err)
		return
	}
	// 采样点上限与服务端一致（4000）；超长的曲线不存（分段窗口只在 80 秒内）
	st.Motion = &MotionState{FPS: res.FPS, Q: quantizeMotion(res.Scores)}
}
