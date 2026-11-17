package media

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
)

// 配音与字幕三件套（工作流按 rid 调用，Java 不再传本地文件）：
// 音轨时间线、换音轨、烧字幕。实现见 av.go，这里只做参数校验与落桶。

const (
	maxTimelineSegments = 256
	maxTimelineSeconds  = 600
	maxSubtitleBytes    = 256 << 10
)

type audioTimelineReq struct {
	Sources []SourceRef `json:"sources"`
	Starts  []float64   `json:"starts"`
	// MaxSeconds 可选：每段最长时长（秒，与 sources 等长，0 = 不截），超出的尾部截掉（视频做同款换声音按句限窗）
	MaxSeconds   []float64 `json:"max_seconds"`
	TotalSeconds float64   `json:"total_seconds"`
	SampleRate   int       `json:"sample_rate"`
	Channels     int       `json:"channels"`
	Purpose      string    `json:"purpose"`
}

type replaceAudioReq struct {
	Video   SourceRef `json:"video"`
	Audio   SourceRef `json:"audio"`
	Purpose string    `json:"purpose"`
}

type burnSubtitlesReq struct {
	Video   SourceRef `json:"video"`
	Srt     string    `json:"srt"`
	Purpose string    `json:"purpose"`
}

func checkTimeline(req audioTimelineReq) error {
	if len(req.Sources) < 1 || len(req.Sources) > maxTimelineSegments || len(req.Sources) != len(req.Starts) {
		return invalid("request.timeline_segments")
	}
	for _, st := range req.Starts {
		if math.IsNaN(st) || math.IsInf(st, 0) || st < 0 {
			return invalid("request.timeline_starts")
		}
	}
	if len(req.MaxSeconds) > 0 {
		if len(req.MaxSeconds) != len(req.Sources) {
			return invalid("request.timeline_max_seconds")
		}
		for _, m := range req.MaxSeconds {
			if math.IsNaN(m) || math.IsInf(m, 0) || m < 0 || m > maxTimelineSeconds {
				return invalid("request.timeline_max_seconds")
			}
		}
	}
	if !(req.TotalSeconds > 0 && req.TotalSeconds <= maxTimelineSeconds) {
		return invalid("request.total_seconds_range")
	}
	if req.SampleRate < 0 || req.Channels < 0 || req.Channels > 8 {
		return invalid("request.audio_format")
	}
	return nil
}

func (s *Server) audioTimeline(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req audioTimelineReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkTimeline(req), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	for _, src := range req.Sources {
		if err := checkSource(src); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		files := make([]string, 0, len(req.Sources))
		for i, src := range req.Sources {
			segDir := filepath.Join(dir, fmt.Sprintf("seg-%03d", i))
			if err := os.MkdirAll(segDir, 0o755); err != nil {
				return nil, err
			}
			path, err := s.sourceFn(src)(ctx, segDir)
			if err != nil {
				return nil, err
			}
			files = append(files, path)
		}
		out := workFile(dir, "timeline.m4a")
		if err := s.svc.AudioTimelineMax(ctx, files, req.Starts, req.MaxSeconds, req.TotalSeconds, req.SampleRate, req.Channels, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.audio_timeline_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) replaceAudio(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req replaceAudioReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Video), checkSource(req.Audio), checkPurpose(req.Purpose)); err != nil {
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
			return nil, err
		}
		audio, err := s.sourceFn(req.Audio)(ctx, audioDir)
		if err != nil {
			return nil, err
		}
		out := workFile(dir, "replaced.mp4")
		if err := s.svc.ReplaceAudio(ctx, video, audio, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.replace_audio_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) burnSubtitles(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req burnSubtitlesReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Video), checkPurpose(req.Purpose)); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Srt == "" || len(req.Srt) > maxSubtitleBytes {
		writeAPIError(w, invalid("request.srt"))
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		video, err := s.sourceFn(req.Video)(ctx, dir)
		if err != nil {
			return nil, err
		}
		subtitle := workFile(dir, "input.srt")
		if err := os.WriteFile(subtitle, []byte(req.Srt), 0o600); err != nil {
			return nil, err
		}
		out := workFile(dir, "subtitled.mp4")
		if err := s.svc.BurnSubtitles(ctx, dir, video, subtitle, out); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.burn_subtitles_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}
