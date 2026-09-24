package linkparse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ErrTooLarge 视频超过上限。因为直链多半不给 Content-Length，所以是边下边判。
var ErrTooLarge = errors.New("linkparse: 视频体积超过上限")

// Download 把直链下下来。直链会过期，所以拿到就得下。
//
// 几条规矩来自他们的文档：不带 Referer（带了反而 403）、user_agent 非空必须带。
// maxBytes 是硬上限：先看 Content-Length，没有就边读边数，**超了立刻停**，
// 不让一个几百兆的视频把内存撑爆。
func Download(ctx context.Context, hc *http.Client, v *Video, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.URL, nil)
	if err != nil {
		return nil, "", err
	}
	if v.UserAgent != "" {
		req.Header.Set("User-Agent", v.UserAgent)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("linkparse: 下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("linkparse: 下载失败 HTTP %d", resp.StatusCode)
	}
	if maxBytes > 0 && resp.ContentLength > maxBytes {
		return nil, "", ErrTooLarge
	}
	// 多读 1 字节：读得出来就说明超了
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("linkparse: 下载失败: %w", err)
	}
	if maxBytes > 0 && int64(len(data)) > maxBytes {
		return nil, "", ErrTooLarge
	}
	return data, contentType(resp.Header.Get("Content-Type")), nil
}

// contentType 直链的类型有时带参数（video/mp4; charset=...），有时干脆没有。
func contentType(v string) string {
	t, _, _ := strings.Cut(v, ";")
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" || !strings.HasPrefix(t, "video/") {
		return "video/mp4" // 四个平台的视频作品都是 mp4
	}
	return t
}
