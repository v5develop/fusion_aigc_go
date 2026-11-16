package core

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	catalogTTL      = 10 * time.Minute // 目录快照兜底过期
	catalogVerCheck = 2 * time.Second  // 读请求路径上最多每 2 秒比对一次版本号
	catalogVerKey   = "aigc:plaza:catalog:ver"
	usageTTL        = 10 * time.Minute // 复刻次数 10 分钟快照（同 Java）
	defaultPageSize = 24
	maxPageSize     = 100
)

// Service 广场模板域服务。用户端读取走内存目录快照，后台写入后递增 Redis 版本号让各副本重载。
type Service struct {
	cfg Config
	db  *bun.DB
	rd  redis.UniversalClient
	src Source
	rep *Repo
	now func() time.Time
	// industry 行业功能总开关（数据库 > 配置文件）；无库（单测）时为 nil，退回配置文件
	industry *IndustrySwitch

	mu         sync.Mutex
	cat        *Catalog
	catLoaded  time.Time
	catChecked time.Time
	catVer     string

	umu      sync.Mutex
	usage    map[int64]int64
	usageAt  time.Time
	usageSet bool

	quoter          QuoteSource
	ovMedia         OverlayMedia
	ovAssets        OverlayAssets
	ovOwner         OverlayOwnership
	ovLim           *userquota.Limiter
	ovIntentLim     *userquota.Limiter
	uploader        *Uploader
	headlineCleaner HeadlineCleaner
	headlineBrief   HeadlineBriefFunc
	headlineStatus  HeadlineStatusFunc
	quoteMu         sync.Mutex
	quoteC          map[string]quoteCacheEntry
	etaC            etaCacheEntry
	anonLim         limiters
}

// New 生产构造：db 为 bun 句柄，rd 可为 nil（单实例 / 开发：版本号、配额计数退回进程内）。
func New(db *bun.DB, rd redis.UniversalClient, cfg Config) *Service {
	rep := NewRepo(db)
	rep.requireEnabled = !cfg.AdminWriteEnabled
	s := &Service{cfg: cfg, db: db, rd: rd, src: rep, rep: rep, now: time.Now, quoteC: map[string]quoteCacheEntry{}}
	if db != nil {
		s.industry = newIndustrySwitch(db.DB, cfg.Industry.Enabled)
	}
	s.initLimiters()
	return s
}

// NewWithSource 单测 / 只读场景：自带数据源与时钟，不连库。
func NewWithSource(src Source, cfg Config, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	s := &Service{cfg: cfg, src: src, now: now, quoteC: map[string]quoteCacheEntry{}}
	s.initLimiters()
	return s
}

// SetQuoteSource 注入报价源（生产是 JavaQuoteClient，单测是假实现）。
func (s *Service) SetQuoteSource(q QuoteSource) { s.quoter = q }

// Config 当前配置（只读副本）。
func (s *Service) Config() Config { return s.cfg }

// catalog 取目录快照：版本号变了（后台写过）或超过 TTL 就重载。
func (s *Service) catalog(ctx context.Context) (*Catalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cat != nil && now.Sub(s.catLoaded) < catalogTTL {
		if s.rd == nil || now.Sub(s.catChecked) < catalogVerCheck {
			return s.cat, nil
		}
		s.catChecked = now
		if v, err := s.rd.Get(ctx, catalogVerKey).Result(); err == nil && v != s.catVer {
			// 版本变了：往下重载
		} else {
			return s.cat, nil // 没变，或 Redis 抖动：沿用
		}
	}
	return s.reloadLocked(ctx)
}

func (s *Service) reloadLocked(ctx context.Context) (*Catalog, error) {
	if s.src == nil {
		return nil, ErrNotReady()
	}
	ver := ""
	if s.rd != nil {
		ver, _ = s.rd.Get(ctx, catalogVerKey).Result()
	}
	c, err := BuildCatalog(ctx, s.src, s.now(), !s.cfg.AdminWriteEnabled)
	if err != nil {
		if s.cat != nil { // 重载失败沿用旧快照，不让用户端整体打不开
			logx.WithContext(ctx).Errorf("plaza: 目录快照重载失败，沿用旧快照: %v", err)
			s.catLoaded = s.now().Add(-catalogTTL + 30*time.Second)
			return s.cat, nil
		}
		return nil, err
	}
	s.cat, s.catLoaded, s.catChecked, s.catVer = c, s.now(), s.now(), ver
	return c, nil
}

// Invalidate 后台写入后调用：本副本立即重载，并递增版本号让其它副本在 2 秒内跟上。
func (s *Service) Invalidate(ctx context.Context) {
	if s.rd != nil {
		if err := s.rd.Incr(ctx, catalogVerKey).Err(); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 递增目录版本号失败（其它副本最迟 10 分钟后跟上）: %v", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.reloadLocked(ctx); err != nil {
		logx.WithContext(ctx).Errorf("plaza: 目录快照重载失败: %v", err)
	}
}

// usageMap 近 7 天复刻次数快照（10 分钟）；查询失败沿用旧快照，没有就当全 0。
func (s *Service) usageMap(ctx context.Context) map[int64]int64 {
	s.umu.Lock()
	defer s.umu.Unlock()
	now := s.now()
	if s.usageSet && now.Sub(s.usageAt) < usageTTL {
		return s.usage
	}
	if s.rep == nil {
		return s.usage
	}
	m, err := s.rep.Uses7d(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 复刻次数统计刷新失败 判据=afv_agent_turn 聚合异常 沿用旧快照: %v", err)
		s.usageAt, s.usageSet = now, true // 退避一个 TTL，不让每个请求都去重试一条出错的查询
		return s.usage
	}
	s.usage, s.usageAt, s.usageSet = m, now, true
	return m
}

// ---------------------------------------------------------------- 用户端：目录 / 列表 / 计数 / 详情

// Directory 行业目录。开关关闭或没有任何可展示的行业 → gateEnabled=false 且 industries=[]。
type Directory struct {
	GateEnabled bool            `json:"gateEnabled"`
	Industries  []DirectoryItem `json:"industries"`
}

func (s *Service) Industries(ctx context.Context) (*Directory, error) {
	if !s.IndustryEnabled(ctx) {
		return &Directory{Industries: []DirectoryItem{}}, nil
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	items := c.Directory(s.now())
	return &Directory{GateEnabled: len(items) > 0, Industries: items}, nil
}

// CountsResult /plaza/counts：只有所选行业这一个范围。
type CountsResult struct {
	Scope      string `json:"scope"`
	Image      int    `json:"image"`
	Video      int    `json:"video"`
	NewCount7d int    `json:"newCount7d"`
}

func (s *Service) Counts(ctx context.Context, industry, sub string) (*CountsResult, error) {
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, industry, sub)
	if err != nil {
		return nil, err
	}
	n := c.Counts(sc, s.now())
	return &CountsResult{Scope: sc.Code(), Image: n.Image, Video: n.Video, NewCount7d: n.NewCount7d}, nil
}

// ListParams 列表参数（原始，未校验）。
type ListParams struct {
	Industry  string
	Sub       string
	MediaType string
	Keyword   string
	Page      int
	Size      int
	// MissCtx 搜索缺口去重用的 IP 哈希（空则不记缺口）。
	IPHash string
}

// ParseListParams 从 query 取参：page/size 缺省 1/24，非数字或 <1 → PARAM_INVALID，size>100 夹到 100。
func ParseListParams(q url.Values) (ListParams, error) {
	p := ListParams{
		Industry: q.Get("industry"), Sub: q.Get("subIndustry"),
		MediaType: strings.TrimSpace(q.Get("mediaType")), Keyword: q.Get("keyword"),
		Page: 1, Size: defaultPageSize,
	}
	var err error
	if p.Page, err = intParam(q, "page", 1); err != nil {
		return p, err
	}
	if p.Size, err = intParam(q, "size", defaultPageSize); err != nil {
		return p, err
	}
	return p, nil
}

func intParam(q url.Values, key string, def int) (int, error) {
	raw := strings.TrimSpace(q.Get(key))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, errParam(key + " 必须是 ≥1 的整数")
	}
	return n, nil
}

// Page 分页信封（沿用 v1 形状 current / pages / total / data）。
type Page struct {
	Current int            `json:"current"`
	Pages   int            `json:"pages"`
	Total   int            `json:"total"`
	Data    []TemplateItem `json:"data"`
}

// resolve 校验 industry（缺 → 400；失效 → INDUSTRY_NOT_FOUND）。
func (s *Service) resolve(c *Catalog, industry, sub string) (*Scope, error) {
	if strings.TrimSpace(industry) == "" {
		return nil, ErrIndustryRequired()
	}
	return c.Resolve(industry, sub)
}

// Templates 列表与搜索（同一个接口）：industry 必填；keyword 只在所选行业内搜；没有跨行业视角。
// 首页（page=1）有关键词却 0 条结果时，记一次行业内搜索缺口（只记清洗后的词，不含任何用户标识）。
func (s *Service) Templates(ctx context.Context, p ListParams) (*Page, error) {
	if strings.TrimSpace(p.Industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if p.MediaType != "" && p.MediaType != "image" && p.MediaType != "video" {
		return nil, errParam("mediaType 只能是 image 或 video")
	}
	if p.Page < 1 || p.Size < 1 {
		return nil, errParam("page 与 size 必须 ≥1")
	}
	if p.Size > maxPageSize {
		p.Size = maxPageSize
	}
	kw, err := validKeyword(p.Keyword)
	if err != nil {
		return nil, err
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
	res := c.List(sc, ListQuery{MediaType: p.MediaType, Keyword: kw, Page: p.Page, Size: p.Size}, now)
	usage := s.usageMap(ctx)
	out := &Page{Current: p.Page, Total: res.Total, Pages: (res.Total + p.Size - 1) / p.Size, Data: make([]TemplateItem, 0, len(res.Recs))}
	for _, rec := range res.Recs {
		out.Data = append(out.Data, s.item(c, rec, usage, now))
	}
	if kw != "" && p.Page == 1 && res.Total == 0 {
		s.recordSearchMiss(ctx, sc.Code(), kw, p.IPHash)
	}
	return out, nil
}

// TemplateByID 按 id 取单条（深链还原已选模板）：不带行业，不是浏览入口；不可见 → RESOURCE_NOT_FOUND。
func (s *Service) TemplateByID(ctx context.Context, id int64) (*TemplateItem, error) {
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	rec, err := c.Get(id, now)
	if err != nil {
		return nil, err
	}
	it := s.item(c, rec, s.usageMap(ctx), now)
	if schema, err := ParseSlotSchema(rec.t.SlotsJSON); err == nil { // 库里的脏数据不让详情打不开：解析失败就当没有入参定义
		it.Slots = schema
	} else {
		logx.WithContext(ctx).Errorf("plaza: 模板 %d 的 slots_json 不合法，详情不下发入参定义: %v", id, err)
	}
	return &it, nil
}

// PrepareInputs 「做同款准备」：按模板的入参 schema 校验并规整用户填的入参（不信前端）。
// 模板按 id 取（深链口径，不要求行业）；不可见 → RESOURCE_NOT_FOUND。
func (s *Service) PrepareInputs(ctx context.Context, id int64, inputs map[string]json.RawMessage) (map[string]any, error) {
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := c.Get(id, s.now())
	if err != nil {
		return nil, err
	}
	schema, perr := ParseSlotSchema(rec.t.SlotsJSON)
	if perr != nil {
		return nil, xerr.New(xerr.ServerError, "模板入参定义有误，请联系运营")
	}
	return ValidateInputs(schema, inputs)
}

// IsBizErr 是否业务错误（handler 用来区分 500）。
func IsBizErr(err error) bool { _, _, ok := xerr.Parse(err); return ok }
