// Package colorfixsolver 是 tools/colorfix-solver（参考图驱动的自动校色求解服务）的 HTTP 客户端。
//
// 求解服务只返回五个滑杆值（hue / saturation / value / brightness / contrast），不出图；调用方把结果当图层初值，
// 成片仍走现有精算导出。协议见 tools/colorfix-solver/README.md：POST /v1/solve + Bearer token，429 忙 / 504 超时 / 422 蒙版不可用。
// 求解端自己下载图片，所以交给它的地址必须是它拿得到的（OSS 域名要进它的 COLORFIX_FETCH_ALLOW_HOSTS，集群内可开 ALLOW_PRIVATE_FETCH）。
package colorfixsolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Conf 配置块（studio-rpc 的 ColorFixSolver）。Endpoint 为空 = 功能关闭。
type Conf struct {
	// Endpoint 求解服务根地址，如 http://colorfix-solver:18090（不带 /v1/solve）；空 = 关闭
	Endpoint string `json:",optional"`
	// Token 与求解服务 COLORFIX_TOKEN 一致的 Bearer token
	Token string `json:",optional"`
	// TimeoutSeconds 单次求解的 HTTP 总超时；求解端自己的 deadline 是 20s，这里留出下载与排队余量
	TimeoutSeconds int `json:",default=40"`
	// MaxConcurrent 本进程同时在途的求解请求上限；满了立刻按「忙」返回，不排队
	MaxConcurrent int `json:",default=4"`
}

// Kind 错误类别。
type Kind string

const (
	KindBusy        Kind = "busy"        // 求解端 429 或本地并发满：稍后重试
	KindTimeout     Kind = "timeout"     // 求解端 504 / 本地超时：可重试
	KindUnusable    Kind = "unusable"    // 求解端 422：蒙版不可用（空 / 全满），重试结果一样
	KindUnavailable Kind = "unavailable" // 连不上 / 5xx / 401 / 403 / 400 等：配置或服务问题，用户侧只能稍后再试
)

// Error 调用失败；Status 为 0 表示没拿到 HTTP 响应。
type Error struct {
	Kind    Kind
	Status  int
	Code    string // 求解端 error.code
	Message string // 求解端 error.message 或本地原因（只进日志，不给用户）
}

func (e *Error) Error() string {
	return fmt.Sprintf("colorfix-solver %s (status=%d code=%s): %s", e.Kind, e.Status, e.Code, e.Message)
}

// KindOf 取错误类别；不是 *Error 返回空串。
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}

// Ref 一件单品的参考图（可带参考图自己的蒙版，当前不用）。
type Ref struct {
	Key     string `json:"key"`
	URL     string `json:"url"`
	MaskURL string `json:"mask_url,omitempty"`
}

// Mask 一件单品在成品里的蒙版。
type Mask struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

// Request 求解请求。
type Request struct {
	ImageURL  string   `json:"image_url"`
	Reference []Ref    `json:"reference"`
	Masks     []Mask   `json:"masks"`
	Strength  *float64 `json:"strength,omitempty"`
}

// Params 五参数；求解端返回整数，这里按 float 解以容忍小数。
type Params struct {
	Hue        float64 `json:"hue"`
	Saturation float64 `json:"saturation"`
	Value      float64 `json:"value"`
	Brightness float64 `json:"brightness"`
	Contrast   float64 `json:"contrast"`
}

// Piece 一件单品的结果。
type Piece struct {
	Key    string  `json:"key"`
	Params Params  `json:"params"`
	Score  float64 `json:"score"`
}

// Response 求解结果（宽松解码：只拿用到的字段）。
type Response struct {
	Pieces  []Piece `json:"pieces"`
	Version string  `json:"version"`
}

// Client 求解服务客户端。零值 / nil 都是「关闭」。
type Client struct {
	endpoint string
	token    string
	http     *http.Client
	sem      chan struct{}
}

// maxRespBytes 响应体上限：结果只有几百字节，超过说明对端不对。
const maxRespBytes = 1 << 20

// New 按配置建客户端；Endpoint 为空返回 nil（Enabled()=false）。
func New(c Conf) *Client {
	ep := strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	if ep == "" {
		return nil
	}
	to := time.Duration(c.TimeoutSeconds) * time.Second
	if to <= 0 {
		to = 40 * time.Second
	}
	n := c.MaxConcurrent
	if n <= 0 {
		n = 4
	}
	return &Client{
		endpoint: ep, token: strings.TrimSpace(c.Token), sem: make(chan struct{}, n),
		http: &http.Client{
			Timeout: to,
			// 求解端不会重定向；跟随重定向会把 Bearer token 带去别处
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Enabled 功能是否开启。
func (c *Client) Enabled() bool { return c != nil && c.endpoint != "" }

// Solve 调求解服务。并发满立刻返回 KindBusy（不排队，免得请求堆在 rpc 超时前）。
func (c *Client) Solve(ctx context.Context, req Request) (*Response, error) {
	if !c.Enabled() {
		return nil, &Error{Kind: KindUnavailable, Message: "未配置求解服务"}
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	default:
		return nil, &Error{Kind: KindBusy, Message: "本进程求解并发已满"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, &Error{Kind: KindUnavailable, Message: "编码请求失败: " + err.Error()}
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/solve", bytes.NewReader(body))
	if err != nil {
		return nil, &Error{Kind: KindUnavailable, Message: "组装请求失败: " + err.Error()}
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		hr.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	if len(raw) > maxRespBytes {
		return nil, &Error{Kind: KindUnavailable, Status: resp.StatusCode, Message: "响应体过大"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp.StatusCode, raw)
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &Error{Kind: KindUnavailable, Status: resp.StatusCode, Message: "响应不是合法 JSON: " + err.Error()}
	}
	return &out, nil
}

func transportError(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return &Error{Kind: KindTimeout, Message: err.Error()}
	}
	return &Error{Kind: KindUnavailable, Message: err.Error()}
}

func statusError(status int, raw []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = truncate(string(raw), 200)
	}
	k := KindUnavailable
	switch status {
	case http.StatusTooManyRequests:
		k = KindBusy
	case http.StatusGatewayTimeout, http.StatusRequestTimeout:
		k = KindTimeout
	case http.StatusUnprocessableEntity:
		k = KindUnusable
	}
	return &Error{Kind: k, Status: status, Code: env.Error.Code, Message: msg}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
