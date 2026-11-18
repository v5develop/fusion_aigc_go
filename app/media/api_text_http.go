package media

import (
	"context"
	"fmt"
	"image/png"
	"net/http"
	"os"
	"strings"
)

// 文字叠加两个入口（同一个渲染内核，见 api_text.go）：
//   POST /v1/mediasvc/image/text         图片上叠文字，输出与输入同格式（PNG / JPEG）
//   POST /v1/mediasvc/video/overlay-text 视频上叠文字（可按 start/end 分时显示），重编码参数与 video/trim 一致
// 输入只接受 rid；字体用 font 摄入得到的 rid（TrueType / OpenType / TTC），不传 font_rid 时只能排拉丁字符。

const textImageMaxSide = 8192

type imageTextReq struct {
	Source  SourceRef  `json:"source"`
	Slots   []textSlot `json:"slots"`
	Purpose string     `json:"purpose"`
}

type videoOverlayTextReq struct {
	Source  SourceRef  `json:"source"`
	Slots   []textSlot `json:"slots"`
	Purpose string     `json:"purpose"`
}

type rectXY struct{ x, y int }

// renderLayers 把全部槽位画成 PNG 文件，返回文件路径与各自在画面上的左上角。
func (s *Server) renderLayers(ctx context.Context, dir string, slots []textSlot, width, height int) ([]string, []rectXY, error) {
	rids := make([]string, len(slots))
	texts := make([]string, len(slots))
	for i, sl := range slots {
		rids[i], texts[i] = sl.FontRid, sl.Text
	}
	fonts, err := s.loadSlotFonts(ctx, dir, rids, texts)
	if err != nil {
		return nil, nil, err
	}
	files := make([]string, len(slots))
	rects := make([]rectXY, len(slots))
	for i, sl := range slots {
		r := slotRect(sl.Box, width, height)
		if r.Dx()*r.Dy() > textLayerLimit {
			return nil, nil, newAPIError(codeSourceRejected, "text.layer_exceeded")
		}
		layer, err := renderSlot(fonts, sl, r.Dx(), r.Dy())
		if err != nil {
			return nil, nil, err
		}
		files[i] = workFile(dir, fmt.Sprintf("slot-%02d.png", i))
		f, err := os.Create(files[i])
		if err != nil {
			return nil, nil, internal(err)
		}
		err = png.Encode(f, layer)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, nil, internal(err)
		}
		rects[i] = rectXY{r.Min.X, r.Min.Y}
	}
	return files, rects, nil
}

func (s *Server) imageText(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageTextReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkRidSource(req.Source), checkPurpose(req.Purpose), checkTextSlots(req.Slots, false)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		path, meta, err := s.fetchImageSource(ctx, dir, "src", req.Source)
		if err != nil {
			return nil, err
		}
		format := imageFormatOf(path)
		if format == "" {
			return nil, newAPIError(codeSourceRejected, "image.format_unsupported")
		}
		if meta.Width > textImageMaxSide || meta.Height > textImageMaxSide {
			return nil, newAPIError(codeSourceRejected, "image.dimension_exceeded")
		}
		layers, rects, err := s.renderLayers(ctx, dir, req.Slots, meta.Width, meta.Height)
		if err != nil {
			return nil, err
		}
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-noautorotate", "-i", path}
		var fg strings.Builder
		fg.WriteString("[0:v:0]format=rgba[b0]")
		for i, l := range layers {
			args = append(args, "-i", l)
			fmt.Fprintf(&fg, ";[b%d][%d:v:0]overlay=x=%d:y=%d:format=auto[b%d]", i, i+1, rects[i].x, rects[i].y, i+1)
		}
		last := fmt.Sprintf("b%d", len(layers))
		ext := ".png"
		if format == "jpeg" {
			fmt.Fprintf(&fg, ";[%s]format=yuvj420p[out]", last)
			ext = ".jpg"
		} else {
			fmt.Fprintf(&fg, ";[%s]format=rgba[out]", last)
		}
		out := workFile(dir, "text"+ext)
		args = append(args, "-filter_complex", fg.String(), "-map", "[out]", "-frames:v", "1")
		if format == "jpeg" {
			args = append(args, "-q:v", "3")
		}
		args = append(args, "-update", "1", out)
		if err := s.svc.runLight(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, imageFailure(ctx, err, "image.text_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

func (s *Server) videoOverlayText(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req videoOverlayTextReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkRidSource(req.Source), checkPurpose(req.Purpose), checkTextSlots(req.Slots, true)); err != nil {
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
		for _, sl := range req.Slots {
			if sl.Start != nil && *sl.Start >= p.DurationSeconds {
				return nil, invalid("request.slot_time_range") // 起点落在片尾之后，永远看不到
			}
		}
		layers, rects, err := s.renderLayers(ctx, dir, req.Slots, p.Width, p.Height)
		if err != nil {
			return nil, err
		}
		args := []string{"-nostdin", "-y", "-threads", s.svc.threads(), "-i", path}
		var fg strings.Builder
		cur := "0:v:0"
		for i, l := range layers {
			args = append(args, "-i", l)
			fmt.Fprintf(&fg, "[%s][%d:v:0]overlay=x=%d:y=%d", cur, i+1, rects[i].x, rects[i].y)
			if sl := req.Slots[i]; sl.Start != nil {
				fmt.Fprintf(&fg, ":enable='between(t,%s,%s)'", ffmpegNumber(*sl.Start), ffmpegNumber(*sl.End))
			}
			fmt.Fprintf(&fg, ":format=auto[v%d];", i+1)
			cur = fmt.Sprintf("v%d", i+1)
		}
		fmt.Fprintf(&fg, "[%s]scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p[vout]", cur)
		out := workFile(dir, "overlay.mp4")
		args = append(args, "-filter_complex", fg.String(), "-map", "[vout]", "-map", "0:a:0?",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", out)
		if err := s.svc.run(ctx, s.svc.ffmpeg(), args...); err != nil {
			return nil, ffmpegFailure(ctx, err, "media.overlay_text_failed")
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}
