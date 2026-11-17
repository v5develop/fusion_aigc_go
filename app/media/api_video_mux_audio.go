package media

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 视频做同款「保留原声」的贴回算子（add-only）：
//
//	POST /v1/mediasvc/video/mux-audio   {"video": {rid}, "audio": {rid}, "ranges": [{start,end}...], "purpose"}
//
// 取 audio（原片）里 ranges 各区间的音频，按顺序首尾相接成一条音轨，对齐到 video 的时间轴后替换 video 的音轨（视频流直接拷贝，不重编码）：
//   - 音轨比视频短 → 末尾补静音，不循环；比视频长 → 截到视频长度；
//   - 某个区间超出原片音轨时长（或整段落在时长之外）→ 该区间里没有声音的部分补静音，后面的区间不前移，时间轴不错位；
//   - video 原有的音轨一律丢弃（可以没有音轨：生成的段视频通常就是无声的）。
//
// 输入只接受 rid；写操作带 Idempotency-Key。判据（check）：
//
//	request.ranges_count      区间数不在 1~16                        400 invalid_request
//	request.segment_range     区间非法（end<=start、负数、NaN、过长）  400 invalid_request
//	audio.no_audio_stream     原片没有音轨                           422 source_rejected
//	video.*                   同 video/cut（无视频流 / 超 80 秒 / 分辨率超限）
//
// 响应 = MediaResult 平铺 + audio_seconds（原片真正提供的声音秒数）+ padded_seconds（补静音的秒数）。

const (
	muxMaxRanges     = 16
	muxSilenceMargin = 0.02 // 区间起点距原片音轨末尾不足这么多秒，视为整段没有声音
)

type videoMuxAudioReq struct {
	Video   SourceRef  `json:"video"`
	Audio   SourceRef  `json:"audio"`
	Ranges  []cutRange `json:"ranges"`
	Purpose string     `json:"purpose"`
}

type muxAudioResp struct {
	MediaResult
	AudioSeconds  float64 `json:"audio_seconds"`
	PaddedSeconds float64 `json:"padded_seconds"`
}

func checkMuxReq(req videoMuxAudioReq) error {
	if len(req.Ranges) < 1 || len(req.Ranges) > muxMaxRanges {
		return invalid("request.ranges_count")
	}
	for _, rg := range req.Ranges {
		if math.IsNaN(rg.Start) || math.IsNaN(rg.End) || math.IsInf(rg.Start, 0) || math.IsInf(rg.End, 0) ||
			rg.Start < 0 || ms(rg.End) <= ms(rg.Start) || rg.End-rg.Start > replicateMaxSeconds+replicateDurationSlack {
			return invalid("request.segment_range")
		}
	}
	return nil
}

// muxFilter 生成 filter_complex：每个区间一路（有声的 atrim，整段落在时长外的用静音源），拼接后补静音到视频时长并截断。
// 返回滤镜串、原片实际提供的声音秒数。
func muxFilter(ranges []cutRange, srcDur, videoDur float64, rate int) (string, float64) {
	var fg, join strings.Builder
	covered := 0.0
	for i, rg := range ranges {
		start, end := ms(rg.Start), ms(rg.End)
		length := ms(end - start)
		if start >= srcDur-muxSilenceMargin {
			fmt.Fprintf(&fg, "anullsrc=r=%d:cl=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[a%d];", rate, ffmpegNumber(length), i)
		} else {
			covered += min(end, srcDur) - start
			fmt.Fprintf(&fg, "[1:a:0]atrim=start=%s:end=%s,asetpts=PTS-STARTPTS,aresample=%d,aformat=sample_fmts=fltp:channel_layouts=stereo,apad=whole_dur=%s,atrim=duration=%s[a%d];",
				ffmpegNumber(start), ffmpegNumber(end), rate, ffmpegNumber(length), ffmpegNumber(length), i)
		}
		fmt.Fprintf(&join, "[a%d]", i)
	}
	fmt.Fprintf(&fg, "%sconcat=n=%d:v=0:a=1,apad=whole_dur=%s,atrim=duration=%s,asetpts=PTS-STARTPTS[aout]",
		join.String(), len(ranges), ffmpegNumber(videoDur), ffmpegNumber(videoDur))
	return fg.String(), covered
}

func (s *Server) videoMuxAudio(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req videoMuxAudioReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkRidSource(req.Video), checkRidSource(req.Audio), checkPurpose(req.Purpose), checkMuxReq(req)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		video, err := s.sourceFn(req.Video)(ctx, dir)
		if err != nil {
			return nil, err
		}
		audioDir := filepath.Join(dir, "audio")
		if err := os.MkdirAll(audioDir, 0o755); err != nil {
			return nil, internal(err)
		}
		audio, err := s.sourceFn(req.Audio)(ctx, audioDir)
		if err != nil {
			return nil, err
		}
		vp, err := s.probeReplicateVideo(ctx, video)
		if err != nil {
			return nil, err
		}
		ap, err := s.svc.Probe(ctx, audio)
		if err != nil {
			return nil, err
		}
		if !ap.HasAudio {
			return nil, newAPIError(codeSourceRejected, "audio.no_audio_stream")
		}
		rate := replicateDefaultSampleHz
		if ap.SampleRate >= 8000 {
			rate = min(ap.SampleRate, 48000)
		}
		videoDur := ms(vp.DurationSeconds)
		fg, covered := muxFilter(req.Ranges, ms(ap.DurationSeconds), videoDur, rate)
		out := workFile(dir, "muxed.mp4")
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-i", video, "-i", audio,
			"-filter_complex", fg, "-map", "0:v:0", "-map", "[aout]",
			"-c:v", "copy", "-c:a", "aac", "-ar", strconv.Itoa(rate), "-movflags", "+faststart", out}
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.mux_audio_failed")
		}
		res, err := s.storeFile(ctx, req.Purpose, out)
		if err != nil {
			return nil, err
		}
		provided := math.Min(covered, videoDur)
		return muxAudioResp{MediaResult: res, AudioSeconds: ms(provided), PaddedSeconds: ms(math.Max(0, videoDur-provided))}, nil
	})
}
