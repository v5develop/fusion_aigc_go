// Package tts 语音合成：百炼 Qwen-Audio-TTS 非实时接口。
// 模型与密钥来自数据库（afv_system_config.ai_tts_model_id → afv_ai_model → afv_ai_provider），不写在配置文件里。
package tts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Endpoint 一次调用所需的全部连接信息。
type Endpoint struct {
	BaseURL string // 如 https://dashscope.aliyuncs.com
	APIKey  string
	Model   string // 如 qwen-audio-3.1-tts-flash
	Timeout time.Duration
}

// 合成参数固定：wav / 24kHz，下游数字人视频直接使用。
const (
	Format     = "wav"
	SampleRate = 24000
)

type Client struct {
	hc *http.Client
}

func New() *Client {
	return &Client{hc: &http.Client{}}
}

// Synthesize 合成一段语音，返回百炼上的音频地址（http，24 小时有效，调用方需尽快转存）。
// 网络层失败（连接超时等）自动重试一次；接口返回的业务错误不重试。
func (c *Client) Synthesize(ctx context.Context, ep Endpoint, text, voice string) (string, error) {
	if ep.APIKey == "" {
		return "", fmt.Errorf("tts: 模型 %s 未配置 api_key", ep.Model)
	}
	body, _ := json.Marshal(map[string]any{
		"model": ep.Model,
		"input": map[string]any{"text": text, "voice": voice, "format": Format, "sample_rate": SampleRate},
	})
	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		url, err := c.once(ctx, ep, body, timeout)
		if err == nil {
			return url, nil
		}
		lastErr = err
		var ne net.Error
		if !errors.As(err, &ne) || ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}

func (c *Client) once(ctx context.Context, ep Endpoint, body []byte, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := strings.TrimRight(ep.BaseURL, "/") + "/api/v1/services/audio/tts/SpeechSynthesizer"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Output  struct {
			Audio struct {
				URL string `json:"url"`
			} `json:"audio"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("tts: 解析响应失败(HTTP %d): %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if out.Code != "" || resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tts: %s 返回 %d %s: %s", ep.Model, resp.StatusCode, out.Code, out.Message)
	}
	if out.Output.Audio.URL == "" {
		return "", fmt.Errorf("tts: %s 没有返回音频", ep.Model)
	}
	return out.Output.Audio.URL, nil
}

// Download 下载音频（合成结果只保留 24 小时，需转存）。
func (c *Client) Download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: 下载音频失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts: 下载音频返回 %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 50<<20))
}

// WavDuration 计算 wav 时长。
//
// 百炼返回的 wav 头里 RIFF/data 长度是流式占位值 0x7FFFFFFF，不能信；
// 这里只取 fmt 块里的采样参数，数据长度按实际字节数算。
func WavDuration(b []byte) (time.Duration, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, errors.New("tts: 不是 wav 文件")
	}
	var byteRate uint32
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := binary.LittleEndian.Uint32(b[off+4 : off+8])
		body := off + 8
		switch id {
		case "fmt ":
			if body+12 > len(b) {
				return 0, errors.New("tts: wav fmt 块不完整")
			}
			byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
		case "data":
			if byteRate == 0 {
				return 0, errors.New("tts: wav 缺少 fmt 块")
			}
			n := len(b) - body
			return time.Duration(float64(n) / float64(byteRate) * float64(time.Second)), nil
		}
		off = body + int(size) + int(size%2)
	}
	return 0, errors.New("tts: wav 缺少 data 块")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
