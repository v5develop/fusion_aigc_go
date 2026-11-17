package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// 画面运动量曲线算子（视频做同款 CONTRACT §R「接缝挪到画面最静处」，add-only）：
//
//	POST /v1/mediasvc/video/motion-profile   {"source": {rid|url|key}（也接受顶层 "rid" 简写）, "fps"?: 4, "from_s"?, "to_s"?}
//	  → {"fps": 4, "from_s": 0, "duration_s": 12.5, "scores": [float...]}
//
// 只读，不落桶，不需要 Idempotency-Key（与 probe / inspect 同类）。
//
// 度量与做法（选型说明）：
//   - ffmpeg 滤镜链 fps=N → scale=96:-2（area 缩小，等于低通，压掉压缩噪点）→ format=gray → tblend=all_mode=difference（相邻两个采样帧逐像素取绝对差）
//     → signalstats → metadata=print:key=lavfi.signalstats.YAVG。YAVG 是差分图的亮度均值 = 相邻两个采样帧的「平均绝对亮度差」（MAFD），范围 0~255。
//   - 不用 select='gte(scene,0)'：scene 分值是「MAFD 的变化量」（FFMIN(mafd, |mafd - 上一帧 mafd|)），持续的大幅运动反而得低分，不能表示运动量；
//     也不用 scdet（只在镜头切换处出分，看不出平缓运动）。tblend + signalstats 直接给出每个采样间隔里画面动了多少，静止段 ≈ 0。
//   - scores[i] = 第 i-1 个与第 i 个采样帧的平均绝对差（采样时刻 from_s + i/fps，i 从 0 起）；第 0 个没有前帧，取 scores[1]（只有一个采样点时为 0）。
//     分值保留 3 位小数；纯静止 = 0，镜头硬切通常 > 30。
//
// 限制：fps 取 0.5~10（缺省 4）；from_s ≥ 0、to_s > from_s（缺省 = 全片）；采样点最多 maxMotionSamples 个。没有视频流 / 解码失败 → 空 scores（不是接口错误，
// 调用方当「没有曲线」处理，退回原来的切点规则）。来源取不到 / 超时 / 被中断照常走错误码。
const (
	motionDefaultFPS  = 4.0
	motionMinFPS      = 0.5
	motionMaxFPS      = 10.0
	maxMotionSamples  = 4000
	motionScaleFilter = "scale=96:-2:flags=area"
	motionStdoutCap   = 4 << 20
)

type videoMotionReq struct {
	Source SourceRef `json:"source"`
	Rid    string    `json:"rid,omitempty"`
	FPS    float64   `json:"fps,omitempty"`
	FromS  *float64  `json:"from_s,omitempty"`
	ToS    *float64  `json:"to_s,omitempty"`
}

// VideoMotionResult 是 video/motion-profile 的响应体。
type VideoMotionResult struct {
	FPS       float64   `json:"fps"`
	FromS     float64   `json:"from_s"`
	DurationS float64   `json:"duration_s"`
	Scores    []float64 `json:"scores"`
}

// motionFilter 滤镜链（fps 取整后的值写进去，避免 locale 小数点问题）。
func motionFilter(fps float64) string {
	return "fps=" + strconv.FormatFloat(fps, 'f', -1, 64) + "," + motionScaleFilter +
		",format=gray,tblend=all_mode=difference,signalstats,metadata=print:key=lavfi.signalstats.YAVG:file=-"
}

// parseMotionScores 解析 metadata=print 的输出，按出现顺序取 YAVG；结果在最前面补一个与第一项相同的值（第 0 个采样点没有前帧）。
func parseMotionScores(out []byte) []float64 {
	var diffs []float64
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "lavfi.signalstats.YAVG="); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				f = 0
			}
			diffs = append(diffs, math.Round(f*1000)/1000)
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	return append([]float64{diffs[0]}, diffs...)
}

// MotionProfile 计算 [from, to) 的运动量曲线。to<=0 表示到片尾。
func (s *Service) MotionProfile(ctx context.Context, file string, fps, from, to float64) (VideoMotionResult, error) {
	res := VideoMotionResult{FPS: fps, FromS: from, Scores: []float64{}}
	probe, err := s.Probe(ctx, file)
	if err != nil {
		if isBadInput(err) {
			return res, nil
		}
		return VideoMotionResult{}, err
	}
	if !probe.HasVideo {
		return res, nil
	}
	end := probe.DurationSeconds
	if to > 0 && (end <= 0 || to < end) {
		end = to
	}
	if end > 0 {
		if end <= from {
			return res, nil
		}
		if n := (end - from) * fps; n > maxMotionSamples {
			return VideoMotionResult{}, invalid("request.motion_too_long")
		}
		res.DurationS = end - from
	}
	args := []string{"-nostdin", "-nostats", "-v", "error"}
	if from > 0 {
		args = append(args, "-ss", strconv.FormatFloat(from, 'f', 3, 64))
	}
	if to > 0 {
		args = append(args, "-t", strconv.FormatFloat(end-from, 'f', 3, 64))
	}
	args = append(args, "-i", file, "-map", "0:v:0", "-vf", motionFilter(fps), "-an", "-f", "null", "-")
	var stdout, stderr cappedBuffer
	stdout.limit, stderr.limit = motionStdoutCap, inspectStderrCap
	runErr := s.compute(ctx, func() error {
		cmd := exec.CommandContext(ctx, s.ffmpeg(), args...)
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		return cmd.Run()
	})
	if runErr != nil {
		if e := interruptedFailure(ctx, runErr, "运动量曲线"); e != nil {
			return VideoMotionResult{}, e
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			return VideoMotionResult{}, internal(runErr)
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			log.Printf("media 运动量曲线解码失败 trace_id=%s: %v: %s", TraceIDFromContext(ctx), runErr, tailOf(stderr.String()))
			return res, nil
		}
		return VideoMotionResult{}, internal(runErr)
	}
	if sc := parseMotionScores(stdout.Bytes()); sc != nil {
		res.Scores = sc
	}
	if len(res.Scores) > maxMotionSamples+1 {
		res.Scores = res.Scores[:maxMotionSamples+1]
	}
	if res.DurationS <= 0 && len(res.Scores) > 0 {
		res.DurationS = float64(len(res.Scores)) / fps
	}
	return res, nil
}

func checkMotionReq(req *videoMotionReq) error {
	if req.Rid != "" {
		if req.Source.Rid != "" || req.Source.Url != "" || req.Source.Key != "" {
			return invalid("request.source_conflict")
		}
		req.Source = SourceRef{Rid: req.Rid}
	}
	if req.FPS == 0 {
		req.FPS = motionDefaultFPS
	}
	if math.IsNaN(req.FPS) || req.FPS < motionMinFPS || req.FPS > motionMaxFPS {
		return invalid("request.motion_fps")
	}
	from, to := 0.0, 0.0
	if req.FromS != nil {
		from = *req.FromS
	}
	if req.ToS != nil {
		to = *req.ToS
	}
	if math.IsNaN(from) || math.IsNaN(to) || math.IsInf(from, 0) || math.IsInf(to, 0) || from < 0 || (req.ToS != nil && to <= from) {
		return invalid("request.motion_range")
	}
	return checkSource(req.Source)
}

func (s *Server) videoMotionProfile(w http.ResponseWriter, r *http.Request) {
	var req videoMotionReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkMotionReq(&req); err != nil {
		writeAPIError(w, err)
		return
	}
	from, to := 0.0, 0.0
	if req.FromS != nil {
		from = *req.FromS
	}
	if req.ToS != nil {
		to = *req.ToS
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		res, err := s.svc.MotionProfile(ctx, path, req.FPS, from, to)
		logStage(ctx, "motion_profile", start)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
}
