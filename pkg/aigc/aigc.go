// Package aigc AIGC 执行面的任务提交与结果轮询，对接文档见项目根目录《任务调用与轮询对接文档.md》。
//
// 流程：Submit 提交任务 → 每 5–10s 调一次 GetResult，直到 success / failed / not_found。
// 结果只保留今天+昨天两天，产物 URL 拿到后需自行落库。
package aigc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Conf 执行面连接配置。Endpoint 为空表示未接入，调用返回 ErrNotConfigured。
type Conf struct {
	// Endpoint 如 http://<aigc_server>:5000
	Endpoint string `json:",optional"`
	// TimeoutSeconds 单次 HTTP 请求超时（不是任务生成时长）
	TimeoutSeconds int `json:",default=10"`
}

var ErrNotConfigured = errors.New("aigc: 未配置 Endpoint")

// 轮询状态
const (
	StatusWaiting  = "waiting"
	StatusRunning  = "running"
	StatusSuccess  = "success"
	StatusFailed   = "failed"
	StatusNotFound = "not_found"
)

// Task 一种任务的专属参数。
type Task interface {
	TaskType() int
	Validate() error
}

type Client struct {
	endpoint string
	hc       *http.Client
}

func New(c Conf) *Client {
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{endpoint: strings.TrimRight(c.Endpoint, "/"), hc: &http.Client{Timeout: timeout}}
}

// Enabled 是否已配置地址。
func (c *Client) Enabled() bool { return c.endpoint != "" }

// NewTaskID 生成 <prefix>_<32位hex>。前缀用于执行面按 task_id.split('_')[0] 路由回调，不能含下划线。
func NewTaskID(prefix string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// NewLifeTaskID 生成本地生活任务提交给执行面的 task_id：
//
//	<prefix>_<任务中心 task_id>_<阶段>_<4位hex>
//
// 例：aigclife_AiCreation_life_vid_28_gen_a1b2。
// 为什么这样：现有系统任务中心里这条任务叫 AiCreation_life_vid_28，执行面的日志只认提交给它的 task_id，
// 两边不一致就没法拿任务中心的编号去查执行面日志。把任务中心的编号整段放进去，搜它就能找到（含各阶段）。
// 前缀仍是 prefix（执行面按 split('_')[0] 路由回调，不能变）；末尾 4 位 hex 保证同一阶段重做（重新构思等）时不撞号。
// ref 用 taskcenter.Ref 生成。
func NewLifeTaskID(prefix, ref, stage string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return prefix + "_" + ref + "_" + stage + "_" + hex.EncodeToString(b)
}

// SubmitResult 提交成功的返回。TaskType 是路由后的类型，可能与请求不同（85+lite→45、9 缺省→86、92+lite→91）。
type SubmitResult struct {
	TaskID      string `json:"task_id"`
	TaskType    int    `json:"task_type"`
	QueueLength int    `json:"queue_length"`
}

// Submit 提交任务。taskID 建议用 NewTaskID 生成并先落库；传空则由服务端生成，以返回值为准。
func (c *Client) Submit(ctx context.Context, taskID string, t Task) (*SubmitResult, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("aigc: task_type=%d %w", t.TaskType(), err)
	}
	body, err := buildBody(taskID, t)
	if err != nil {
		return nil, err
	}
	raw, status, err := c.do(ctx, http.MethodPost, c.endpoint+"/aigc/generate", body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Code    int           `json:"code"`
		Message string        `json:"message"`
		Success bool          `json:"success"`
		Data    *SubmitResult `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("aigc: 提交响应解析失败(HTTP %d): %s", status, truncate(string(raw), 300))
	}
	if status != http.StatusOK || !out.Success || out.Data == nil {
		return nil, fmt.Errorf("aigc: 提交失败(HTTP %d code=%d): %s", status, out.Code, out.Message)
	}
	return out.Data, nil
}

// Result 轮询结果。产物字段按任务不同：GeneratedImagePath（单产物）、ImagePaths（75）、Script（74）。
type Result struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
	// TaskType 路由后的类型，以此为准
	TaskType           int             `json:"task_type"`
	GeneratedImagePath string          `json:"generated_image_path"`
	ImagePaths         []string        `json:"image_paths"`
	Script             json.RawMessage `json:"script"`
	// Text / Duration 仅 70 视频解析：剧情文本与原片时长（秒）
	Text     string  `json:"text"`
	Duration float64 `json:"duration"`

	Pool string  `json:"pool"`
	Cost float64 `json:"cost"`

	// 失败时
	ErrorMsg   string `json:"error_msg"`
	ErrorCode  any    `json:"error_code"`
	Upstream   any    `json:"upstream"`
	Traceback  string `json:"traceback"`
	RetryCount int    `json:"retry_count"`
	Timestamp  string `json:"timestamp"`
}

// Done 是否已到终态（success / failed / not_found）。
func (r *Result) Done() bool {
	return r.Status == StatusSuccess || r.Status == StatusFailed || r.Status == StatusNotFound
}

// Outputs 所有产物 URL：75 取 ImagePaths，其余取 GeneratedImagePath；74 无文件返回空。
func (r *Result) Outputs() []string {
	if len(r.ImagePaths) > 0 {
		return r.ImagePaths
	}
	if r.GeneratedImagePath != "" {
		return []string{r.GeneratedImagePath}
	}
	return nil
}

// Err 失败/查不到时返回描述错误，其余返回 nil。
func (r *Result) Err() error {
	switch r.Status {
	case StatusFailed:
		return fmt.Errorf("aigc: 任务 %s 失败: %s", r.TaskID, r.ErrorMsg)
	case StatusNotFound:
		return fmt.Errorf("aigc: 任务 %s 不存在（结果只保留两天）", r.TaskID)
	}
	return nil
}

// GetResult 查询一次结果。不要用 /aigc/status（空壳接口）。
func (c *Client) GetResult(ctx context.Context, taskID string) (*Result, error) {
	if !c.Enabled() {
		return nil, ErrNotConfigured
	}
	raw, status, err := c.do(ctx, http.MethodGet, c.endpoint+"/aigc/getRes/"+url.PathEscape(taskID), nil)
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil || r.Status == "" {
		return nil, fmt.Errorf("aigc: 轮询响应无法识别(HTTP %d): %s", status, truncate(string(raw), 300))
	}
	return &r, nil
}

// Wait 按 interval 轮询直到终态或 ctx 结束。单次查询出错（网络抖动等）会继续重试。
// 返回终态结果；任务失败/查不到时 err 为 Result.Err()。
func (c *Client) Wait(ctx context.Context, taskID string, interval time.Duration) (*Result, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	var lastErr error
	for {
		r, err := c.GetResult(ctx, taskID)
		if errors.Is(err, ErrNotConfigured) {
			return nil, err
		}
		if err == nil && r.Done() {
			return r, r.Err()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return nil, fmt.Errorf("%w（最后一次查询: %v）", ctx.Err(), lastErr)
			}
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *Client) do(ctx context.Context, method, u string, body []byte) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("aigc: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("aigc: 读取响应失败: %w", err)
	}
	return raw, resp.StatusCode, nil
}

// buildBody 把任务参数平铺到顶层，并加上 task_id / task_type。
func buildBody(taskID string, t Task) ([]byte, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if taskID != "" {
		m["task_id"] = taskID
	}
	m["task_type"] = t.TaskType()
	return json.Marshal(m)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
