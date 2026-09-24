// Package linkparse 调同事的分享链接解析服务：分享链接/整段分享文案 → 作品直链。
//
//	POST {base}/parse  {"url": "...", "prefer_quality": false}
//
// 覆盖抖音 / 快手 / 小红书 / B站，平台自动识别。几个要命的细节（都写在他们文档里）：
//
//   - **直链带签名、有有效期**（抖音约 3 小时），所以拿到就得当场下文件，不能存 URL；
//   - **下载不要带 Referer**，四个平台裸请求都能下，带了反而 403；
//   - `user_agent` 非空时下载**必须**带上，目前只有 B站 需要；
//   - B站 走 yt-dlp 取 1080P 时音视频是分离的（needs_mux），直接下会得到哑片。
//     所以我们固定传 prefer_quality=false，让 B站 走 720P 单文件，省掉合流。
//
// 耗时：小红书 0.5–3s、快手/B站 ~2s、抖音 4–8s，降级路径更慢，所以超时给到 60s。
package linkparse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Conf struct {
	// BaseURL 解析服务地址；留空表示「链接查视频」不开放
	BaseURL string `json:",optional"`
	// TimeoutSeconds 他们要求不低于 60 秒
	TimeoutSeconds int `json:",default=60"`
}

var (
	// ErrUnsupported 平台不支持，或这段文案里根本没有链接（他们的 422）。重试没用。
	ErrUnsupported = errors.New("linkparse: 链接不支持或没找到链接")
	// ErrUpstream 解析服务自己失败了（502/503）。可以重试。
	ErrUpstream = errors.New("linkparse: 解析服务暂时不可用")
	// ErrNoVideo 解析出来的是图文/图集作品，没有视频
	ErrNoVideo = errors.New("linkparse: 这个作品里没有视频")
)

type Client struct {
	conf Conf
	hc   *http.Client
}

func New(c Conf) *Client {
	t := c.TimeoutSeconds
	if t <= 0 {
		t = 60
	}
	return &Client{conf: c, hc: &http.Client{Timeout: time.Duration(t) * time.Second}}
}

func (c *Client) Enabled() bool { return c != nil && strings.TrimSpace(c.conf.BaseURL) != "" }

// Video 解析出来的视频作品。
type Video struct {
	URL       string  // 直链，带签名会过期，拿到就下
	UserAgent string  // 非空时下载必须带上（目前只有 B站）
	Title     string  // 作品标题，抖音/快手通常就是文案
	CoverURL  string  // 封面图
	Duration  float64 // 时长（秒），部分平台拿不到，为 0
	SourceURL string  // 归一化后的规范链接（短链已展开），存库用这个
	Platform  string
}

type mediaItem struct {
	URL       string  `json:"url"`
	Kind      string  `json:"kind"`
	Duration  float64 `json:"duration"`
	UserAgent string  `json:"user_agent"`
}

type parseResult struct {
	Platform  string      `json:"platform"`
	SourceURL string      `json:"source_url"`
	Title     string      `json:"title"`
	Cover     *mediaItem  `json:"cover"`
	Medias    []mediaItem `json:"medias"`
	NeedsMux  bool        `json:"needs_mux"`
}

// Parse 解析一段分享链接或分享文案。
func (c *Client) Parse(ctx context.Context, shared string) (*Video, error) {
	body, err := json.Marshal(map[string]any{
		"url": shared,
		// 固定走「优先速度」：只影响 B站，换来单文件、不用合流
		"prefer_quality": false,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.conf.BaseURL, "/")+"/parse", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return nil, ErrUnsupported
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d %s", ErrUpstream, resp.StatusCode, truncate(string(raw), 200))
	}
	var r parseResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("linkparse: 返回解析失败: %s", truncate(string(raw), 200))
	}
	// 音视频分离的（B站 DASH）直接下会是哑片。传了 prefer_quality=false 一般不会走到这，兜个底
	if r.NeedsMux {
		return nil, fmt.Errorf("%w: 音视频分离", ErrNoVideo)
	}
	m := firstVideo(r.Medias)
	if m == nil {
		return nil, ErrNoVideo
	}
	v := &Video{
		URL: m.URL, UserAgent: m.UserAgent, Title: strings.TrimSpace(r.Title),
		Duration: m.Duration, SourceURL: r.SourceURL, Platform: r.Platform,
	}
	if r.Cover != nil {
		v.CoverURL = r.Cover.URL
	}
	return v, nil
}

func firstVideo(list []mediaItem) *mediaItem {
	for i := range list {
		if list[i].Kind == "video" && list[i].URL != "" {
			return &list[i]
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
