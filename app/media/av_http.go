package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// maxFrames 单次抽帧的上限：审核最多要几帧，再多没有意义。
const maxFrames = 16

// maxParamsBytes multipart 里 params 字段的上限（只放数字和短数组）。
const maxParamsBytes = 64 << 10

// registerAV 注册音视频类接口。输入有两种形式：
//   - 原始文件体（probe / frames / validate / audio/extract / video/adjust-duration）；
//   - multipart（audio/timeline、video/replace-audio、video/burn-subtitles），参数放在 params 字段里。
func (s *Server) registerAV() {
	s.mux.HandleFunc("POST /v1/probe", s.auth(s.probe))
	s.mux.HandleFunc("POST /v1/frames", s.auth(s.frames))
	s.mux.HandleFunc("POST /v1/validate", s.auth(s.validate))
	s.mux.HandleFunc("POST /v1/audio/extract", s.auth(s.extractAudio))
	s.mux.HandleFunc("POST /v1/audio/timeline", s.auth(s.audioTimeline))
	s.mux.HandleFunc("POST /v1/video/adjust-duration", s.auth(s.adjustDuration))
	s.mux.HandleFunc("POST /v1/video/replace-audio", s.auth(s.replaceAudio))
	s.mux.HandleFunc("POST /v1/video/burn-subtitles", s.auth(s.burnSubtitles))
}

// job 一次请求的执行环境：占好并发名额、建好工作目录，超时与清理由 done 统一收尾。
type job struct {
	ctx  context.Context
	dir  string
	done func()
}

func (s *Server) begin(w http.ResponseWriter, r *http.Request) (*job, bool) {
	ctx, dir, done, err := s.svc.begin(r.Context())
	if err != nil {
		writeOpError(w, err)
		return nil, false
	}
	return &job{ctx: ctx, dir: dir, done: done}, true
}

// fail 把错误写回去，并收尾。
func fail(w http.ResponseWriter, j *job, err error) {
	j.done()
	writeOpError(w, err)
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	defer j.done()
	src, err := s.svc.FromBody(r.Body)(j.ctx, j.dir)
	if err != nil {
		writeOpError(w, err)
		return
	}
	res, err := s.svc.Probe(j.ctx, src)
	if err != nil {
		writeOpError(w, err)
		return
	}
	writeJSON(w, res)
}

func (s *Server) frames(w http.ResponseWriter, r *http.Request) {
	count, err := strconv.Atoi(r.URL.Query().Get("count"))
	if err != nil || count < 1 || count > maxFrames {
		writeError(w, http.StatusUnprocessableEntity, "bad_input", fmt.Sprintf("count 须在 1 到 %d 之间", maxFrames))
		return
	}
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	defer j.done()
	src, err := s.svc.FromBody(r.Body)(j.ctx, j.dir)
	if err != nil {
		writeOpError(w, err)
		return
	}
	frames, err := s.svc.Frames(j.ctx, src, count)
	if err != nil {
		writeOpError(w, err)
		return
	}
	type item struct {
		Timestamp float64 `json:"timestamp"`
		JPEGB64   string  `json:"jpeg_base64"`
	}
	out := make([]item, 0, len(frames))
	for _, f := range frames {
		out = append(out, item{Timestamp: f.Timestamp, JPEGB64: base64.StdEncoding.EncodeToString(f.JPEG)})
	}
	writeJSON(w, map[string]any{"frames": out})
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	defer j.done()
	src, err := s.svc.FromBody(r.Body)(j.ctx, j.dir)
	if err != nil {
		writeOpError(w, err)
		return
	}
	if err := s.svc.Validate(j.ctx, src); err != nil {
		writeOpError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) extractAudio(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	src, err := s.svc.FromBody(r.Body)(j.ctx, j.dir)
	if err != nil {
		fail(w, j, err)
		return
	}
	out := filepath.Join(j.dir, "audio.mp3")
	if err := s.svc.ExtractAudio(j.ctx, src, out); err != nil {
		fail(w, j, classifyFFmpeg(j.ctx, err, "抽取音轨失败"))
		return
	}
	streamFile(w, j, out, "audio/mpeg")
}

type timelineParams struct {
	Starts        []float64 `json:"starts"`
	TotalDuration float64   `json:"total_duration"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
}

func (s *Server) audioTimeline(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	var p timelineParams
	parts, err := s.readMultipart(r, j.dir, &p)
	if err != nil {
		fail(w, j, err)
		return
	}
	if len(p.Starts) == 0 || len(p.Starts) > s.svc.cfg.MaxComposeInputs {
		fail(w, j, badInput(fmt.Sprintf("音频片段数须在 1 到 %d 之间", s.svc.cfg.MaxComposeInputs)))
		return
	}
	if p.TotalDuration <= 0 {
		fail(w, j, badInput("总时长必须大于 0"))
		return
	}
	files := make([]string, 0, len(p.Starts))
	for i := range p.Starts {
		f, ok := parts[fmt.Sprintf("audio%d", i)]
		if !ok {
			fail(w, j, badInput(fmt.Sprintf("缺少音频片段 audio%d", i)))
			return
		}
		files = append(files, f)
	}
	out := filepath.Join(j.dir, "timeline.m4a")
	if err := s.svc.AudioTimeline(j.ctx, files, p.Starts, p.TotalDuration, p.SampleRate, p.Channels, out); err != nil {
		fail(w, j, classifyFFmpeg(j.ctx, err, "音轨拼接失败"))
		return
	}
	streamFile(w, j, out, "audio/mp4")
}

func (s *Server) adjustDuration(w http.ResponseWriter, r *http.Request) {
	factor, err := strconv.ParseFloat(r.URL.Query().Get("factor"), 64)
	if err != nil || factor <= 0 || factor > 100 {
		writeError(w, http.StatusUnprocessableEntity, "bad_input", "factor 须在 (0, 100] 之间")
		return
	}
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	src, err := s.svc.FromBody(r.Body)(j.ctx, j.dir)
	if err != nil {
		fail(w, j, err)
		return
	}
	out := filepath.Join(j.dir, "adjusted.mp4")
	if err := s.svc.AdjustDuration(j.ctx, src, factor, out); err != nil {
		fail(w, j, classifyFFmpeg(j.ctx, err, "变速失败"))
		return
	}
	streamFile(w, j, out, "video/mp4")
}

func (s *Server) replaceAudio(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	parts, err := s.readMultipart(r, j.dir, nil)
	if err != nil {
		fail(w, j, err)
		return
	}
	video, audio := parts["video"], parts["audio"]
	if video == "" || audio == "" {
		fail(w, j, badInput("需要 video 与 audio 两个文件"))
		return
	}
	out := filepath.Join(j.dir, "replaced.mp4")
	if err := s.svc.ReplaceAudio(j.ctx, video, audio, out); err != nil {
		fail(w, j, classifyFFmpeg(j.ctx, err, "替换音轨失败"))
		return
	}
	streamFile(w, j, out, "video/mp4")
}

func (s *Server) burnSubtitles(w http.ResponseWriter, r *http.Request) {
	j, ok := s.begin(w, r)
	if !ok {
		return
	}
	parts, err := s.readMultipart(r, j.dir, nil)
	if err != nil {
		fail(w, j, err)
		return
	}
	video, subtitle := parts["video"], parts["subtitle"]
	if video == "" || subtitle == "" {
		fail(w, j, badInput("需要 video 与 subtitle 两个文件"))
		return
	}
	out := filepath.Join(j.dir, "subtitled.mp4")
	if err := s.svc.BurnSubtitles(j.ctx, j.dir, video, subtitle, out); err != nil {
		fail(w, j, classifyFFmpeg(j.ctx, err, "烧录字幕失败"))
		return
	}
	streamFile(w, j, out, "video/mp4")
}

// readMultipart 读取 multipart 请求：params 字段解析成 v（为 nil 则忽略），其余每个字段存为 dir 下的文件，
// 返回 字段名 → 本地路径。单个文件受 MaxInputBytes 约束；字段名只能是字母数字，避免路径穿越。
func (s *Server) readMultipart(r *http.Request, dir string, v any) (map[string]string, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, badInput("需要 multipart/form-data 请求")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	files := make(map[string]string)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badInput("multipart 请求格式错误")
		}
		name := part.FormName()
		if name == "" || !isSafeFieldName(name) {
			return nil, badInput("字段名不合法")
		}
		if name == "params" {
			raw, err := io.ReadAll(io.LimitReader(part, maxParamsBytes+1))
			if err != nil || len(raw) > maxParamsBytes {
				return nil, badInput("params 过大或读取失败")
			}
			if v != nil {
				if err := json.Unmarshal(raw, v); err != nil {
					return nil, badInput("params 不是合法 JSON")
				}
			}
			continue
		}
		path := filepath.Join(dir, name)
		out, err := os.Create(path)
		if err != nil {
			return nil, internal(err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(part, s.svc.cfg.MaxInputBytes+1))
		_ = out.Close()
		if copyErr != nil {
			return nil, badInput("读取上传文件失败")
		}
		if n == 0 {
			return nil, badInput(fmt.Sprintf("%s 为空", name))
		}
		if n > s.svc.cfg.MaxInputBytes {
			return nil, badInput("文件体积超过上限")
		}
		files[name] = path
	}
	return files, nil
}

func isSafeFieldName(name string) bool {
	for _, c := range name {
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return len(name) <= 32
}

func streamFile(w http.ResponseWriter, j *job, path, contentType string) {
	defer j.done()
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "服务内部错误")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, _ = io.Copy(w, f)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
