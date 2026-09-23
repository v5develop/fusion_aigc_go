// Package unionauth 用联盟（xxlm）的 token 换取用户信息，用于 App 登录。
//
// 两步（都在 mall_user_svc_api）：
//  1. GET {base}/api/v1/user/auth/token，带用户的联盟 token（Authorization: Bearer）→ 临时 token
//     临时 token 是 32 位随机串，在对方 Redis 里存 5 分钟，**用一次就删**。
//  2. GET {base}/api/v1/auth/{临时token}/user → 用户信息（手机号是脱敏的）
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

// ErrUnauthorized 联盟 token 无效或已过期（对方返回非成功码），调用方应让用户重新登录。
var ErrUnauthorized = errors.New("unionauth: 联盟登录已失效")

type Client struct {
	conf Conf
	hc   *http.Client
}

func New(c Conf) *Client {
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{conf: c, hc: &http.Client{Timeout: timeout}}
}

// Enabled 没配地址时登录接口直接拒绝，不去打空地址。
func (c *Client) Enabled() bool { return strings.TrimSpace(c.conf.BaseURL) != "" }

// User 联盟返回的用户信息。Phone 是脱敏的（如 130****7881），拿不到真实号码。
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

// Login 用联盟 token 换用户信息（两步一起做）。
func (c *Client) Login(ctx context.Context, unionToken string) (*User, error) {
	temp, err := c.tempToken(ctx, unionToken)
	if err != nil {
		return nil, err
	}
	return c.userInfo(ctx, temp)
}

func (c *Client) tempToken(ctx context.Context, unionToken string) (string, error) {
	var out struct {
		TempToken string `json:"tempToken"`
	}
	err := c.get(ctx, "/api/v1/user/auth/token", unionToken, &out)
	if err != nil {
		return "", err
	}
	if out.TempToken == "" {
		return "", fmt.Errorf("unionauth: 没有拿到临时 token")
	}
	return out.TempToken, nil
}

func (c *Client) userInfo(ctx context.Context, tempToken string) (*User, error) {
	var u User
	// 临时 token 在路径里；用一次即失效
	if err := c.get(ctx, "/api/v1/auth/"+url.PathEscape(tempToken)+"/user", "", &u); err != nil {
		return nil, err
	}
	if u.UserID.String() == "" || u.UserID.String() == "0" {
		return nil, fmt.Errorf("unionauth: 用户信息里没有 userId")
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
		return fmt.Errorf("unionauth: 请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	var body struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("unionauth: %s 返回解析失败(HTTP %d): %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	if body.Code != okCode {
		// 对方的业务错误基本都是 token 失效 / 用户不存在，统一当登录失效处理
		return fmt.Errorf("%w（%s: %s）", ErrUnauthorized, path, body.Message)
	}
	if len(body.Data) == 0 {
		return fmt.Errorf("unionauth: %s 没有返回 data", path)
	}
	return json.Unmarshal(body.Data, data)
}

// okCode 联盟接口的成功码是 1（不是 0）
const okCode = 1

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
