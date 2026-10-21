package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
)

// 精确尺寸缩放（契约 /v1/mediasvc/image/resize）：输出严格是 width×height，不保比例（与 Java2D 现行 resizeImage 一致）。
//   - png：源图任一像素 alpha != 255 才保留 alpha（与 DefaultMediaToolkit.hasTranslucentPixel 同口径，RGBA 全不透明的图写成 RGB，
//     否则体积约翻倍）；
//   - jpeg：先铺白底再叠源图，输出 RGB（源图的透明区域变白，不变黑）。
//
// 只接受 PNG / JPEG / WebP 源；其它格式（包括视频）返回 422。解码交给 ffmpeg，解不开即 422 source_rejected。

const (
	maxResizeSide = 16384
)

// checkResizeSize 宽高各自 1..16384；总像素上限在处理时按 MaxSourcePixels 检查（422）。
func checkResizeSize(w, h int) error {
	if w < 1 || w > maxResizeSide || h < 1 || h > maxResizeSide {
		return invalid("request.size_range")
	}
	return nil
}

func checkResizeFormat(f string) error {
	if f != "png" && f != "jpeg" {
		return invalid("request.format")
	}
	return nil
}

// isImageFile 只看文件头：PNG / JPEG / WebP。
func isImageFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 8 && string(head[:8]) == "\x89PNG\r\n\x1a\n":
		return true
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return true
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return true
	}
	return false
}

// imageResize 精确尺寸缩放的 HTTP 入口。
func (s *Server) imageResize(w http.ResponseWriter, r *http.Request) {
	if err := checkKey(r.Header.Get(idempotencyHeader)); err != nil {
		writeAPIError(w, err)
		return
	}
	var req imageResizeReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := firstErr(checkSource(req.Source), checkPurpose(req.Purpose),
		checkResizeSize(req.Width, req.Height), checkResizeFormat(req.Format)); err != nil {
		writeAPIError(w, err)
		return
	}
	s.withJob(w, r, func(ctx context.Context, dir string) (any, error) {
		// 目标像素超限在下载前拒收：不需要为一个注定被拒的请求付出下载成本
		if limit := s.svc.cfg.MaxSourcePixels; limit > 0 && int64(req.Width)*int64(req.Height) > limit {
			return nil, newAPIError(codeSourceRejected, "image.target_pixels_exceeded")
		}
		path, err := s.sourceFn(req.Source)(ctx, dir)
		if err != nil {
			return nil, err
		}
		if !isImageFile(path) {
			return nil, newAPIError(codeSourceRejected, "image.not_image")
		}
		// 源图像素上限与可解码性在这里检查（probeMeta 统一做）
		if _, err := s.probeMeta(ctx, path); err != nil {
			return nil, err
		}
		ext := ".png"
		if req.Format == "jpeg" {
			ext = ".jpg"
		}
		out := workFile(dir, "resized"+ext)
		if err := s.svc.ResizeImage(ctx, path, req.Width, req.Height, req.Format, out); err != nil {
			return nil, err
		}
		return s.storeFile(ctx, req.Purpose, out)
	})
}

// ResizeImage 把图片精确缩放到 width×height，写到 out。format 为 png 或 jpeg（已由调用方校验）。
func (s *Service) ResizeImage(ctx context.Context, in string, width, height int, format, out string) error {
	args := []string{"-nostdin", "-y", "-threads", s.threads(), "-i", in}
	switch format {
	case "png":
		keepAlpha, err := s.hasTranslucent(ctx, in)
		if err != nil {
			return err
		}
		pix := "rgb24"
		if keepAlpha {
			pix = "rgba"
		}
		args = append(args, "-filter_complex",
			"[0:v:0]scale="+strconv.Itoa(width)+":"+strconv.Itoa(height)+",format="+pix+"[out]")
	case "jpeg":
		// 白底在下面：color 源提供白色画布，源图叠在上面（scale 不保比例，所以是强制尺寸）
		args = append(args, "-filter_complex",
			"color=c=white:s="+strconv.Itoa(width)+"x"+strconv.Itoa(height)+",format=rgba[bg];"+
				"[0:v:0]scale="+strconv.Itoa(width)+":"+strconv.Itoa(height)+",format=rgba[fg];"+
				"[bg][fg]overlay=format=auto,format=rgb24[out]")
	default:
		return newAPIError(codeInvalidRequest, "request.format")
	}
	// -update 1：image2 muxer 对没有序号模式的单文件输出要求显式声明，否则不写文件
	args = append(args, "-map", "[out]", "-frames:v", "1", "-update", "1", out)
	if err := s.run(ctx, s.ffmpeg(), args...); err != nil {
		return classifyFFmpeg(ctx, err, "图片缩放失败")
	}
	return nil
}

// hasTranslucent 任一像素 alpha != 255 即为 true（与 Java hasTranslucentPixel 同口径）。在计算名额内执行。
func (s *Service) hasTranslucent(ctx context.Context, file string) (bool, error) {
	var found bool
	err := s.compute(ctx, func() error {
		var err error
		found, err = s.scanTranslucent(ctx, file)
		return err
	})
	return found, err
}

// scanTranslucent 逐块读 ffmpeg 输出的原始 rgba 流，找到第一个非不透明像素就停止 ffmpeg，不把整张图放进内存。
func (s *Service) scanTranslucent(ctx context.Context, file string) (bool, error) {
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, s.ffmpeg(), "-nostdin", "-v", "error", "-i", file,
		"-map", "0:v:0", "-vf", "format=rgba", "-f", "rawvideo", "-pix_fmt", "rgba", "-")
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, internal(err)
	}
	if err := cmd.Start(); err != nil {
		return false, classifyFFmpeg(ctx, err, "图片缩放失败")
	}
	buf := make([]byte, 64<<10)
	var pos int64 // 在 rgba 字节流中的位置；alpha 位于 pos%4 == 3
	found := false
	for !found {
		n, readErr := stdout.Read(buf)
		for i := range n {
			if (pos+int64(i))%4 == 3 && buf[i] != 0xFF {
				found = true
				break
			}
		}
		pos += int64(n)
		if readErr != nil {
			break
		}
	}
	// 只有找到了才提前结束 ffmpeg；读到 EOF 时 ffmpeg 已经正常退出，不能再把它杀掉（否则会误报失败）
	if found {
		stop()
	}
	waitErr := cmd.Wait()
	if found {
		return true, nil
	}
	if waitErr != nil {
		return false, classifyFFmpeg(ctx, waitErr, "图片缩放失败")
	}
	return false, nil
}
