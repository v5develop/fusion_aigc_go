// Package javatool 调 Java 主项目（fusion-aigc）的 creaft v1 兼容接口：
//
//	POST /api/v1/tools/ai-image-upscaler  {imageUrl, resolution}                     高清放大
//	POST /api/v1/tools/ai-ratio-convert   {imageUrls, resolution+aspectRatio | width+height}  扩图（比例转换）
//	POST /api/v1/tools/ai-inpaint         {images:[{url,bbox:[{x,y}×4]}], content, num}       局部重绘
//	GET  /api/v1/tasks/{taskUid}?original=true                                    任务查询（status / result.url）
//	GET  /api/v1/features/image-model-configs | video-model-configs               按用户报价的模型价目
//
// 出处：fusion-aigc controller/compat/v1/coretools/CreaftV1CoreToolsController.java、
// service/compat/v1/coretools/CoreToolSubmissionRequest.java、controller/compat/v1/task/CreaftV1TaskController.java。
//
// 字段名：请求体按 Java 记录 CoreToolSubmissionRequest 的驼峰字段名发（Jackson 按驼峰绑定；兼容层的 v1 请求校验器
// 会先把键转成 snake_case 再按 v1 模型校验，所以驼峰两边都过）。响应信封是 Java CommonResult {code,msg,data}，code=0 成功；
// data 的键按驼峰（taskId / taskUid）解析，同时兼容 v1 的下划线写法（task_id / task_uid）。
//
// 鉴权：每个请求原样带用户的 Authorization（Bearer 原文由网关经 gRPC metadata 透传过来）。Java 按该用户计费、
// 记任务中心、入素材库；Go 侧只记 node run 与结果引用。提交带 Idempotency-Key，同键重试不重复扣费。
package javatool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 兼容工具路径。
const (
	PathUpscale  = "/api/v1/tools/ai-image-upscaler"
	PathOutpaint = "/api/v1/tools/ai-ratio-convert"
	PathInpaint  = "/api/v1/tools/ai-inpaint"
	pathTask     = "/api/v1/tasks/"
	pathImageCfg = "/api/v1/features/image-model-configs"
	pathVideoCfg = "/api/v1/features/video-model-configs"
	// pathCreationTasks 非工作流任务的网关调用关联：{taskId}/gateway-calls（画布直接调网关时回报调用号）。
	pathCreationTasks = "/api/v1/creation/tasks/"
)

// v1 任务状态（CompatV1TaskStatus）。
const (
	StatusPending   = "PENDING"
	StatusRunning   = "RUNNING"
	StatusWaiting   = "WAITING_MANUAL_PROCESS"
	StatusSucceed   = "SUCCEED"
	StatusFailed    = "FAILED"
	StatusTimeout   = "TIMEOUT"
	StatusCancelled = "CANCELLED"
)

// ErrNotConfigured 未配置 Java.BaseURL。
var ErrNotConfigured = errors.New("javatool: 未配置 Java.BaseURL")

// ErrNoBearer 没有用户凭证（无法代用户调用）。
var ErrNoBearer = errors.New("javatool: 缺少用户 Bearer")

// Error Java 返回的业务错误或 HTTP 错误。
type Error struct {
	HTTPStatus int
	Code       int    // CommonResult.code（业务码；0 表示 HTTP 层错误没有信封）
	Reason     string // CommonResult.reason（ErrorCode 常量名，如 INSUFFICIENT_POINTS）
	Msg        string
}

func (e *Error) Error() string {
	return fmt.Sprintf("javatool: HTTP %d code=%d %s %s", e.HTTPStatus, e.Code, e.Reason, e.Msg)
}

// Temporary 网络 / 5xx / 429 为暂时性错误。
func (e *Error) Temporary() bool {
	return e.HTTPStatus == 0 || e.HTTPStatus == http.StatusTooManyRequests || e.HTTPStatus >= 500
}

// Unauthorized 401 / 2001：用户凭证失效。
func (e *Error) Unauthorized() bool { return e.HTTPStatus == http.StatusUnauthorized || e.Code == 2001 }

// Client Java 兼容接口客户端。
type Client struct {
	base string
	hc   *http.Client
}

// New base 为空时 Enabled()=false。
func New(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{base: strings.TrimRight(strings.TrimSpace(base), "/"), hc: &http.Client{Timeout: timeout}}
}

// Enabled 是否已配置地址。
func (c *Client) Enabled() bool { return c != nil && c.base != "" }

// RatioPoint 比例坐标（0–1）。
type RatioPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// InpaintImage ai-inpaint 的一张图：bbox 恰好 4 点，顺序左上、右上、右下、左下。
type InpaintImage struct {
	URL  string       `json:"url"`
	BBox []RatioPoint `json:"bbox"`
}

// UpscaleReq ai-image-upscaler。
type UpscaleReq struct {
	Title      string `json:"title,omitempty"`
	ImageURL   string `json:"imageUrl"`
	Resolution string `json:"resolution"` // 1k / 2k / 4k
}

// RatioConvertReq ai-ratio-convert：resolution+aspectRatio 与 width+height 二选一。
type RatioConvertReq struct {
	Title       string   `json:"title,omitempty"`
	ImageURLs   []string `json:"imageUrls"`
	Resolution  string   `json:"resolution,omitempty"`
	AspectRatio string   `json:"aspectRatio,omitempty"`
	Width       int      `json:"width,omitempty"`
	Height      int      `json:"height,omitempty"`
}

// InpaintReq ai-inpaint。
type InpaintReq struct {
	Title   string         `json:"title,omitempty"`
	Images  []InpaintImage `json:"images"`
	Content string         `json:"content"`
	Num     int            `json:"num,omitempty"`
}

// Submission 提交回执。
type Submission struct {
	TaskID  string
	TaskIDs []string
}

// Submit 提交一个兼容工具。path 为 PathUpscale / PathOutpaint / PathInpaint。
func (c *Client) Submit(ctx context.Context, bearer, path string, body any, idem string) (*Submission, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrNoBearer
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.SubmitRaw(ctx, bearer, path, b, idem)
}

// SubmitRaw 按原始字节提交（画布首次发送前把这些字节落库，同幂等键重提 / 找回任务号时逐字节重发）。
func (c *Client) SubmitRaw(ctx context.Context, bearer, path string, b []byte, idem string) (*Submission, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrNoBearer
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if idem != "" {
		h.Set("Idempotency-Key", idem)
	}
	data, err := c.do(ctx, http.MethodPost, path, nil, bearer, h, b)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("javatool: 提交回执无法解析: %w", err)
	}
	s := &Submission{TaskID: str(m, "taskId", "task_id")}
	for _, v := range list(m, "taskIds", "task_ids") {
		if t, ok := v.(string); ok && t != "" {
			s.TaskIDs = append(s.TaskIDs, t)
		}
	}
	if s.TaskID == "" && len(s.TaskIDs) > 0 {
		s.TaskID = s.TaskIDs[0]
	}
	if s.TaskID == "" {
		return nil, errors.New("javatool: 提交回执缺少 taskId")
	}
	return s, nil
}

// Task 一个任务的状态。
type Task struct {
	TaskUID      string
	Status       string
	ResultURL    string
	ErrorCode    string
	ErrorMessage string
}

// Terminal 是否终态（WAITING_MANUAL_PROCESS 不算）。
func (t *Task) Terminal() bool {
	switch t.Status {
	case StatusSucceed, StatusFailed, StatusTimeout, StatusCancelled:
		return true
	}
	return false
}

// Succeeded 成功且有产物。
func (t *Task) Succeeded() bool { return t.Status == StatusSucceed }

// GetTask GET /api/v1/tasks/{taskUid}?original=true（下载用地址，不退回缩略图）。
func (c *Client) GetTask(ctx context.Context, bearer, taskUID string) (*Task, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrNoBearer
	}
	q := url.Values{"original": {"true"}}
	data, err := c.do(ctx, http.MethodGet, pathTask+url.PathEscape(taskUID), q, bearer, nil, nil)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("javatool: 任务详情无法解析: %w", err)
	}
	t := &Task{
		TaskUID: str(m, "taskUid", "task_uid"), Status: strings.ToUpper(str(m, "status")),
		ErrorCode: str(m, "errorCode", "error_code"), ErrorMessage: str(m, "errorMessage", "error_message"),
	}
	if r, ok := m["result"].(map[string]any); ok {
		t.ResultURL = str(r, "url")
	}
	return t, nil
}

// ModelConfigs 用户的图片 / 视频模型价目（data 原文）。
func (c *Client) ModelConfigs(ctx context.Context, bearer string, video bool) (json.RawMessage, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrNoBearer
	}
	p := pathImageCfg
	if video {
		p = pathVideoCfg
	}
	return c.do(ctx, http.MethodGet, p, nil, bearer, nil, nil)
}

// do 发请求并拆 CommonResult 信封，返回 data 原文。
func (c *Client) do(ctx context.Context, method, path string, q url.Values, bearer string, h http.Header, body []byte) (json.RawMessage, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", AuthHeader(bearer))
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var env struct {
		Code   *int            `json:"code"`
		Reason string          `json:"reason"`
		Msg    string          `json:"msg"`
		Data   json.RawMessage `json:"data"`
	}
	if jerr := json.Unmarshal(raw, &env); jerr != nil || env.Code == nil {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &Error{HTTPStatus: resp.StatusCode, Msg: msg}
	}
	if *env.Code != 0 || resp.StatusCode >= 400 {
		return nil, &Error{HTTPStatus: resp.StatusCode, Code: *env.Code, Reason: env.Reason, Msg: env.Msg}
	}
	return env.Data, nil
}

// AuthHeader 规范成「Bearer <token>」。
func AuthHeader(bearer string) string {
	b := strings.TrimSpace(bearer)
	if len(b) > 7 && strings.EqualFold(b[:7], "Bearer ") {
		return "Bearer " + strings.TrimSpace(b[7:])
	}
	return "Bearer " + b
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

func list(m map[string]any, keys ...string) []any {
	for _, k := range keys {
		if v, ok := m[k].([]any); ok {
			return v
		}
	}
	return nil
}

// LinkGatewayCalls 把画布直接调网关得到的调用号回报给 Java（POST /api/v1/creation/tasks/{taskId}/gateway-calls）。
// Java 侧按调用号去重，重复回报只留一条，所以失败可以安全重试。
func (c *Client) LinkGatewayCalls(ctx context.Context, bearer, taskID string, callIDs []string) error {
	if !c.Enabled() {
		return ErrNotConfigured
	}
	if bearer == "" {
		return ErrNoBearer
	}
	if taskID == "" || len(callIDs) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"callIds": callIDs})
	if err != nil {
		return err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	_, err = c.do(ctx, http.MethodPost, pathCreationTasks+url.PathEscape(taskID)+"/gateway-calls", nil, bearer, h, body)
	return err
}

// runTaskPattern 画布运行的任务号：cv_<Java 创作任务号>_run_<4 位十六进制>（aigc.NewLifeTaskID，ref 段即 Java 任务号）。
var runTaskPattern = regexp.MustCompile(`^cv_(.+)_run_[0-9a-f]{4}$`)

// JavaTaskIDOf 从画布运行的任务号取出 Java 创作任务号；不符合格式返回空串。
func JavaTaskIDOf(runTaskID string) string {
	m := runTaskPattern.FindStringSubmatch(runTaskID)
	if m == nil {
		return ""
	}
	return m[1]
}

// QuoteSuffix 报价接口 = 对应提交路径 + "/quote"（Java 只读报价，请求体与提交完全相同，不受理、不预扣）。
const QuoteSuffix = "/quote"

// ErrQuoteUnsupported Java 没有这个报价接口（旧版本，404 / 405）：调用方回落「价格未知」，不要求与 Java 同批上线。
var ErrQuoteUnsupported = errors.New("javatool: Java 未提供该工具的报价接口")

// QuoteResult Java 报价（提交时会冻结的点数）。
type QuoteResult struct {
	Points           int64
	PriceUnit        string
	Quantity         int
	FreeQuotaApplied bool
}

// Quote POST {path}/quote：body 与提交同一份。返回 Java 按其价目算出的点数。
// 老版本 Java 没有该接口（404 / 405）→ ErrQuoteUnsupported。
func (c *Client) Quote(ctx context.Context, bearer, path string, body any) (*QuoteResult, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return nil, ErrNoBearer
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	data, err := c.do(ctx, http.MethodPost, path+QuoteSuffix, nil, bearer, h, b)
	if err != nil {
		var je *Error
		if errors.As(err, &je) && (je.HTTPStatus == http.StatusNotFound || je.HTTPStatus == http.StatusMethodNotAllowed) {
			return nil, ErrQuoteUnsupported
		}
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("javatool: 报价回执无法解析: %w", err)
	}
	pts, ok := m["points"].(float64)
	if !ok || pts < 0 {
		return nil, errors.New("javatool: 报价回执缺少 points")
	}
	q := &QuoteResult{Points: int64(pts)}
	q.FreeQuotaApplied, _ = m["freeQuotaApplied"].(bool)
	if pb, ok := m["priceBasis"].(map[string]any); ok {
		q.PriceUnit = str(pb, "priceUnit")
		if n, ok := pb["quantity"].(float64); ok {
			q.Quantity = int(n)
		}
	}
	return q, nil
}
