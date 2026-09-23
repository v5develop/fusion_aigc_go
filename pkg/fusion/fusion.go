// Package fusion 调现有 Java 服务（fusion_aigc）的联盟开放接口。
//
// 我们只用建档那两个：
//
//	GET  /api/v1/union/user-exists?unionUserId=...   用户是否已建档
//	POST /api/v1/union/user-register                 建档/同步资料，返回 userId
//
// 为什么要绕这一圈：建一个用户不只是写 afv_app_user，还要建团队、写对唯一键，
// 以后他们再加别的副作用我们也得跟着改。交给 Java 建，口径永远是一致的。
// 没配地址/密钥时不启用，由我们自己写表兜底（见 pkg/appuser）。
//
// 鉴权是 HMAC-SHA256 签名，口径照联盟侧现成的客户端实现
// （~/Documents/echo/xxlm/mall_user_svc_api/public/service/aigc）。
package fusion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Conf struct {
	// BaseURL Java 服务地址，如 http://127.0.0.1:8080；留空则不启用
	BaseURL string `json:",optional"`
	// AppKey / AppSecret 对方分配的应用密钥（afv_open_app / afv_open_app_secret）
	AppKey    string `json:",optional"`
	AppSecret string `json:",optional"`
	// KeyVersion 密钥轮换时用；留空则不带这个头，由对方按应用上的默认版本取
	KeyVersion     string `json:",optional"`
	TimeoutSeconds int    `json:",default=10"`
}

type Client struct {
	conf Conf
	hc   *http.Client
}

func New(c Conf) *Client {
	t := c.TimeoutSeconds
	if t <= 0 {
		t = 10
	}
	return &Client{conf: c, hc: &http.Client{Timeout: time.Duration(t) * time.Second}}
}

// Enabled 三样齐了才启用：地址、appKey、appSecret。
func (c *Client) Enabled() bool {
	return c != nil && strings.TrimSpace(c.conf.BaseURL) != "" &&
		strings.TrimSpace(c.conf.AppKey) != "" && strings.TrimSpace(c.conf.AppSecret) != ""
}

// RegisterReq 建档入参，字段名按对方的契约（驼峰）。
type RegisterReq struct {
	UnionUserID    string   `json:"unionUserId"`
	Nickname       string   `json:"nickname"`
	Avatar         string   `json:"avatar"`
	Phone          string   `json:"phone,omitempty"`
	Birthday       string   `json:"birthday,omitempty"`
	Gender         int32    `json:"gender"`
	Province       string   `json:"province,omitempty"`
	City           string   `json:"city,omitempty"`
	RealNameStatus string   `json:"realNameStatus,omitempty"`
	Roles          []string `json:"roles"`
	Status         string   `json:"status,omitempty"`
}

// UserExists 用户是否已建档。
func (c *Client) UserExists(ctx context.Context, unionUserID string) (bool, error) {
	var exists bool
	err := c.do(ctx, http.MethodGet, "/api/v1/union/user-exists",
		map[string]string{"unionUserId": unionUserID}, nil, &exists)
	return exists, err
}

// UserRegister 建档/同步资料，返回对方库里的 userId（就是 afv_app_user.id）。
//
// 并发下两个请求可能同时建同一个人，后到的会撞唯一键拿到 409。
// 这**不是**失败：请求本身合法，人已经建好了，调用方回查即可。
func (c *Client) UserRegister(ctx context.Context, req RegisterReq) (int64, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, err
	}
	var out struct {
		UserID int64 `json:"userId"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/union/user-register", nil, body, &out); err != nil {
		return 0, err
	}
	return out.UserID, nil
}

// APIError 带上对方的 HTTP 状态与业务码，方便判断能不能重试。
type APIError struct {
	HTTPStatus int
	Code       int
	Msg        string
}

func (e *APIError) Error() string {
	if e.HTTPStatus != 0 && e.HTTPStatus != http.StatusOK {
		return fmt.Sprintf("fusion: HTTP %d code %d %s", e.HTTPStatus, e.Code, e.Msg)
	}
	return fmt.Sprintf("fusion: code %d %s", e.Code, e.Msg)
}

// Duplicate 并发撞唯一键（对方幂等层的后到者）。人已经在了，回查就行。
func (e *APIError) Duplicate() bool { return e.HTTPStatus == http.StatusConflict }

// Permanent 重试也不会变的失败：参数不合法、签名错、没权限。
// 409（撞车）和 429（限流）说的是「现在不行」，不算。
func (e *APIError) Permanent() bool {
	if e.HTTPStatus == http.StatusConflict || e.HTTPStatus == http.StatusTooManyRequests {
		return false
	}
	return e.HTTPStatus >= 400 && e.HTTPStatus < 500
}

// IsDuplicate 解包判断是不是撞车。
func IsDuplicate(err error) bool {
	var api *APIError
	for err != nil {
		if a, ok := err.(*APIError); ok {
			api = a
			break
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return api != nil && api.Duplicate()
}

func (c *Client) do(ctx context.Context, method, uri string, query map[string]string, body []byte, data any) error {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce, err := genNonce()
	if err != nil {
		return err
	}
	canonQuery := canonicalQuery(query)
	sign := hmacSHA256(stringToSign(method, uri, canonQuery, sha256Hex(body), ts, nonce, c.conf.AppKey), c.conf.AppSecret)

	url := strings.TrimSuffix(c.conf.BaseURL, "/") + uri
	if canonQuery != "" {
		url += "?" + canonQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Union-App-Key", c.conf.AppKey)
	req.Header.Set("X-Union-Timestamp", ts)
	req.Header.Set("X-Union-Nonce", nonce)
	req.Header.Set("X-Union-Signature", sign)
	if c.conf.KeyVersion != "" {
		req.Header.Set("X-Union-Key-Version", c.conf.KeyVersion)
	}
	req.Header.Set("Content-Type", "application/json;charset=utf-8")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fusion: 请求 %s 失败: %w", uri, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env) // 非 200 时对方也会带 code/msg，解不出来就留空
	if resp.StatusCode != http.StatusOK {
		return &APIError{HTTPStatus: resp.StatusCode, Code: env.Code, Msg: env.Msg}
	}
	// 这套接口成功码是 0（注意和联盟用户服务的 1 不一样）
	if env.Code != 0 {
		return &APIError{HTTPStatus: http.StatusOK, Code: env.Code, Msg: env.Msg}
	}
	if data == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, data)
}
