// Package llm 文本模型调用。模型与密钥来自数据库（afv_ai_model / afv_ai_provider），
// 不写在配置文件里，换模型只改 afv_system_config.ai_writing_model_id。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Endpoint 一次调用所需的全部连接信息。
type Endpoint struct {
	Platform string // dashscope 等
	BaseURL  string // 如 https://dashscope.aliyuncs.com
	APIKey   string
	Model    string // 如 qwen3-vl-plus
	Timeout  time.Duration
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Client struct {
	hc *http.Client
}

func New() *Client {
	return &Client{hc: &http.Client{}}
}

// Chat 走 OpenAI 兼容协议（阿里云百炼的 compatible-mode）。
func (c *Client) Chat(ctx context.Context, ep Endpoint, system, user string) (string, error) {
	if ep.APIKey == "" {
		return "", fmt.Errorf("llm: 模型 %s 未配置 api_key", ep.Model)
	}
	body, _ := json.Marshal(map[string]any{
		"model": ep.Model,
		"messages": []Message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL(ep), bytes.NewReader(body))
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: %s 返回 %d: %s", ep.Model, resp.StatusCode, truncate(string(raw), 500))
	}
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: 解析响应失败: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("llm: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: %s 没有返回内容", ep.Model)
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// chatURL 百炼的 OpenAI 兼容地址是 {base}/compatible-mode/v1/chat/completions；
// 其他平台默认 {base}/v1/chat/completions。
func chatURL(ep Endpoint) string {
	base := strings.TrimRight(ep.BaseURL, "/")
	switch {
	case strings.Contains(base, "/chat/completions"):
		return base
	case ep.Platform == "dashscope":
		return base + "/compatible-mode/v1/chat/completions"
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
