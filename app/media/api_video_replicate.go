package media

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 视频做同款用到的三个媒体算子（add-only，输入只接受 rid，写操作都带 Idempotency-Key）：
//   POST /v1/mediasvc/video/cut     按区间切段（重编码，起点精确到毫秒），一次最多 6 段，返回各段 rid
//   POST /v1/mediasvc/video/concat  有序多段拼接，规格统一；段可以是 rid，也可以是「占位画面」（失败段预览版用）
// 文字叠加（video/overlay-text、image/text）见 api_text_http.go。
//
// 限制与产品口径一致（DESIGN 5.17：80 秒 / 6 段）；超限一律给出可读的 check：
//   request.segments_count        段数不在 1~6                          400 invalid_request
//   request.segment_range         区间非法（0 长度、end<=start、负数、NaN） 400 invalid_request
//   request.segment_too_short     区间短于 0.1 秒                        400 invalid_request
//   video.cut_out_of_range        区间超出视频时长                       422 source_too_short
//   video.duration_exceeded       单个视频超过 80 秒                     422 source_rejected
//   video.total_duration_exceeded 拼接总时长超过 80 秒（含重叠余量）       422 source_rejected
//   video.resolution_exceeded     视频超过 3840x2160 / 单边 4096          422 source_rejected
//   video.no_video_stream         没有视频流                             422 source_rejected
//   video.duration_unknown        读不到时长                             422 source_rejected
//   request.placeholder_needs_size 全是占位又没给 width/height            400 invalid_request

const (
	replicateMaxSeconds       = 80.0
	replicateDurationSlack    = 1.0 // 容器时长的取整误差
	replicateConcatSlack      = 5.0 // 拼接时各段为衔接多留的重叠窗口，总时长额外放宽的量
	replicateMaxSegments      = 6
	replicateMaxPixels        = 3840 * 2160
	replicateMaxSide          = 4096
	replicateMinCut           = 0.1
	replicateCutEndTolerance  = 0.05
	replicateMaxFPS           = 60
	replicateDefaultFPS       = 25
	replicateDefaultSampleHz  = 44100
	replicateMinSide          = 64
	replicateMaxOutSide       = 3840
	placeholderMaxRunes       = 200
	placeholderDefaultBGColor = "#1F2937"
)

// probeReplicateVideo 探测并检查时长 / 分辨率 / 视频流。
func (s *Server) probeReplicateVideo(ctx context.Context, path string) (ProbeResult, error) {
	p, err := s.svc.Probe(ctx, path)
	if err != nil {
		return ProbeResult{}, err
	}
	switch {
	case !p.HasVideo || p.Width < 1 || p.Height < 1:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.no_video_stream")
	case p.DurationSeconds <= 0:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.duration_unknown")
	case p.DurationSeconds > replicateMaxSeconds+replicateDurationSlack:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.duration_exceeded")
	case int64(p.Width)*int64(p.Height) > replicateMaxPixels || max(p.Width, p.Height) > replicateMaxSide:
		return ProbeResult{}, newAPIError(codeSourceRejected, "video.resolution_exceeded")
	}
	return p, nil
}

func ms(v float64) float64 { return math.Round(v*1000) / 1000 }

// ---- video/cut ----

type cutRange struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type videoCutReq struct {
	Source   SourceRef  `json:"source"`
	Segments []cutRange `json:"segments"`
	Purpose  string     `json:"purpose"`
}

// cutSegment 一段结果：MediaResult 的字段平铺在外层（object / url），再带上实际的起止（秒，毫秒精度）。
type cutSegment struct {
	MediaResult
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type videoCutResp struct {
	Segments []cutSegment `json:"segments"`
}

func checkCutReq(req videoCutReq) error {
	if len(req.Segments) < 1 || len(req.Segments) > replicateMaxSegments {
		return invalid("request.segments_count")
	}
	for _, sg := range req.Segments {
		if math.IsNaN(sg.Start) || math.IsNaN(sg.End) || math.IsInf(sg.Start, 0) || math.IsInf(sg.End, 0) ||
			sg.Start < 0 || ms(sg.End) <= ms(sg.Start) {
			return invalid("request.segment_range")
		}
		if ms(sg.End)-ms(sg.Start) < replicateMinCut {
			return invalid("request.segment_too_short")
		}
		if sg.End-sg.Start > replicateMaxSeconds+replicateDurationSlack {
			return newAPIError(codeSourceRejected, "video.duration_exceeded")
		}
	}
	return nil
}

func (s *Server) videoCut(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req videoCutReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkRidSource(req.Source), checkPurpose(req.Purpose), checkCutReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		p, err := s.probeReplicateVideo(ctx, path)
		if err != nil {
			return nil, err
		}
		dur := ms(p.DurationSeconds)
		ranges := make([]cutRange, len(req.Segments))
		for i, sg := range req.Segments {
			start, end := ms(sg.Start), ms(sg.End)
			if start >= dur || end > dur+replicateCutEndTolerance {
				return nil, newAPIError(codeSourceTooShort, "video.cut_out_of_range")
			}
			ranges[i] = cutRange{Start: start, End: min(end, dur)}
			if ranges[i].End-ranges[i].Start < replicateMinCut {
				return nil, newAPIError(codeSourceTooShort, "video.cut_out_of_range")
			}
		}
		base := idemKeyOf(ctx)
		resp := videoCutResp{Segments: make([]cutSegment, 0, len(ranges))}
		for i, rg := range ranges {
			out := workFile(dir, fmt.Sprintf("cut-%d.mp4", i))
			// 输入侧 -ss + 重编码：从最近的关键帧解码到精确起点，起点不要求落在关键帧上
			args := []string{"-nostdin", "-y", "-threads", s.svc.threads(),
				"-ss", ffmpegNumber(rg.Start), "-i", path, "-t", ffmpegNumber(rg.End - rg.Start),
				"-map", "0:v:0", "-map", "0:a:0?"}
			if p.Width%2 != 0 || p.Height%2 != 0 {
				args = append(args, "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2")
			}
			args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
				"-movflags", "+faststart", "-avoid_negative_ts", "make_zero", out)
			if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
				return nil, ffmpegFailure(ctx, err, "media.cut_failed")
			}
			// 每段有各自的 rid：按序号派生子键（同一键重放得到同一组 rid）
			res, err := s.storeFile(withIdemKey(ctx, base+"#cut"+strconv.Itoa(i)), req.Purpose, out)
			if err != nil {
				return nil, err
			}
			resp.Segments = append(resp.Segments, cutSegment{MediaResult: res, Start: rg.Start, End: rg.End})
		}
		return resp, nil
	})
}

// ---- video/concat ----

type placeholderSpec struct {
	Duration   float64 `json:"duration"`
	Text       string  `json:"text,omitempty"`
	FontRid    string  `json:"font_rid,omitempty"`
	Size       int     `json:"size,omitempty"`       // 字号上限（像素），0 = 按画面高度取
	Color      string  `json:"color,omitempty"`      // 文字色，默认 #FFFFFF
	Background string  `json:"background,omitempty"` // 底色，默认 #1F2937
}

type concatItem struct {
	Source      *SourceRef       `json:"source,omitempty"`
	Placeholder *placeholderSpec `json:"placeholder,omitempty"`
}

type videoConcatReq struct {
	Items      []concatItem `json:"items"`
	Width      int          `json:"width,omitempty"` // 目标规格；省略 = 第一个真实段的规格（width / height 要么都给要么都不给）
	Height     int          `json:"height,omitempty"`
	Fps        int          `json:"fps,omitempty"`
	SampleRate int          `json:"sample_rate,omitempty"`
	Audio      string       `json:"audio,omitempty"` // keep（默认）| drop
	Purpose    string       `json:"purpose"`
}

type concatTimelineEntry struct {
	Index    int     `json:"index"`
	Kind     string  `json:"kind"` // source | placeholder
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// concatResp MediaResult 的字段平铺在外层（与其它算子一致），多带一份时间轴：各段在成片里的起点与时长。
type concatResp struct {
	MediaResult
	Timeline []concatTimelineEntry `json:"timeline"`
}

func checkConcatReq(req videoConcatReq) error {
	if len(req.Items) < 1 || len(req.Items) > replicateMaxSegments {
		return invalid("request.segments_count")
	}
	for _, it := range req.Items {
		if (it.Source == nil) == (it.Placeholder == nil) {
			return invalid("request.item_exactly_one")
		}
		if it.Source != nil {
			if err := checkRidSource(*it.Source); err != nil {
				return err
			}
			continue
		}
		ph := it.Placeholder
		if math.IsNaN(ph.Duration) || ph.Duration < 0.1 || ph.Duration > replicateMaxSeconds {
			return invalid("request.placeholder_duration")
		}
		if utf8.RuneCountInString(ph.Text) > placeholderMaxRunes || !utf8.ValidString(ph.Text) || strings.ContainsRune(ph.Text, 0) {
			return invalid("request.placeholder_text")
		}
		if ph.FontRid != "" {
			if _, _, err := ParseRID(ph.FontRid); err != nil {
				return invalid("request.font_rid")
			}
		}
		if ph.Size < 0 || ph.Size > textMaxSize {
			return invalid("request.slot_size")
		}
		if _, ok := parseTextColor(ph.Color, color.NRGBA{}); !ok {
			return invalid("request.slot_color")
		}
		if _, ok := parseTextColor(ph.Background, color.NRGBA{}); !ok {
			return invalid("request.placeholder_background")
		}
	}
	if (req.Width == 0) != (req.Height == 0) {
		return invalid("request.size_pair")
	}
	if req.Width != 0 && (req.Width < replicateMinSide || req.Height < replicateMinSide ||
		req.Width > replicateMaxOutSide || req.Height > replicateMaxOutSide) {
		return invalid("request.size_range")
	}
	if req.Fps < 0 || req.Fps > replicateMaxFPS {
		return invalid("request.fps_range")
	}
	if req.SampleRate != 0 && (req.SampleRate < 8000 || req.SampleRate > 48000) {
		return invalid("request.sample_rate_range")
	}
	switch req.Audio {
	case "", "keep", "drop":
	default:
		return invalid("request.audio_mode")
	}
	return checkPurpose(req.Purpose)
}

func evenDown(v int) int { return max(2, v/2*2) }

// placeholderFrame 画一张占位画面：纯色底 + 居中文字。
func placeholderFrame(fonts map[string]*textFont, ph placeholderSpec, w, h int) (*image.NRGBA, error) {
	bg, _ := parseTextColor(ph.Background, mustColor(placeholderDefaultBGColor))
	frame := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(frame, frame.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	if strings.TrimSpace(ph.Text) == "" {
		return frame, nil
	}
	size := ph.Size
	if size == 0 {
		size = max(16, h/14)
	}
	sl := textSlot{
		Text: ph.Text, Box: textBox{X: 0.08, Y: 0.3, W: 0.84, H: 0.4}, FontRid: ph.FontRid,
		Size: size, Color: ph.Color, Align: "center", Valign: "middle",
	}
	r := slotRect(sl.Box, w, h)
	layer, err := renderSlot(fonts, sl, r.Dx(), r.Dy())
	if err != nil {
		return nil, err
	}
	draw.Draw(frame, r, layer, image.Point{}, draw.Over)
	return frame, nil
}

func mustColor(v string) color.NRGBA {
	c, _ := parseTextColor(v, color.NRGBA{})
	return c
}

// concatPart 拼接前整理好的一段。
type concatPart struct {
	index    int
	kind     string
	path     string  // 真实段的本地文件；占位为生成的 PNG
	duration float64 // 秒
	hasAudio bool
}

func (s *Server) videoConcat(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req videoConcatReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkConcatReq(req); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		parts := make([]concatPart, len(req.Items))
		var first *ProbeResult // 第一个真实段：统一规格的基准
		var firstAudio *ProbeResult
		total := 0.0
		for i, it := range req.Items {
			if it.Placeholder != nil {
				parts[i] = concatPart{index: i, kind: "placeholder", duration: ms(it.Placeholder.Duration)}
				total += parts[i].duration
				continue
			}
			segDir := filepath.Join(dir, fmt.Sprintf("seg-%d", i))
			if err := os.MkdirAll(segDir, 0o755); err != nil {
				return nil, internal(err)
			}
			path, err := s.sourceFn(*it.Source)(ctx, segDir)
			if err != nil {
				return nil, err
			}
			p, err := s.probeReplicateVideo(ctx, path)
			if err != nil {
				return nil, err
			}
			parts[i] = concatPart{index: i, kind: "source", path: path, duration: ms(p.DurationSeconds), hasAudio: p.HasAudio}
			total += parts[i].duration
			if first == nil {
				pc := p
				first = &pc
			}
			if firstAudio == nil && p.HasAudio {
				pc := p
				firstAudio = &pc
			}
		}
		if total > replicateMaxSeconds+replicateConcatSlack {
			return nil, newAPIError(codeSourceRejected, "video.total_duration_exceeded")
		}

		// 目标规格：参数优先，否则第一个真实段
		width, height, fps, rate := req.Width, req.Height, req.Fps, req.SampleRate
		if width == 0 {
			if first == nil {
				return nil, invalid("request.placeholder_needs_size")
			}
			width, height = first.Width, first.Height
		}
		width, height = evenDown(width), evenDown(height)
		if fps == 0 {
			fps = replicateDefaultFPS
			if first != nil && first.FPS >= 1 {
				fps = min(int(math.Round(first.FPS)), replicateMaxFPS)
			}
		}
		if rate == 0 {
			rate = replicateDefaultSampleHz
			if firstAudio != nil && firstAudio.SampleRate >= 8000 {
				rate = min(firstAudio.SampleRate, 48000)
			}
		}
		withAudio := false
		if req.Audio != "drop" {
			for _, p := range parts {
				withAudio = withAudio || p.hasAudio
			}
		}

		// 占位画面
		var rids, texts []string
		for _, it := range req.Items {
			if it.Placeholder != nil && strings.TrimSpace(it.Placeholder.Text) != "" {
				rids = append(rids, it.Placeholder.FontRid)
				texts = append(texts, it.Placeholder.Text)
			}
		}
		fonts, err := s.loadSlotFonts(ctx, dir, rids, texts)
		if err != nil {
			return nil, err
		}
		for i, it := range req.Items {
			if it.Placeholder == nil {
				continue
			}
			frame, err := placeholderFrame(fonts, *it.Placeholder, width, height)
			if err != nil {
				return nil, err
			}
			parts[i].path = workFile(dir, fmt.Sprintf("placeholder-%d.png", i))
			f, err := os.Create(parts[i].path)
			if err != nil {
				return nil, internal(err)
			}
			err = png.Encode(f, frame)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, internal(err)
			}
		}
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads()}
		var fg strings.Builder
		var join strings.Builder
		resp := concatResp{Timeline: make([]concatTimelineEntry, 0, len(parts))}
		at := 0.0
		for i, p := range parts {
			if p.kind == "placeholder" {
				args = append(args, "-loop", "1", "-framerate", strconv.Itoa(fps), "-t", ffmpegNumber(p.duration), "-i", p.path)
			} else {
				args = append(args, "-i", p.path)
			}
			d := ffmpegNumber(p.duration)
			fmt.Fprintf(&fg, "[%d:v:0]fps=%d,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p,tpad=stop_mode=clone:stop_duration=1,trim=duration=%s,setpts=PTS-STARTPTS[v%d];",
				i, fps, width, height, width, height, d, i)
			join.WriteString(fmt.Sprintf("[v%d]", i))
			if withAudio {
				if p.hasAudio {
					fmt.Fprintf(&fg, "[%d:a:0]aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo,apad,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d];", i, rate, d, i)
				} else {
					fmt.Fprintf(&fg, "anullsrc=r=%d:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d];", rate, d, i)
				}
				join.WriteString(fmt.Sprintf("[a%d]", i))
			}
			resp.Timeline = append(resp.Timeline, concatTimelineEntry{Index: i, Kind: p.kind, Start: ms(at), Duration: p.duration})
			at += p.duration
		}
		if withAudio {
			fmt.Fprintf(&fg, "%sconcat=n=%d:v=1:a=1[vout][aout]", join.String(), len(parts))
		} else {
			fmt.Fprintf(&fg, "%sconcat=n=%d:v=1:a=0[vout]", join.String(), len(parts))
		}
		out := workFile(dir, "concat.mp4")
		args = append(args, "-filter_complex", fg.String(), "-map", "[vout]")
		if withAudio {
			args = append(args, "-map", "[aout]", "-c:a", "aac", "-ar", strconv.Itoa(rate))
		}
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-preset", "veryfast", "-r", strconv.Itoa(fps),
			"-movflags", "+faststart", out)
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.concat_failed")
		}
		res, err := s.storeFile(ctx, req.Purpose, out)
		if err != nil {
			return nil, err
		}
		resp.MediaResult = res
		return resp, nil
	})
}
