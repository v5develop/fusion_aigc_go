package media

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
)

// 视频做同款「多比例导出」的改画幅算子（add-only）：
//
//	POST /v1/mediasvc/video/reframe   {"source": {rid}, "ratio": "9:16", "mode": "pad|crop", "purpose"}
//
// 把成片改成目标画幅，得到一条新视频（新 rid）：
//   - ratio：9:16 / 1:1 / 16:9 / 3:4 / 4:3（只收这五种，别的 400 request.ratio）；
//   - mode=pad（缺省）：原画面等比缩放、完整居中，空出来的上下 / 左右用「同一画面铺满再高斯模糊」的底铺满，不出黑边；
//   - mode=crop：把画面铺满目标画幅后居中裁切，不留底；
//   - 分辨率档位：输出长边 = 源长边（上限 3840），短边按比例算，宽高都取偶数；
//   - 音轨原样拷贝（-c:a copy，不重编码）；源没有音轨则成片也没有；
//   - 帧率沿用源；视频重编码为 H.264 / yuv420p。
//
// 输入只接受 rid；写操作带 Idempotency-Key。判据（check）：
//
//	request.ratio   不是支持的画幅    400 invalid_request
//	request.mode    不是 pad / crop   400 invalid_request
//	video.*         同 video/cut（无视频流 / 超时长 / 分辨率超限）；时长上限放宽到成片可能的长度（见 reframeMaxSeconds）
//
// 响应 = MediaResult 平铺 + ratio / mode（回显）。

// reframeMaxSeconds 成片时长上限：与拼接上限一致（源 80 秒 + 拼接重叠窗口 + 容器取整误差）。
const reframeMaxSeconds = replicateMaxSeconds + replicateConcatSlack + replicateDurationSlack

// reframeRatios 支持的目标画幅：宽 : 高。
var reframeRatios = map[string][2]int{
	"9:16": {9, 16}, "1:1": {1, 1}, "16:9": {16, 9}, "3:4": {3, 4}, "4:3": {4, 3},
}

type videoReframeReq struct {
	Source  SourceRef `json:"source"`
	Ratio   string    `json:"ratio"`
	Mode    string    `json:"mode"`
	Purpose string    `json:"purpose"`
}

type reframeResp struct {
	MediaResult
	Ratio string `json:"ratio"`
	Mode  string `json:"mode"`
}

func checkReframeReq(req videoReframeReq) error {
	if _, ok := reframeRatios[req.Ratio]; !ok {
		return invalid("request.ratio")
	}
	if req.Mode != "" && req.Mode != "pad" && req.Mode != "crop" {
		return invalid("request.mode")
	}
	return nil
}

func evenFloor(v int) int { return max(2, v/2*2) }

// reframeSize 输出尺寸：长边 = 源长边（封顶 3840），短边按目标比例，宽高取偶数。
func reframeSize(srcW, srcH int, ratio [2]int) (int, int) {
	long := min(max(srcW, srcH), replicateMaxOutSide)
	long = evenFloor(long)
	rw, rh := ratio[0], ratio[1]
	switch {
	case rw == rh:
		return long, long
	case rw > rh:
		return long, evenFloor(int(math.Round(float64(long) * float64(rh) / float64(rw))))
	default:
		return evenFloor(int(math.Round(float64(long) * float64(rw) / float64(rh)))), long
	}
}

// reframeFilter 生成视频滤镜：
//
//	pad  ：split → 底（铺满目标 + 裁切 + 缩小模糊再放大，省算力）与前景（等比缩进目标内）→ 前景居中叠在底上
//	crop ：铺满目标后居中裁切
func reframeFilter(mode string, w, h int) string {
	if mode == "crop" {
		return fmt.Sprintf("[0:v:0]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,format=yuv420p[vout]", w, h, w, h)
	}
	bw, bh := evenFloor(w/8), evenFloor(h/8) // 底只要糊成色块，缩小到 1/8 再模糊，4K 也不慢
	return fmt.Sprintf("[0:v:0]split=2[bgsrc][fgsrc];"+
		"[bgsrc]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,scale=%d:%d,gblur=sigma=8,scale=%d:%d,setsar=1[bg];"+
		"[fgsrc]scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1[fg];"+
		"[bg][fg]overlay=(W-w)/2:(H-h)/2,format=yuv420p[vout]",
		w, h, w, h, bw, bh, w, h, w, h)
}

func (s *Server) videoReframe(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req videoReframeReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkRidSource(req.Source), checkPurpose(req.Purpose), checkReframeReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Mode == "" {
		req.Mode = "pad"
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		src, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		p, err := s.probeReframeVideo(ctx, src)
		if err != nil {
			return nil, err
		}
		ow, oh := reframeSize(p.Width, p.Height, reframeRatios[req.Ratio])
		out := workFile(dir, "reframed.mp4")
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-i", src,
			"-filter_complex", reframeFilter(req.Mode, ow, oh), "-map", "[vout]", "-map", "0:a:0?"}
		if p.FPS >= 1 && p.FPS <= replicateMaxFPS {
			args = append(args, "-r", strconv.FormatFloat(p.FPS, 'f', 3, 64))
		}
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-c:a", "copy", "-movflags", "+faststart", out)
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.reframe_failed")
		}
		res, err := s.storeFile(ctx, req.Purpose, out)
		if err != nil {
			return nil, err
		}
		return reframeResp{MediaResult: res, Ratio: req.Ratio, Mode: req.Mode}, nil
	})
}

// probeReframeVideo 同 probeReplicateVideo，时长上限放宽到成片长度。
func (s *Server) probeReframeVideo(ctx context.Context, path string) (ProbeResult, error) {
	p, err := s.svc.Probe(ctx, path)
	if err != nil {
		return ProbeResult{}, err
	}
	switch {
	case !p.HasVideo || p.Width < 1 || p.Height < 1:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.no_video_stream")
	case p.DurationSeconds <= 0:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.duration_unknown")
	case p.DurationSeconds > reframeMaxSeconds:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.duration_exceeded")
	case int64(p.Width)*int64(p.Height) > replicateMaxPixels || max(p.Width, p.Height) > replicateMaxSide:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.resolution_exceeded")
	}
	return p, nil
}
