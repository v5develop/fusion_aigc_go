package aigateway

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/pkg/traceid"
)

// 默认值。
const (
	DefaultCallerID     = "fusion-go"
	DefaultKeyID        = "fusion-go-v1"
	DefaultTimeout      = 10 * time.Second
	DefaultLongPollWait = 25 * time.Second

	// UseDefaultWait 作为 wait 传入时使用 Config.LongPollWait。
	UseDefaultWait time.Duration = -1

	maxResponseBytes = 32 << 20
)

var (
	idempotencyKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:/#@-]{1,255}$`)
	callIDRe         = regexp.MustCompile(`^call_[0-9A-Za-z]{10,40}$`)
	reconcileIDRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Config 网关连接配置，对应 etc yaml 的 AiGateway 段。BaseURL 为空表示未接入，调用返回 ErrNotConfigured。
type Config struct {
	// BaseURL 如 http://ai-gateway:8080
	BaseURL string `json:",optional"`
	// CallerID 与密钥绑定的调用方
	CallerID string `json:",default=fusion-go"`
	// KeyID 密钥 ID
	KeyID string `json:",default=fusion-go-v1"`
	// Secret HMAC 密钥
	Secret string `json:",optional"`
	// Timeout 单次 HTTP 请求超时（长轮询请求另加等待时长）
	Timeout time.Duration `json:",default=10s"`
	// LongPollWait 传 UseDefaultWait 时的长轮询时长（上限 60s）
	LongPollWait time.Duration `json:",default=25s"`
}

// Client ai-gateway 客户端。并发安全。不做自动重试：提交是幂等的，重试与否由上层编排决定。
type Client struct {
	base     string
	signer   Signer
	timeout  time.Duration
	longPoll time.Duration
	hc       *http.Client

	// now / nonce 测试可替换
	now   func() time.Time
	nonce func() string
}

// New 创建客户端，空字段按默认值补齐。
func New(cfg Config) *Client {
	if cfg.CallerID == "" {
		cfg.CallerID = DefaultCallerID
	}
	if cfg.KeyID == "" {
		cfg.KeyID = DefaultKeyID
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.LongPollWait <= 0 {
		cfg.LongPollWait = DefaultLongPollWait
	}
	return &Client{
		base:     strings.TrimRight(cfg.BaseURL, "/"),
		signer:   Signer{CallerID: cfg.CallerID, KeyID: cfg.KeyID, Secret: cfg.Secret},
		timeout:  cfg.Timeout,
		longPoll: min(cfg.LongPollWait, MaxWait),
		// 超时按请求用 context 控制（长轮询要更长），这里不设 Client.Timeout
		hc:    &http.Client{Transport: newTransport()},
		now:   time.Now,
		nonce: NewNonce,
	}
}

// maxIdleConnsPerHost 到网关的空闲连接保留数。默认 Transport 只留 2 条：几十个长轮询协程（canvas Poll.Watchers、
// studio Poll.Workers）各挂一条连接，每轮长轮询返回后多出来的连接都被关掉、下一轮重新握手（TCP + TLS）。
const maxIdleConnsPerHost = 256

func newTransport() http.RoundTripper {
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t = t.Clone()
	t.MaxIdleConns = max(t.MaxIdleConns, maxIdleConnsPerHost)
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	return t
}

// Enabled 是否已配置地址。
func (c *Client) Enabled() bool { return c.base != "" }

// NewNonce crypto/rand 16 字节的 hex（32 位）。
func NewNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SubmitOptions 提交选项。
type SubmitOptions struct {
	// IdempotencyKey 必填，放在 Idempotency-Key 请求头。同一调用方内唯一、由业务确定性生成（如 job-123:render/0）
	IdempotencyKey string
	// Wait 长轮询到终态的最长时长（0 立即返回；UseDefaultWait 用配置值；上限 60s）
	Wait time.Duration
	// RetryFailed 对 retry_safe 的 FAILED 记录开启新一轮
	RetryFailed bool
	// ReconciledNotCharged 人工对账单号：仅与 RetryFailed 一起、用于重开 UNKNOWN 记录
	ReconciledNotCharged string
	// Traceparent W3C 链路头，可空
	Traceparent string
	// TraceID 创作任务号（afv_creation_task.task_id），放在 X-Trace-Id 请求头，网关据此把调用挂到任务轨迹上；可空
	TraceID string
}

// Submit 提交（或重放）一次调用。POST /v1/calls，scope=calls。
//
// 同键同内容返回已有记录（无论状态）；同键不同内容返回 409（errors.Is(err, ErrIdempotencyConflict)）。
func (c *Client) Submit(ctx context.Context, req CallRequest, opt SubmitOptions) (*Call, error) {
	if !idempotencyKeyRe.MatchString(opt.IdempotencyKey) {
		return nil, fmt.Errorf("aigateway: 幂等键非法 %q (check=aig.idempotency_key)", opt.IdempotencyKey)
	}
	if opt.ReconciledNotCharged != "" && (!opt.RetryFailed || !reconcileIDRe.MatchString(opt.ReconciledNotCharged)) {
		return nil, errors.New("aigateway: reconciled_not_charged 必须与 RetryFailed 同用且格式合法 (check=aig.reconciled_not_charged)")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("aigateway: 请求序列化失败: %w", err)
	}
	wait := c.waitSeconds(opt.Wait)
	q := url.Values{}
	if wait > 0 {
		q.Set("wait", strconv.Itoa(wait))
	}
	if opt.RetryFailed {
		q.Set("retry_failed", "true")
	}
	if opt.ReconciledNotCharged != "" {
		q.Set("reconciled_not_charged", opt.ReconciledNotCharged)
	}
	h := http.Header{"Idempotency-Key": {opt.IdempotencyKey}}
	if opt.Traceparent != "" {
		h.Set("traceparent", opt.Traceparent)
	}
	if t := strings.TrimSpace(opt.TraceID); t != "" {
		h.Set("X-Trace-Id", t)
	}
	var out Call
	if err := c.do(ctx, PurposeCalls, http.MethodPost, "/v1/calls", q, body, h, wait, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get 读取调用记录。GET /v1/calls/{id}，scope=calls。wait>0 时长轮询到终态（网关在每次状态变化时立即唤醒）。
func (c *Client) Get(ctx context.Context, id string, wait time.Duration) (*Call, error) {
	if !callIDRe.MatchString(id) {
		return nil, fmt.Errorf("aigateway: call id 非法 %q (check=aig.call_id)", id)
	}
	sec := c.waitSeconds(wait)
	q := url.Values{}
	if sec > 0 {
		q.Set("wait", strconv.Itoa(sec))
	}
	var out Call
	if err := c.do(ctx, PurposeCalls, http.MethodGet, "/v1/calls/"+id, q, nil, nil, sec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetByKey 按幂等键读取调用记录（只查不建）。GET /v1/calls?idempotency_key=…，scope=calls。
// 网关上没有这个键的调用 → 404（errors.Is(err, ErrNotFound)）；与 Submit 同键重放不同，绝不会新建或派发调用，
// 用于「确认此前的提交有没有在网关上建单」（取消 / 回收时不能自己制造一次供应商调用）。
func (c *Client) GetByKey(ctx context.Context, idempotencyKey string) (*Call, error) {
	if !idempotencyKeyRe.MatchString(idempotencyKey) {
		return nil, fmt.Errorf("aigateway: 幂等键非法 %q (check=aig.idempotency_key)", idempotencyKey)
	}
	q := url.Values{"idempotency_key": {idempotencyKey}}
	var out Call
	if err := c.do(ctx, PurposeCalls, http.MethodGet, "/v1/calls", q, nil, nil, 0, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BatchGet 批量读取（≤200）。POST /v1/calls:batchGet，scope=calls。
// 不存在或不属于本调用方的 id 不在结果里（网关在 missing 中列出，这里不返回，调用方按 id 比对即可）。
func (c *Client) BatchGet(ctx context.Context, ids []string) ([]Call, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > MaxBatchGet {
		return nil, fmt.Errorf("aigateway: batchGet 最多 %d 个 id，实际 %d (check=aig.batch_get_size)", MaxBatchGet, len(ids))
	}
	body, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items   []Call   `json:"items"`
		Missing []string `json:"missing"`
	}
	if err := c.do(ctx, PurposeCalls, http.MethodPost, "/v1/calls:batchGet", nil, body, nil, 0, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Cancel 取消调用。POST /v1/calls/{id}/cancel，scope=calls。幂等；终态记录原样返回。
// 未发送 → CANCELLED 零成本；本轮已有尝试发给供应商（且供应商不能止损）→ 409 CANCEL_NOT_ALLOWED
// （check=cancel.vendor_sent，用 IsCancelNotAllowed 判别），调用状态不变、继续运行，结果照常交付、按实际用量结算。
func (c *Client) Cancel(ctx context.Context, id string) (*Call, error) {
	if !callIDRe.MatchString(id) {
		return nil, fmt.Errorf("aigateway: call id 非法 %q (check=aig.call_id)", id)
	}
	var out Call
	if err := c.do(ctx, PurposeCalls, http.MethodPost, "/v1/calls/"+id+"/cancel", nil, []byte("{}"), nil, 0, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaxPrewarmInputs 单次预上传最多几个输入（网关契约 PrewarmRequest.inputs 上限）。
const MaxPrewarmInputs = 16

// 预上传结果状态（PrewarmItem.Status）。未知的新值按 skipped 看待。
const (
	PrewarmQueued  = "queued"
	PrewarmCached  = "cached"
	PrewarmSkipped = "skipped"
)

// PrewarmRequest 参考图预上传请求（POST /v1/media:prewarm）。Inputs 与提交时的 MediaRef 同形，传同一份（mr://{rid}+sha256）才能命中同一缓存。
type PrewarmRequest struct {
	ModelGroup string     `json:"model_group"`
	Operation  string     `json:"operation"`
	Inputs     []MediaRef `json:"inputs"`
}

// PrewarmItem 每个输入的受理结果（宽容读：只取用到的字段）。
type PrewarmItem struct {
	URI    string `json:"uri"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Prewarm 把参考图提前上传到供应商（只为提速：不建调用、不计费，失败不影响之后的提交）。POST /v1/media:prewarm，scope=calls。
// 网关立即返回 202，实际上传在网关后台执行。旧版网关没有该接口 → 404（errors.Is(err, ErrNotFound)），调用方应关闭此功能。
func (c *Client) Prewarm(ctx context.Context, req PrewarmRequest) ([]PrewarmItem, error) {
	if len(req.Inputs) == 0 {
		return nil, nil
	}
	if len(req.Inputs) > MaxPrewarmInputs {
		return nil, fmt.Errorf("aigateway: prewarm 最多 %d 个输入，实际 %d (check=aig.prewarm_size)", MaxPrewarmInputs, len(req.Inputs))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []PrewarmItem `json:"items"`
	}
	if err := c.do(ctx, PurposeCalls, http.MethodPost, "/v1/media:prewarm", nil, body, nil, 0, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ModelGroups 当前发布配置的模型组。GET /v1/catalog/model-groups，scope=catalog。
func (c *Client) ModelGroups(ctx context.Context) ([]ModelGroup, error) {
	var out struct {
		Items []ModelGroup `json:"items"`
	}
	if err := c.do(ctx, PurposeCatalog, http.MethodGet, "/v1/catalog/model-groups", nil, nil, nil, 0, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// ReadinessQuery readiness 的一项：Operation 为空表示模型组任一操作可路由即可；
// 建议显式带 operation（如 image.edit），网关会按该操作筛部署（不支持时 reason=operation.mismatch）。
type ReadinessQuery struct {
	ModelGroup string `json:"model_group"`
	Operation  string `json:"operation,omitempty"`
}

// Readiness 各模型组当前是否有可路由的部署（≤200）。POST /v1/catalog/readiness，scope=catalog。等价于不带 operation 的 ReadinessOp。
func (c *Client) Readiness(ctx context.Context, groupCodes []string) ([]ReadinessItem, error) {
	items := make([]ReadinessQuery, len(groupCodes))
	for i, g := range groupCodes {
		items[i] = ReadinessQuery{ModelGroup: g}
	}
	return c.ReadinessOp(ctx, items)
}

// ReadinessOp 同 Readiness，每项可指定 operation。
func (c *Client) ReadinessOp(ctx context.Context, items []ReadinessQuery) ([]ReadinessItem, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > MaxBatchGet {
		return nil, fmt.Errorf("aigateway: readiness 最多 %d 个模型组，实际 %d", MaxBatchGet, len(items))
	}
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []ReadinessItem `json:"items"`
	}
	if err := c.do(ctx, PurposeCatalog, http.MethodPost, "/v1/catalog/readiness", nil, body, nil, 0, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Costs 读取成本流水。GET /v1/ledger/costs，scope=ledger。
//
// 注意：fusion-go 的密钥只有 calls、catalog 两个 scope，调用会得到 403 FORBIDDEN_SCOPE。
// 成本对账目前归 fusion-java；此方法保留接口，待网关给 fusion-go 授予 ledger 后可直接使用。
func (c *Client) Costs(ctx context.Context, query CostsQuery) (*CostLedgerPage, error) {
	q := url.Values{"after_id": {strconv.FormatInt(query.AfterID, 10)}}
	if query.Limit > 0 {
		q.Set("limit", strconv.Itoa(query.Limit))
	}
	var out CostLedgerPage
	if err := c.do(ctx, PurposeLedger, http.MethodGet, "/v1/ledger/costs", q, nil, nil, 0, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// waitSeconds 把等待时长换成 wait 参数（秒，0–60）。
func (c *Client) waitSeconds(d time.Duration) int {
	if d == UseDefaultWait {
		d = c.longPoll
	}
	if d <= 0 {
		return 0
	}
	return int(min(d, MaxWait) / time.Second)
}

// do 签名、发送并解析响应。2xx 解到 out；其余返回 *Error。waitSec 会加到本次请求的超时上。
func (c *Client) do(ctx context.Context, purpose, method, path string, q url.Values, body []byte, h http.Header, waitSec int, out any) error {
	if !c.Enabled() {
		return ErrNotConfigured
	}
	u := c.base + path
	if raw := CanonicalValues(q); raw != "" {
		// 线上 query 直接用规范形式，与签名逐字一致
		u += "?" + raw
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout+time.Duration(waitSec)*time.Second)
	defer cancel()

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return fmt.Errorf("aigateway: 构造请求失败: %w", err)
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 没显式给追踪号时沿用入站请求的 X-Trace-Id（Java → Go → 网关同一个号）
	traceid.Inject(ctx, req.Header)
	if err := c.signer.Sign(req, purpose, body, c.now(), c.nonce()); err != nil {
		return err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return transportErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return transportErr(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("aigateway: 响应解析失败(HTTP %d): %w: %s", resp.StatusCode, err, truncate(string(raw), 300))
	}
	return nil
}

// transportErr 网络错误包成 *Error（Temporary）。调用方自己取消的 ctx 原样返回取消原因，便于上层区分。
func transportErr(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	return &Error{Code: CodeTransport, Message: err.Error(), RetrySafe: true, Err: err}
}
