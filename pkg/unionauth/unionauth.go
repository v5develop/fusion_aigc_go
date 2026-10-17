// Package unionauth 用联盟（xxlm）的**临时 token** 换取用户信息，用于 App 登录。
//
// 整条链路两步，第一步在前端做：
//  1. 前端：GET {base}/api/v1/user/auth/token，带用户的联盟 token（Authorization: Bearer）→ 临时 token
//     临时 token 是 32 位随机串，在对方 Redis 里存 5 分钟，**用一次就删**。
//  2. 我们：GET {base}/api/v1/auth/{临时token}/user → 用户信息（手机号是脱敏的）
//
// 也就是说我们只做第二步，拿到的凭证是一次性的，只够登录用一次。
package unionauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Conf struct {
	// BaseURL 联盟用户服务地址，如 https://agent.pengqiwl.com
	BaseURL        string `json:",optional"`
	TimeoutSeconds int    `json:",default=10"`
}

// ErrRejected 联盟明确拒绝：HTTP 正常，但业务码不成功（token 无效 / 已过期），或用户信息缺 userId。
// 对应 Java 的 PARAM_INVALID（1003），文案取联盟返回的 message。
var ErrRejected = errors.New("unionauth: 联盟拒绝")

// ErrUnavailable 联盟不可用：传输失败、HTTP 非 2xx（含 401/403）、响应解析失败、没有 data。
// 对应 Java 的 THIRD_PARTY_ERROR（6001）。Java 的 RestTemplate 对 4xx/5xx 也是抛异常，口径一致。
var ErrUnavailable = errors.New("unionauth: 联盟服务不可用")

// RejectedError 带上联盟给的 message，供登录接口原样返回。
type RejectedError struct{ Message string }

func (e *RejectedError) Error() string { return "unionauth: 联盟拒绝: " + e.Message }

// Is 让 errors.Is(err, ErrRejected) 成立。
func (e *RejectedError) Is(target error) bool { return target == ErrRejected }

type Client struct {
	conf Conf
	hc   *http.Client
}

// New 建客户端。只走联盟的 HTTP 接口，与 Java 的 UnionTokenAuthService 相同：不直连联盟库，也不读联盟 Redis。
func New(c Conf) *Client {
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{conf: c, hc: &http.Client{Timeout: timeout}}
}

// Enabled 没配 BaseURL 就等于登录不可用，接口直接拒绝而不是去打空地址。
func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.conf.BaseURL) != ""
}

// User 联盟返回的用户信息。Phone 是脱敏的（如 130****7881），拿不到真实号码。
//
// 对方现在返回的就是这 12 个字段，**没有点数、权益之类的资产字段**。
// 以后他们加了字段，这里解析时会自动忽略；真要用也只能当展示值，
// 不能拿它去改点数或权益（见 appuser.syncProfile 的说明）。
type User struct {
	UserID         json.Number `json:"userId"` // 联盟用户 ID，很大，用字符串存
	Phone          string      `json:"phone"`
	Nickname       string      `json:"nickname"`
	Avatar         string      `json:"avatar"`
	Gender         int32       `json:"gender"`
	Birthday       string      `json:"birthday"`
	Province       string      `json:"province"`
	City           string      `json:"city"`
	RealNameStatus string      `json:"realNameStatus"`
	Status         string      `json:"status"` // active / frozen / deactivating / deactivated
	CreatedAt      string      `json:"createdAt"`
	Roles          []string    `json:"roles"`
}

// Login 用**临时 token** 换用户信息。
//
// 临时 token 由前端自己去联盟换（GET /api/v1/user/auth/token，带联盟 token），
// 传给我们的就是它。它 5 分钟有效、**用一次就失效**，所以只能用在登录这一下，
// 不能拿来当每次请求的凭证——登录成功后用我们签发的令牌。
func (c *Client) Login(ctx context.Context, tempToken string) (*User, error) {
	var u User
	// 临时 token 在路径里；用一次即失效
	if err := c.get(ctx, "/api/v1/auth/"+url.PathEscape(tempToken)+"/user", "", &u); err != nil {
		return nil, err
	}
	if u.UserID.String() == "" || u.UserID.String() == "0" {
		return nil, &RejectedError{Message: "联盟用户ID无效"}
	}
	return &u, nil
}

// get 调一次联盟接口并解开它的外层（code/message/data，成功码是 1）。
func (c *Client) get(ctx context.Context, path, bearer string, data any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.conf.BaseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(bearer, "Bearer "))
	}
	req.Header.Set("Accept-Language", "zh-CN")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: 请求 %s 失败: %v", ErrUnavailable, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: 读取 %s 响应失败: %v", ErrUnavailable, path, err)
	}
	// 非 2xx（含 401/403）一律按「联盟不可用」，与 Java 的 RestTemplate 抛异常一致
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s 返回 HTTP %d", ErrUnavailable, path, resp.StatusCode)
	}
	var body struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("%w: %s 响应格式异常: %s", ErrUnavailable, path, truncate(string(raw), 200))
	}
	if body.Code != okCode {
		msg := body.Message
		if msg == "" {
			msg = "Token无效或已过期"
		}
		return &RejectedError{Message: msg}
	}
	if len(body.Data) == 0 {
		return fmt.Errorf("%w: %s 没有返回 data", ErrUnavailable, path)
	}
	if err := json.Unmarshal(body.Data, data); err != nil {
		return fmt.Errorf("%w: %s data 解析失败: %v", ErrUnavailable, path, err)
	}
	return nil
}

// okCode 联盟接口的成功码是 1（不是 0）
const okCode = 1

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
