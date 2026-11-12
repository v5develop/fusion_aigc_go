// Package reverse 反推 sidecar（tools/prompt-reverse/serve_reverse.py）的 Go 客户端。
//
// 七条路由：GET /api/status；POST /api/pose/source、/api/pose/prompt、/api/reverse/analyze、
// /api/reverse/analyze-batch、/api/reverse/crop-plan、/api/reverse/crop-check。字段以 sidecar 代码为准（见 types.go）。
//
// 约定：
//   - 图片一律传 data URL（用 DataURL 拼）；单张 ≤12MB、整个请求体 ≤24MB，超限在客户端直接拒绝（KindInvalid）。
//   - crop-plan 服务端只处理前 6 张：CropPlan 自动均匀分批（9 张 → 5+4，避免 compose 尾批只剩 1 张而走候选表模式），
//     并把 plans[].index 回映到原数组下标。crop-check 一次 ≤12 张：CropCheck 自动分批并回映。
//   - 所有 HTTP 调用共用一个信号量，同时在途不超过 Concurrency。
//   - 只对可重试错误（5xx、网络、单次超时）重试 MaxRetries 次；4xx / 未配 Key 不重试。
package reverse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout 单次 HTTP 超时：sidecar 读图单次最长 180s，姿态读图内部还会重试，README 要求 Go 侧 ≥200s。
	DefaultTimeout = 200 * time.Second
	// DefaultConcurrency 同时在途的 sidecar 请求上限。
	DefaultConcurrency = 3
	// DefaultMaxRetries 可重试错误的额外尝试次数。
	DefaultMaxRetries = 1
	// DefaultRetryBackoff 第 n 次重试前等待 n×RetryBackoff。
	DefaultRetryBackoff = 1500 * time.Millisecond

	// MaxImageBytes 单张 data URL 长度上限（与 sidecar _image_url 一致，按字符串长度算）。
	MaxImageBytes = 12 * 1024 * 1024
	// MaxBodyBytes 请求体上限（sidecar MAX_BODY，超出 413）。
	MaxBodyBytes = 24 * 1024 * 1024
	// CropPlanBatch crop-plan 每次最多处理的图数（sidecar images[:6]）。
	CropPlanBatch = 6
	// CropCheckBatch crop-check 每次最多处理的图数（sidecar items[:12]）。
	CropCheckBatch = 12
	// MaxBatchModels analyze-batch 模特上限（sidecar models[:9]）。
	MaxBatchModels = 9
)

// 路由。
const (
	PathStatus       = "/api/status"
	PathPoseSource   = "/api/pose/source"
	PathPosePrompt   = "/api/pose/prompt"
	PathAnalyze      = "/api/reverse/analyze"
	PathAnalyzeBatch = "/api/reverse/analyze-batch"
	PathCropPlan     = "/api/reverse/crop-plan"
	PathCropCheck    = "/api/reverse/crop-check"
)

// Config 客户端配置；零值字段取默认。
type Config struct {
	BaseURL      string
	Timeout      time.Duration
	Concurrency  int
	MaxRetries   int // <0 表示不重试；0 取默认
	RetryBackoff time.Duration
	HTTPClient   *http.Client
}

// Client sidecar 客户端，并发安全。
type Client struct {
	BaseURL      string
	Timeout      time.Duration
	Concurrency  int
	MaxRetries   int
	RetryBackoff time.Duration

	hc  *http.Client
	sem chan struct{}
}

// New 创建客户端。BaseURL 为空时 Enabled()=false，调用返回 KindNotConfigured。
func New(cfg Config) *Client {
	c := &Client{
		BaseURL:      strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		Timeout:      cfg.Timeout,
		Concurrency:  cfg.Concurrency,
		MaxRetries:   cfg.MaxRetries,
		RetryBackoff: cfg.RetryBackoff,
		hc:           cfg.HTTPClient,
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	switch {
	case c.MaxRetries == 0:
		c.MaxRetries = DefaultMaxRetries
	case c.MaxRetries < 0:
		c.MaxRetries = 0
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = DefaultRetryBackoff
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	c.sem = make(chan struct{}, c.Concurrency)
	return c
}

// Enabled 是否配置了 BaseURL。
func (c *Client) Enabled() bool { return c != nil && c.BaseURL != "" }

// DataURL 拼 data URL。mime 为空按 image/jpeg。
func DataURL(mime string, b []byte) string {
	mime = strings.TrimSpace(mime)
	if mime == "" {
		mime = "image/jpeg"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// checkImage 预检单张：必须是 data:image/ 且 ≤ MaxImageBytes。
func checkImage(path, what, data string) error {
	if !strings.HasPrefix(data, "data:image/") {
		return invalid(path, what+" 不是 data:image/ URL")
	}
	if len(data) > MaxImageBytes {
		return invalid(path, fmt.Sprintf("%s 超过 %dMB", what, MaxImageBytes>>20))
	}
	return nil
}

// ---------------------------------------------------------------- routes

// Status GET /api/status。
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.call(ctx, http.MethodGet, PathStatus, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MaxPoseGarments 一次姿态读图最多带几张服装参考图（sidecar POSE_MAX_GARMENTS）。
const MaxPoseGarments = 6

// PoseSource POST /api/pose/source：人物原图读图。422 = 无人物 / 疑似未成年（IsRejected）。
// garments 可选（≤6 张 data URL）：同时读服装参考图，source 里多出 garment_refs / garment_image_count，
// 之后 pose/prompt 的提示词按「图1 原图 / 图2 姿势 / 图3+ 服装」写。单人原图专用（garment_person 固定 0）。
func (c *Client) PoseSource(ctx context.Context, imageDataURL string, garments ...string) (*PoseSource, error) {
	if err := checkImage(PathPoseSource, "image", imageDataURL); err != nil {
		return nil, err
	}
	if len(garments) > MaxPoseGarments {
		return nil, invalid(PathPoseSource, fmt.Sprintf("garments 最多 %d 张", MaxPoseGarments))
	}
	for i, g := range garments {
		if err := checkImage(PathPoseSource, fmt.Sprintf("garments[%d]", i), g); err != nil {
			return nil, err
		}
	}
	var raw struct {
		Source json.RawMessage `json:"source"`
		Meta   PoseMeta        `json:"meta"`
	}
	if err := c.call(ctx, http.MethodPost, PathPoseSource, poseSourceBody(imageDataURL, garments), &raw); err != nil {
		return nil, err
	}
	out := &PoseSource{Meta: raw.Meta, Raw: raw.Source}
	if len(raw.Source) > 0 {
		if err := json.Unmarshal(raw.Source, &out.Source); err != nil {
			return nil, &Error{Path: PathPoseSource, Status: http.StatusOK, Kind: KindTransport, Message: "source 无法解析", Err: err}
		}
	}
	return out, nil
}

// poseSourceBody 不带服装参考图时与旧版逐字相同（只有 image）。
func poseSourceBody(image string, garments []string) any {
	if len(garments) == 0 {
		return map[string]string{"image": image}
	}
	return map[string]any{"image": image, "garments": garments, "garment_person": 0}
}

// PosePrompt POST /api/pose/prompt：姿势参考图 → 长版提示词。source 用 PoseSource 的返回（按 Raw 原样回传）。
// mapping 可选（nil = sidecar 默认配对），见 PosePromptReq.Mapping。
func (c *Client) PosePrompt(ctx context.Context, source *PoseSource, poseDataURL, notes string, mapping []int) (*PosePrompt, error) {
	return c.PosePromptMode(ctx, source, poseDataURL, notes, "", mapping)
}

// PosePromptMode 同 PosePrompt，mode=short 走短版。
func (c *Client) PosePromptMode(ctx context.Context, source *PoseSource, poseDataURL, notes, mode string, mapping []int) (*PosePrompt, error) {
	if source == nil {
		return nil, invalid(PathPosePrompt, "缺少 source")
	}
	if err := checkImage(PathPosePrompt, "image", poseDataURL); err != nil {
		return nil, err
	}
	if len(mapping) > MaxPosePeople {
		return nil, invalid(PathPosePrompt, fmt.Sprintf("mapping 最多 %d 位人物", MaxPosePeople))
	}
	src := source.Raw
	if len(src) == 0 {
		b, err := json.Marshal(source.Source)
		if err != nil {
			return nil, invalid(PathPosePrompt, "source 无法序列化")
		}
		src = b
	}
	var raw json.RawMessage
	req := PosePromptReq{Source: src, Image: poseDataURL, Notes: strings.TrimSpace(notes), Mode: mode, Mapping: mapping}
	if err := c.call(ctx, http.MethodPost, PathPosePrompt, req, &raw); err != nil {
		return nil, err
	}
	out := &PosePrompt{Raw: raw}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, &Error{Path: PathPosePrompt, Status: http.StatusOK, Kind: KindTransport, Message: "响应无法解析", Err: err}
	}
	return out, nil
}

// Analyze POST /api/reverse/analyze。读图失败时 sidecar 自己降级（Meta.Degraded=true），HTTP 仍是 200。
func (c *Client) Analyze(ctx context.Context, req AnalyzeReq) (*AnalyzeResp, error) {
	for i, im := range req.Images {
		if err := checkImage(PathAnalyze, fmt.Sprintf("images[%d]", i), im.Data); err != nil {
			return nil, err
		}
	}
	var out AnalyzeResp
	if err := c.call(ctx, http.MethodPost, PathAnalyze, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AnalyzeBatch POST /api/reverse/analyze-batch：同一组服装 × 多个模特。单个模特失败只在该项 Error 里体现。
// 模特 1–9 个；Concurrency 夹到 1–6（0 = sidecar 默认 3）。
func (c *Client) AnalyzeBatch(ctx context.Context, req AnalyzeBatchReq) (*AnalyzeBatchResp, error) {
	if len(req.Models) == 0 {
		return nil, invalid(PathAnalyzeBatch, "没有可用的模特图")
	}
	if len(req.Models) > MaxBatchModels {
		return nil, invalid(PathAnalyzeBatch, fmt.Sprintf("模特最多 %d 个", MaxBatchModels))
	}
	for i, im := range req.Images {
		if err := checkImage(PathAnalyzeBatch, fmt.Sprintf("images[%d]", i), im.Data); err != nil {
			return nil, err
		}
	}
	for i, m := range req.Models {
		if err := checkImage(PathAnalyzeBatch, fmt.Sprintf("models[%d]", i), m.Data); err != nil {
			return nil, err
		}
	}
	if req.Concurrency != 0 {
		req.Concurrency = max(1, min(6, req.Concurrency))
	}
	var out AnalyzeBatchResp
	if err := c.call(ctx, http.MethodPost, PathAnalyzeBatch, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// splitEven 把 n 均匀分成每份 ≤size 的若干段，返回各段 [start,end)。
func splitEven(n, size int) [][2]int {
	if n <= 0 {
		return nil
	}
	k := (n + size - 1) / size
	out := make([][2]int, 0, k)
	start := 0
	for i := 0; i < k; i++ {
		cnt := n / k
		if i < n%k {
			cnt++
		}
		out = append(out, [2]int{start, start + cnt})
		start += cnt
	}
	return out
}

// CropPlan POST /api/reverse/crop-plan，>6 张自动分批（各批并发，受信号量约束），合并 plans 并把 index 回映到 req.Images 下标。
// 任一批失败整体返回错误（切图是批次级缓存，部分结果没有意义）。
func (c *Client) CropPlan(ctx context.Context, req CropPlanReq) (*CropPlanResp, error) {
	if len(req.Images) == 0 {
		return nil, invalid(PathCropPlan, "没有可识别的服装图")
	}
	for i, im := range req.Images {
		if err := checkImage(PathCropPlan, fmt.Sprintf("images[%d]", i), im.Data); err != nil {
			return nil, err
		}
	}
	parts := splitEven(len(req.Images), CropPlanBatch)
	resps := make([]CropPlanResp, len(parts))
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for bi, p := range parts {
		wg.Add(1)
		go func(bi int, p [2]int) {
			defer wg.Done()
			sub := req
			sub.Images = req.Images[p[0]:p[1]]
			errs[bi] = c.call(ctx, http.MethodPost, PathCropPlan, sub, &resps[bi])
		}(bi, p)
	}
	wg.Wait()
	if err := firstErr(errs); err != nil {
		return nil, err
	}
	out := &CropPlanResp{Plans: []CropPlan{}}
	var modes []string
	for bi, p := range parts {
		r := resps[bi]
		n := p[1] - p[0]
		for _, plan := range r.Plans {
			if plan.Index < 0 || plan.Index >= n {
				continue // sidecar 已夹过下标，这里只防御
			}
			plan.Index += p[0]
			if plan.Crops == nil {
				plan.Crops = []Crop{}
			}
			out.Plans = append(out.Plans, plan)
		}
		m := r.Meta
		if m.Mode == "" {
			m.Mode = ModeCandidates
		}
		out.Batches = append(out.Batches, m)
		out.Meta.Images += m.Images
		out.Meta.Plans += len(r.Plans)
		out.Meta.ElapsedMs = max(out.Meta.ElapsedMs, m.ElapsedMs)
		if !contains(modes, m.Mode) {
			modes = append(modes, m.Mode)
		}
	}
	out.Meta.Mode = strings.Join(modes, ",")
	return out, nil
}

// CropCheck POST /api/reverse/crop-check，>12 张自动分批并把 index 回映到 items 下标。
// 某批失败时返回其余批的结果和错误（编排按「失败不阻断」处理：全部保留）。
func (c *Client) CropCheck(ctx context.Context, items []CropCheckItem) ([]CropCheckResult, error) {
	if len(items) == 0 {
		return nil, nil
	}
	for i, it := range items {
		if err := checkImage(PathCropCheck, fmt.Sprintf("items[%d]", i), it.Data); err != nil {
			return nil, err
		}
	}
	var parts [][2]int
	for s := 0; s < len(items); s += CropCheckBatch {
		parts = append(parts, [2]int{s, min(len(items), s+CropCheckBatch)})
	}
	resps := make([]cropCheckResp, len(parts))
	errs := make([]error, len(parts))
	var wg sync.WaitGroup
	for bi, p := range parts {
		wg.Add(1)
		go func(bi int, p [2]int) {
			defer wg.Done()
			errs[bi] = c.call(ctx, http.MethodPost, PathCropCheck,
				map[string]any{"items": items[p[0]:p[1]]}, &resps[bi])
		}(bi, p)
	}
	wg.Wait()
	var out []CropCheckResult
	for bi, p := range parts {
		if errs[bi] != nil {
			continue
		}
		n := p[1] - p[0]
		for _, r := range resps[bi].Results {
			if r.Index < 0 || r.Index >= n {
				continue
			}
			r.Index += p[0]
			out = append(out, r)
		}
	}
	return out, firstErr(errs)
}

func firstErr(errs []error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- transport

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	if !c.Enabled() {
		return &Error{Path: path, Kind: KindNotConfigured, Message: "未配置 sidecar BaseURL"}
	}
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return invalid(path, "请求无法序列化: "+err.Error())
		}
		if len(b) > MaxBodyBytes {
			return invalid(path, fmt.Sprintf("请求体超过 %dMB", MaxBodyBytes>>20))
		}
		body = b
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = c.once(ctx, method, path, body, out)
		if err == nil || !IsRetryable(err) || attempt >= c.MaxRetries || ctx.Err() != nil {
			return err
		}
		t := time.NewTimer(time.Duration(attempt+1) * c.RetryBackoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

func (c *Client) once(ctx context.Context, method, path string, body []byte, out any) error {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return transportError(path, ctx.Err())
	}
	defer func() { <-c.sem }()

	rctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(rctx, method, c.BaseURL+path, rd)
	if err != nil {
		return invalid(path, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return transportError(path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return transportError(path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var env struct {
			Error string `json:"error"`
		}
		msg := ""
		if json.Unmarshal(raw, &env) == nil && env.Error != "" {
			msg = env.Error
		} else {
			msg = truncate(string(raw), 300)
		}
		return classify(path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Path: path, Status: resp.StatusCode, Kind: KindTransport, Message: "响应无法解析", Err: err}
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
