package aigc

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// VideoLimits 视频复刻对源视频的限制（设计稿：仅支持 3 分钟、50M 以内的视频）。
type VideoLimits struct {
	MaxBytes   int64    // 体积上限
	MaxSeconds int      // 时长上限（拿不到时长时只卡体积，等 70 返回真实时长再判）
	Formats    []string // 允许的扩展名，如 mp4、mov
}

// ErrVideoTooLarge / ErrVideoFormat 供调用方按类型给不同提示。
type VideoError struct {
	Reason string // tooLarge / badFormat / unreachable
	Msg    string
}

func (e *VideoError) Error() string { return e.Msg }

// ProbeVideo 下载前先探一下：HEAD 拿 Content-Length 与类型，超限直接拒，避免白下一场。
//
// 注意 HEAD 拿不到时长，时长只能靠链接解析返回或等执行面 70 返回；
// 服务端不认 HEAD 时退化成 Range 探测（只取前 1 字节）。
func (l VideoLimits) ProbeVideo(ctx context.Context, hc *http.Client, url string) error {
	if err := l.checkFormat(url); err != nil {
		return err
	}
	size, err := contentLength(ctx, hc, url)
	if err != nil {
		return &VideoError{Reason: "unreachable", Msg: "视频地址打不开，请换一个试试"}
	}
	if size > 0 && size > l.MaxBytes {
		return &VideoError{Reason: "tooLarge", Msg: l.LimitMsg()}
	}
	return nil // size <= 0：对方没给长度，交给边下边数兜底
}

// LimitMsg 超限时给用户的统一提示（设计稿文案）。
func (l VideoLimits) LimitMsg() string {
	return fmt.Sprintf("仅支持 %d 分钟、%dM 以内的视频，请重新上传", l.MaxSeconds/60, l.MaxBytes>>20)
}

func (l VideoLimits) checkFormat(url string) error {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(strings.SplitN(url, "?", 2)[0]), "."))
	for _, f := range l.Formats {
		if ext == f {
			return nil
		}
	}
	return &VideoError{Reason: "badFormat", Msg: "仅支持 " + strings.ToUpper(strings.Join(l.Formats, "/")) + " 格式的视频"}
}

func contentLength(ctx context.Context, hc *http.Client, url string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := hc.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return resp.ContentLength, nil
		}
	}
	// 有些 CDN 不认 HEAD，用 Range 只取 1 字节，从 Content-Range 里读总长度
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp2, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent && resp2.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("aigc: 探测视频返回 %d", resp2.StatusCode)
	}
	if cr := resp2.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			var total int64
			if _, err := fmt.Sscan(cr[i+1:], &total); err == nil {
				return total, nil
			}
		}
	}
	return 0, nil
}
