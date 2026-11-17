package media

import (
	"bytes"
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// 坏片检测算子（视频做同款 J4）：对一段视频整段跑 ffmpeg blackdetect + freezedetect，报告黑屏占比、定格占比、能否解码。
//
//	POST /v1/mediasvc/video/inspect   {"source": {rid|url|key}}   只读，不落桶，不需要 Idempotency-Key（与 probe 同类）
//
// 响应 {decodable, duration_s, black_ratio, freeze_ratio, black_seconds, freeze_seconds}：
//   - 解不开（容器损坏 / 完整解码报错）不是接口错误，而是判定结果：decodable=false，两个占比为 0，由调用方按坏片处理；
//   - 来源取不到、鉴权、超时、被中断等照常走错误码（调用方应当把它当「检测不可用」而不是「坏片」）。
// 占比 = 命中区间合并去重后的总秒数 / 视频时长，范围 0~1。命中区间若在片尾仍未结束（ffmpeg 不一定补发 end），按到片尾计算。

const (
	// blackdetect：d 为最短黑屏时长；pix_th 像素亮度阈值（0.10 约等于 YUV 亮度 <= 26，涵盖压缩后的「近黑」）；pic_th 画面里黑像素占比阈值。
	inspectBlackFilter = "blackdetect=d=0.2:pix_th=0.10:pic_th=0.98"
	// freezedetect：n 为相邻帧差异的噪声容忍（-55dB，容得下压缩噪点）；d 为最短定格时长。
	inspectFreezeFilter = "freezedetect=n=-55dB:d=0.5"
	inspectStderrCap    = 4 << 20
)

type videoInspectReq struct {
	Source SourceRef `json:"source"`
}

// VideoInspectResult 是 video/inspect 的响应体（replicate 的媒体客户端按同名 JSON 字段解析）。
type VideoInspectResult struct {
	Decodable     bool    `json:"decodable"`
	DurationS     float64 `json:"duration_s"`
	BlackRatio    float64 `json:"black_ratio"`
	FreezeRatio   float64 `json:"freeze_ratio"`
	BlackSeconds  float64 `json:"black_seconds"`
	FreezeSeconds float64 `json:"freeze_seconds"`
}

var (
	reBlackLine   = regexp.MustCompile(`black_start:([0-9.]+)\s+black_end:([0-9.]+)`)
	reFreezeStart = regexp.MustCompile(`freeze_start:\s*([0-9.]+)`)
	reFreezeEnd   = regexp.MustCompile(`freeze_end:\s*([0-9.]+)`)
)

type span struct{ start, end float64 }

// parseBlackSpans 解析 blackdetect 的输出行（每个黑屏区间一行，含起止）。
func parseBlackSpans(stderr []byte) []span {
	var out []span
	for _, m := range reBlackLine.FindAllSubmatch(stderr, -1) {
		a, e1 := strconv.ParseFloat(string(m[1]), 64)
		b, e2 := strconv.ParseFloat(string(m[2]), 64)
		if e1 == nil && e2 == nil && b > a {
			out = append(out, span{a, b})
		}
	}
	return out
}

// parseFreezeSpans 解析 freezedetect 的 start / end；片尾仍在定格时可能只有 start，此时 end 记为 duration。
func parseFreezeSpans(stderr []byte, duration float64) []span {
	starts := reFreezeStart.FindAllSubmatch(stderr, -1)
	ends := reFreezeEnd.FindAllSubmatch(stderr, -1)
	var out []span
	for i, sm := range starts {
		a, err := strconv.ParseFloat(string(sm[1]), 64)
		if err != nil {
			continue
		}
		b := duration
		if i < len(ends) {
			if v, err := strconv.ParseFloat(string(ends[i][1]), 64); err == nil {
				b = v
			}
		}
		if b > a {
			out = append(out, span{a, b})
		}
	}
	return out
}

// coveredSeconds 合并重叠区间后的总秒数，并裁剪到 [0, limit]（limit<=0 表示不裁剪）。
func coveredSeconds(spans []span, limit float64) float64 {
	if len(spans) == 0 {
		return 0
	}
	cp := append([]span(nil), spans...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].start < cp[j].start })
	total, curS, curE := 0.0, math.Max(cp[0].start, 0), cp[0].end
	flush := func() {
		e := curE
		if limit > 0 && e > limit {
			e = limit
		}
		if e > curS {
			total += e - curS
		}
	}
	for _, sp := range cp[1:] {
		if sp.start <= curE {
			curE = math.Max(curE, sp.end)
			continue
		}
		flush()
		curS, curE = math.Max(sp.start, 0), sp.end
	}
	flush()
	return total
}

// buildInspectResult 由时长与两类区间算出占比；时长未知时以最后一个区间的末端兜底。
func buildInspectResult(duration float64, black, freeze []span) VideoInspectResult {
	if duration <= 0 {
		for _, sp := range append(append([]span(nil), black...), freeze...) {
			duration = math.Max(duration, sp.end)
		}
	}
	res := VideoInspectResult{Decodable: true, DurationS: duration}
	res.BlackSeconds = coveredSeconds(black, duration)
	res.FreezeSeconds = coveredSeconds(freeze, duration)
	if duration > 0 {
		res.BlackRatio = math.Min(1, res.BlackSeconds/duration)
		res.FreezeRatio = math.Min(1, res.FreezeSeconds/duration)
	}
	return res
}

type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil // 超出上限的丢弃，但不让 ffmpeg 因写失败而中止
}

// InspectVideo 整段解码一遍并做黑屏 / 定格检测。解码失败返回 decodable=false（不是错误）；被中断、超时、找不到 ffmpeg 返回错误。
func (s *Service) InspectVideo(ctx context.Context, file string) (VideoInspectResult, error) {
	probe, err := s.Probe(ctx, file)
	if err != nil {
		if isBadInput(err) {
			return VideoInspectResult{Decodable: false}, nil
		}
		return VideoInspectResult{}, err
	}
	if !probe.HasVideo {
		return VideoInspectResult{Decodable: false, DurationS: probe.DurationSeconds}, nil
	}
	var stderr cappedBuffer
	stderr.limit = inspectStderrCap
	runErr := s.compute(ctx, func() error {
		cmd := exec.CommandContext(ctx, s.ffmpeg(), "-nostdin", "-nostats", "-v", "info", "-i", file,
			"-map", "0:v:0", "-vf", inspectBlackFilter+","+inspectFreezeFilter, "-an", "-f", "null", "-")
		cmd.Stderr = &stderr
		return cmd.Run()
	})
	if runErr != nil {
		if e := interruptedFailure(ctx, runErr, "坏片检测"); e != nil {
			return VideoInspectResult{}, e
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			return VideoInspectResult{}, internal(runErr)
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			log.Printf("media 坏片检测解码失败 trace_id=%s: %v: %s", TraceIDFromContext(ctx), runErr, tailOf(stderr.String()))
			return VideoInspectResult{Decodable: false, DurationS: probe.DurationSeconds}, nil
		}
		return VideoInspectResult{}, internal(runErr)
	}
	raw := stderr.Bytes()
	return buildInspectResult(probe.DurationSeconds, parseBlackSpans(raw), parseFreezeSpans(raw, probe.DurationSeconds)), nil
}

func isBadInput(err error) bool {
	var oe *opError
	return errors.As(err, &oe) && oe.kind == kindBadInput
}

func (s *Server) videoInspect(w http.ResponseWriter, r *http.Request) {
	var req videoInspectReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkSource(req.Source); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		start := time.Now()
		res, err := s.svc.InspectVideo(ctx, path)
		logStage(ctx, "inspect", start)
		if err != nil {
			return nil, err
		}
		return res, nil
	})
}
