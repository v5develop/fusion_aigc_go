package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 匿名报价（DESIGN 17.2）：Go 只读调用 Java 的出图报价，不复制任何计费规则，不编造价格。
const (
	quoteFreshTTL  = 5 * time.Minute // 匿名标价缓存
	quoteStaleTTL  = time.Hour       // Java 不可用时沿用最近一次成功值的上限
	etaTTL         = time.Hour
	etaMinSamples  = 30
	defaultQuoteRe = "1k"
	maxVariants    = 4
	// ReasonTooFrequent 限流（HTTP 429，体验文案 E-05）。
	ReasonTooFrequent = "TOO_FREQUENT"
	maxRefImages      = 50 // 与 Java image-quote 的 referenceCount 上限一致
)

// QuoteReq 对 Java 报价接口的请求（POST /api/v1/agent/image-quote）。
type QuoteReq struct {
	Resolution     string
	AspectRatio    string
	Num            int
	ReferenceCount int
	IncludeTurn    bool
	// Bearer 非空：用用户自己的令牌（登录用户个性化报价）；空：用报价探针用户。
	Bearer string
}

// JavaQuote Java 报价回包里我们用到的字段（容忍读：未知字段忽略）。
type JavaQuote struct {
	UnitPoints       int64  `json:"unitPoints"`
	ImagePoints      int64  `json:"imagePoints"`
	TurnPoints       *int64 `json:"turnPoints"`
	FreeQuotaApplied *bool  `json:"freeQuotaApplied"`
	TotalPoints      *int64 `json:"totalPoints"`
	Balance          *int64 `json:"balance"`
	Sufficient       *bool  `json:"sufficient"`
	// TurnPriceUnavailable Java 读不到轮次费（按 0 计）：前端提示「轮次费以实际为准」
	TurnPriceUnavailable *bool `json:"turnPriceUnavailable"`
}

// QuoteSource 报价源。
type QuoteSource interface {
	ImageQuote(ctx context.Context, req QuoteReq) (*JavaQuote, error)
}

// QuoteParams GET /plaza/quote 的参数。
type QuoteParams struct {
	Industry   string
	Sub        string
	TemplateID int64
	Variants   int
	// Resolution 出图档位 1k / 2k / 4k（大小写不敏感）；空 = 1k，非法 → 400 PARAM_INVALID
	Resolution string
	// RefImageCount 请求里实际填了几张参考图（image 位已填数量）；nil = 请求没带，退回 NeedsProductImage 口径。
	RefImageCount *int
	IPHash        string
	// Bearer 登录用户的 Authorization 原文（可空）：有则转发给 Java 取个性化报价。
	Bearer string
}

// Eta 预计耗时（秒）。
type Eta struct {
	P50     int `json:"p50"`
	P80     int `json:"p80"`
	Samples int `json:"samples"`
}

// QuoteResult 匿名标价 / 登录个性化报价。
type QuoteResult struct {
	TemplateID int64 `json:"templateId"`
	// MediaType image / video；MaxVariants 一次最多出几版（图 4，视频以 Java 为准，当前 1）。
	MediaType        string `json:"mediaType"`
	MaxVariants      int    `json:"maxVariants"`
	Variants         int    `json:"variants"`
	Resolution       string `json:"resolution"`
	ReferenceCount   int    `json:"referenceCount"`
	UnitPoints       int64  `json:"unitPoints"`
	ImagePoints      int64  `json:"imagePoints"` // 出图 / 出片合计（视频模板同样放这里，形状不变）
	TurnPoints       *int64 `json:"turnPoints"`
	TurnFeeIncluded  bool   `json:"turnFeeIncluded"`
	ListPrice        bool   `json:"listPrice"`
	Personalized     bool   `json:"personalized"`
	EtaSeconds       *Eta   `json:"etaSeconds"`
	Stale            bool   `json:"stale"`
	FreeQuotaApplied *bool  `json:"freeQuotaApplied,omitempty"`
	TotalPoints      *int64 `json:"totalPoints,omitempty"`
	Balance          *int64 `json:"balance,omitempty"`
	Sufficient       *bool  `json:"sufficient,omitempty"`
	// TurnPriceUnavailable 个性化报价里 Java 没读到轮次费（turnPoints 按 0 计），总价以实际为准
	TurnPriceUnavailable *bool `json:"turnPriceUnavailable,omitempty"`
}

type quoteCacheEntry struct {
	unit int64
	at   time.Time
}

type etaCacheEntry struct {
	eta *Eta
	at  time.Time
	set bool
}

type limiters struct {
	perIP  *userquota.Limiter
	global *userquota.Limiter
}

func (s *Service) initLimiters() {
	s.anonLim = limiters{
		perIP:  userquota.New("plaza:quote_ip", s.cfg.Quote.AnonPerMinute, s.cfg.Quote.AnonPerDay, s.rd),
		global: userquota.New("plaza:quote_all", s.cfg.Quote.GlobalPerMinute, 0, s.rd),
	}
}

func ipKey(ipHash string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(ipHash))
	return int64(h.Sum64() >> 1)
}

// Quote 公开匿名报价。industry 必填（缺 → 400）；templateId 必须属于该行业且此刻可见。
// 有 Bearer 时转发给 Java 取个性化报价（失败退回匿名标价）；匿名标价不含轮次费，登录后才显示实际价。
func (s *Service) Quote(ctx context.Context, p QuoteParams) (*QuoteResult, error) {
	if strings.TrimSpace(p.Industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if p.TemplateID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少模板")
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, p.Industry, p.Sub)
	if err != nil {
		return nil, err
	}
	now := s.now()
	rec, ok := c.InScope(sc, p.TemplateID, now)
	if !ok {
		return nil, ErrTemplateGone()
	}
	res0, rerr := normalizeResolution(p.Resolution)
	if rerr != nil {
		return nil, rerr
	}
	variants := p.Variants
	if variants < 1 {
		variants = 1
	}
	if variants > maxVariants {
		variants = maxVariants
	}
	if s.anonLim.global.Take(ctx, 0) != userquota.Allowed || s.anonLim.perIP.Take(ctx, ipKey(p.IPHash)) != userquota.Allowed {
		return nil, xerr.NewReason(xerr.Conflict, ReasonTooFrequent, "操作太快了，稍后再试")
	}
	if rec.t.MediaType == model.MediaVideo {
		// 视频不经智能体对话出片（走「生成视频」页），这里没有对应的报价源：不编造价格
		return nil, xerr.NewReason(xerr.ThirdPartyError, ReasonQuoteUnavailable, "视频模板在生成视频页报价")
	}
	refCount := 0
	if p.RefImageCount != nil {
		refCount = *p.RefImageCount
		if refCount < 0 {
			refCount = 0
		}
		if refCount > maxRefImages {
			refCount = maxRefImages
		}
	} else if rec.t.NeedsProductImage == 1 {
		refCount = 1
	}
	resolution := res0
	res := &QuoteResult{TemplateID: p.TemplateID, MediaType: model.MediaImage, MaxVariants: maxVariants,
		Variants: variants, Resolution: resolution, ReferenceCount: refCount}
	res.EtaSeconds = s.eta(ctx)

	if p.Bearer != "" && s.quoter != nil {
		jq, err := s.quoter.ImageQuote(ctx, QuoteReq{Resolution: resolution, Num: variants, ReferenceCount: refCount, IncludeTurn: true, Bearer: p.Bearer})
		if err == nil && jq != nil {
			res.UnitPoints, res.ImagePoints = jq.UnitPoints, jq.ImagePoints
			res.TurnPoints, res.TurnFeeIncluded, res.Personalized = jq.TurnPoints, true, true
			res.FreeQuotaApplied, res.TotalPoints, res.Balance, res.Sufficient = jq.FreeQuotaApplied, jq.TotalPoints, jq.Balance, jq.Sufficient
			res.TurnPriceUnavailable = jq.TurnPriceUnavailable
			return res, nil
		}
		logx.WithContext(ctx).Infof("plaza: 个性化报价失败，退回匿名标价: %v", err)
	}

	// Java 只按「有没有参考图」分文生图 / 图生图两个单价：匿名缓存按 0/1 归一，避免按张数把缓存键放大
	priceRef := 0
	if refCount > 0 {
		priceRef = 1
	}
	key := resolution + ":" + strconv.Itoa(priceRef)
	unit, stale, err := s.listUnitPrice(ctx, key, func(ctx context.Context) (int64, error) {
		if s.quoter == nil {
			return 0, fmt.Errorf("未配置报价源")
		}
		jq, qerr := s.quoter.ImageQuote(ctx, QuoteReq{Resolution: resolution, Num: 1, ReferenceCount: priceRef, IncludeTurn: false})
		if qerr != nil || jq == nil {
			return 0, fmt.Errorf("java image-quote 不可用: %v", qerr)
		}
		return jq.UnitPoints, nil
	})
	if err != nil {
		return nil, err
	}
	res.UnitPoints, res.ImagePoints = unit, unit*int64(variants)
	res.ListPrice, res.Stale = true, stale
	return res, nil
}

// listUnitPrice 匿名标价单价：按 key 缓存 5 分钟；取价失败时沿用最近一次成功值（≤1 小时，stale=true）；
// 再久返回 QUOTE_UNAVAILABLE，由前端显示「登录后查看报价」，不编造价格。
func (s *Service) listUnitPrice(ctx context.Context, key string, fetch func(context.Context) (int64, error)) (unit int64, stale bool, err error) {
	now := s.now()
	s.quoteMu.Lock()
	e, have := s.quoteC[key]
	s.quoteMu.Unlock()
	if !have && s.rd != nil {
		if v, rerr := s.rd.Get(ctx, "aigc:plaza:quote:"+key).Result(); rerr == nil {
			var cu struct {
				U int64 `json:"u"`
				T int64 `json:"t"`
			}
			if json.Unmarshal([]byte(v), &cu) == nil {
				e, have = quoteCacheEntry{unit: cu.U, at: time.UnixMilli(cu.T)}, true
			}
		}
	}
	if have && now.Sub(e.at) < quoteFreshTTL {
		return e.unit, false, nil
	}
	u, qerr := fetch(ctx)
	if qerr == nil && u > 0 {
		ne := quoteCacheEntry{unit: u, at: now}
		s.quoteMu.Lock()
		s.quoteC[key] = ne
		s.quoteMu.Unlock()
		if s.rd != nil {
			b, _ := json.Marshal(map[string]int64{"u": ne.unit, "t": ne.at.UnixMilli()})
			_ = s.rd.Set(ctx, "aigc:plaza:quote:"+key, b, quoteStaleTTL).Err()
		}
		return ne.unit, false, nil
	}
	logx.WithContext(ctx).Errorf("plaza: 匿名报价取价失败 判据=Java 报价不可用或单价非正 key=%s 缓存=%v: %v", key, have, qerr)
	if have && now.Sub(e.at) < quoteStaleTTL {
		return e.unit, true, nil
	}
	return 0, false, xerr.NewReason(xerr.ThirdPartyError, ReasonQuoteUnavailable, "登录后查看报价")
}

// eta 近 7 天同类出图任务的 p50 / p80（每小时算一次）；样本 < 30 返回 nil（不显示时间，不编造）。
func (s *Service) eta(ctx context.Context) *Eta {
	codes, cache := s.cfg.Quote.EtaEntryCodes, &s.etaC
	if s.rep == nil || len(codes) == 0 {
		return nil
	}
	s.quoteMu.Lock()
	if cache.set && s.now().Sub(cache.at) < etaTTL {
		e := cache.eta
		s.quoteMu.Unlock()
		return e
	}
	s.quoteMu.Unlock()
	ms, err := s.rep.LatencySamples(ctx, codes)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 预计耗时查询失败（不显示时间）: %v", err)
		return nil
	}
	e := etaFromSamples(ms)
	s.quoteMu.Lock()
	*cache = etaCacheEntry{eta: e, at: s.now(), set: true}
	s.quoteMu.Unlock()
	return e
}

// etaFromSamples 取 p50 / p80（升序样本，最近秩法），毫秒转秒向上取整；样本不足返回 nil。
func etaFromSamples(ms []int64) *Eta {
	if len(ms) < etaMinSamples {
		return nil
	}
	sorted := append([]int64(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) int {
		idx := int(float64(len(sorted))*p+0.9999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return int((sorted[idx] + 999) / 1000)
	}
	return &Eta{P50: pick(0.5), P80: pick(0.8), Samples: len(sorted)}
}

// ---------------------------------------------------------------- Java 报价客户端

// JavaQuoteClient 调 Java 内网 /api/v1/agent/image-quote。无 Bearer 时以「报价探针」系统用户的会话令牌调用。
type JavaQuoteClient struct {
	BaseURL     string
	ProbeUserID int64
	Session     *session.Store
	HTTP        *http.Client

	mu       sync.Mutex
	probeTok string
	probeExp time.Time
}

// NewJavaQuoteClient baseURL 为空返回 nil（报价不可用）。
func NewJavaQuoteClient(baseURL string, probeUserID int64, sess *session.Store) *JavaQuoteClient {
	if strings.TrimSpace(baseURL) == "" {
		return nil
	}
	return &JavaQuoteClient{BaseURL: strings.TrimRight(baseURL, "/"), ProbeUserID: probeUserID, Session: sess,
		HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (j *JavaQuoteClient) probeToken(ctx context.Context) (string, error) {
	if j.ProbeUserID <= 0 || !j.Session.Enabled() {
		return "", fmt.Errorf("报价探针用户或会话 Redis 未配置")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.probeTok != "" && time.Now().Before(j.probeExp) {
		return j.probeTok, nil
	}
	tk, err := j.Session.Issue(ctx, session.Claims{UserID: j.ProbeUserID, Username: "plaza-quote-probe"})
	if err != nil {
		return "", err
	}
	// 留 10% 余量提前换新
	j.probeTok = tk.AccessToken
	j.probeExp = time.Now().Add(time.Duration(float64(tk.ExpiresIn)*0.9) * time.Second)
	return j.probeTok, nil
}

func (j *JavaQuoteClient) auth(ctx context.Context, bearer string) (string, error) {
	if bearer == "" {
		tok, err := j.probeToken(ctx)
		if err != nil {
			return "", err
		}
		return "Bearer " + tok, nil
	}
	if !strings.HasPrefix(strings.ToLower(bearer), "bearer ") {
		return "Bearer " + bearer, nil
	}
	return bearer, nil
}

// post 调 Java 只读报价接口并把 CommonResult.data 解到 out。
func (j *JavaQuoteClient) post(ctx context.Context, path, bearer string, body map[string]any, out any) error {
	auth, err := j.auth(ctx, bearer)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("java %s http %d", path, resp.StatusCode)
	}
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("java %s code %d", path, env.Code)
	}
	return json.Unmarshal(env.Data, out)
}

func (j *JavaQuoteClient) ImageQuote(ctx context.Context, r QuoteReq) (*JavaQuote, error) {
	var q JavaQuote
	err := j.post(ctx, "/api/v1/agent/image-quote", r.Bearer, map[string]any{
		"resolution": r.Resolution, "aspectRatio": r.AspectRatio, "num": r.Num,
		"referenceCount": r.ReferenceCount, "includeTurn": r.IncludeTurn,
	}, &q)
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// normalizeResolution 档位归一：空 → 1k；1k / 2k / 4k（不分大小写）；其他 → 400。
func normalizeResolution(raw string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(raw))
	switch r {
	case "":
		return defaultQuoteRe, nil
	case "1k", "2k", "4k":
		return r, nil
	}
	return "", xerr.Param("resolution 只能是 1k / 2k / 4k")
}
