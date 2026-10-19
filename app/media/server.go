package media

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// TokenHeader 调用方鉴权头。
const TokenHeader = "X-Media-Token"

const maxRequestBody = 1 << 20 // 请求体只有 URL 列表，1M 足够

// Server 媒体服务的 HTTP 入口。契约见 docs/api/media-service.md。
type Server struct {
	svc   *Service
	token string
	mux   *http.ServeMux
}

// NewServer token 为空直接报错：这是能让人把任意地址拉下来处理的服务，不允许无鉴权启动。
func NewServer(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("media: 必须配置 Token，拒绝无鉴权启动")
	}
	s := &Server{svc: NewService(cfg), token: cfg.Token, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /v1/capabilities", s.auth(s.capabilities))
	s.registerAV()
	s.mux.HandleFunc("POST /v1/cover", s.auth(s.cover))
	s.mux.HandleFunc("POST /v1/watermark", s.auth(s.watermark))
	s.mux.HandleFunc("POST /v1/compose", s.auth(s.compose))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// auth 常量时间比较 token，避免靠响应时间猜出来。两边先过 SHA-256 再比，定长比较，不泄露 token 长度。
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.token))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(r.Header.Get(TokenHeader)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "鉴权失败")
			return
		}
		next(w, r)
	}
}

// capabilities 告诉调用方服务端现在能做什么（水印依赖 logo 是否已配置）。
func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{
		"cover":     true,
		"compose":   true,
		"av":        true,
		"watermark": s.svc.logoAvailable(),
	})
}

type videoRequest struct {
	VideoURL string `json:"video_url"`
}

type composeRequest struct {
	VideoURLs []string `json:"video_urls"`
}

func (s *Server) cover(w http.ResponseWriter, r *http.Request) {
	in, ok := s.source(w, r)
	if !ok {
		return
	}
	data, err := s.svc.Cover(r.Context(), in)
	if err != nil {
		writeOpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) watermark(w http.ResponseWriter, r *http.Request) {
	in, ok := s.source(w, r)
	if !ok {
		return
	}
	out, err := s.svc.Watermark(r.Context(), in)
	if err != nil {
		writeOpError(w, err)
		return
	}
	defer out.Cleanup()
	streamVideo(w, out.Path)
}

func (s *Server) compose(w http.ResponseWriter, r *http.Request) {
	var req composeRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := s.svc.Compose(r.Context(), req.VideoURLs)
	if err != nil {
		writeOpError(w, err)
		return
	}
	defer out.Cleanup()
	streamVideo(w, out.Path)
}

// source 输入二选一：JSON 里给公网地址（服务端下载），或直接把视频文件作为请求体（Content-Type 非 JSON）。
// 后者让调用方不必为本地临时文件另外发布地址。
func (s *Server) source(w http.ResponseWriter, r *http.Request) (Source, bool) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req videoRequest
		if !decode(w, r, &req) {
			return nil, false
		}
		return s.svc.FromURL(req.VideoURL), true
	}
	return s.svc.FromBody(r.Body), true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "bad_input", "请求体不是合法 JSON")
		return false
	}
	return true
}

func streamVideo(w http.ResponseWriter, path string) {
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
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	_, _ = io.Copy(w, f)
}

func writeOpError(w http.ResponseWriter, err error) {
	status, code := statusOf(err)
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	writeError(w, status, code, errMessage(err))
}

// errMessage 对外只给分类说明，ffmpeg 的原始输出只进服务日志。
func errMessage(err error) string {
	var oe *opError
	if errors.As(err, &oe) {
		return oe.msg
	}
	return "服务内部错误"
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}
