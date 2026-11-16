package httpapi

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/rules/analysisfix"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 运行参数（afv_system_config，DESIGN §14.2）。读法同 studio 的运行时开关：单表、deleted=0、60 秒缓存，
// 查库失败沿用上一次的值（首次失败用 yaml / 内核默认），值非法的键忽略并告警（带键名与原值）。
const (
	KeyEnabled           = "replicate.enabled"                 // bool，总开关：库里没有（或值非法）时用 Replicate.Enabled（数据库 > 文件配置）；关着时写接口回 7003
	KeyLongformEnabled   = "replicate.longform_enabled"        // bool，多段视频（装配区间切段）
	KeyAutoRetryMax      = "replicate.auto_retry_max"          // int 0~3
	KeyRedoMaxPerSegment = "replicate.redo_max_per_segment"    // int 1~10
	KeyRedoMaxPerJob     = "replicate.redo_max_per_job"        // int 1~50
	KeyContinueHours     = "replicate.continue_deadline_hours" // int 1~168
	KeyPlanTTLHours      = "replicate.plan_ttl_hours"          // int 1~720
	KeyMaxCostCNY        = "replicate.max_cost_cny"            // 十进制字符串（网关 limits.max_cost）
	KeyMaxInFlight       = "replicate.max_in_flight"           // int 1~6：每 job 同时在途段数
	KeyMaxSegments       = "replicate.max_segments"            // int 1~6（决定 limits.maxDurationSec）
	KeyOverlapMs         = "replicate.overlap_ms"              // int 0~2000
	KeyVisionModel       = "replicate_vision"                  // 模型组码（画面理解）
	KeyWriterModel       = "replicate_writer"                  // 模型组码（写手）
	KeyWriterEnabled     = "replicate.writer.enabled"          // bool，写手开关
	KeyModels            = "replicate.models"                  // 逗号分隔的开放模型组（空 = 四家全部）
	KeyDefaultModel      = "replicate.default_model"           // 推荐模型组
	KeyFontRid           = "replicate.font_rid"                // 成片叠字 / 失败段占位的中文字体 rid（空 = 英文占位、不叠字）
	KeyPlaceholderText   = "replicate.placeholder_text"        // 失败段占位文字（默认「这一段还在重做」）
	// KeyConfirmThreshold 发送前二次确认阈值（点数，int 0~100000000，默认 0 = 不启用）：方案总价超过它时确认必须带 expectedPoints 且与现算一致
	KeyConfirmThreshold = "replicate.confirm_threshold_points"
	// KeyAnalysisRules 分析纠偏规则配置（JSON，rules/analysisfix.Config：{"disabled":[规则码],"durationTolerance":0.15,…}；空 = 默认）
	KeyAnalysisRules = "replicate.analysis_rules"

	// 内容审核（P0-4，策略同 Java app.moderation.*）
	KeyModerationEnabled     = "replicate.moderation.enabled"           // bool，默认 true
	KeyModerationFailOpen    = "replicate.moderation.fail_open"         // bool，默认 false（审核服务故障不让确认）
	KeyModerationReviewBlock = "replicate.moderation.review_as_block"   // bool，默认 true（嫌疑按违规）
	KeyModerationTextGroup   = "replicate.moderation.text_model_group"  // 默认 moderation.text.default
	KeyModerationImageGroup  = "replicate.moderation.image_model_group" // 默认 moderation.image.default
	KeyModerationRejectTTL   = "replicate.moderation.reject_ttl_hours"  // int 1~720，默认 24：不通过的结论多久后允许重审
	KeyModerationConfirmWait = "replicate.moderation.confirm_wait_ms"   // int 500~10000，默认 3000：确认时等后台预审结论的上限
	KeyModerationPlanWait    = "replicate.moderation.plan_wait_ms"      // int 0~5000，默认 0：生成方案时等预审结论（0 = 不等，立即返回）

	// 限频（P0-4）：每用户 分析 / 方案 / 确认 的每分钟与每日上限（0 = 不限），全局在途 job 上限（0 = 不限）
	KeyRateAnalysisPerMinute = "replicate.rate.analysis_per_minute" // 默认 5
	KeyRateAnalysisPerDay    = "replicate.rate.analysis_per_day"    // 默认 20
	KeyRatePlanPerMinute     = "replicate.rate.plan_per_minute"     // 默认 10
	KeyRatePlanPerDay        = "replicate.rate.plan_per_day"        // 默认 30
	KeyRateConfirmPerMinute  = "replicate.rate.confirm_per_minute"  // 默认 5
	KeyRateConfirmPerDay     = "replicate.rate.confirm_per_day"     // 默认 20
	KeyRateMaxInFlightJobs   = "replicate.rate.max_inflight_jobs"   // 默认 200
	// 改标注 / 再做一条（Go_V1.0.41）：独立计数，不占「方案」额度
	KeyRateAnnotatePerMinute = "replicate.rate.annotate_per_minute" // 默认 20
	KeyRateAnnotatePerDay    = "replicate.rate.annotate_per_day"    // 默认 300
)

// RateParams 限频参数。
type RateParams struct {
	AnalysisPerMinute, AnalysisPerDay int
	PlanPerMinute, PlanPerDay         int
	ConfirmPerMinute, ConfirmPerDay   int
	MaxInFlightJobs                   int
	// AnnotatePerMinute / AnnotatePerDay 改标注与「再做一条」（独立于「方案」）
	AnnotatePerMinute, AnnotatePerDay int
}

// DefaultRate 默认限频（目标假设：EXPERIENCE「今天还能分析 17 条、生成 28 次方案」量级）。
func DefaultRate() RateParams {
	return RateParams{AnalysisPerMinute: 5, AnalysisPerDay: 20, PlanPerMinute: 10, PlanPerDay: 30,
		ConfirmPerMinute: 5, ConfirmPerDay: 20, MaxInFlightJobs: 200, AnnotatePerMinute: 20, AnnotatePerDay: 300}
}

const runtimeTTL = 60 * time.Second

// Params 一次读到的运行参数（只记库里有且合法的项）。
type Params struct {
	Enabled  bool
	Longform *bool
	Cfg      core.Config
	Rate     RateParams
}

// loader 读 afv_system_config 里 replicate 相关的全部行：返回 键 → 值。
type loader func(ctx context.Context) (map[string]string, error)

func systemConfigLoader(conn sqlx.SqlConn) loader {
	return func(ctx context.Context) (map[string]string, error) {
		var rows []struct {
			Key   string `db:"config_key"`
			Value string `db:"config_value"`
		}
		q := "SELECT config_key, COALESCE(config_value, '') AS config_value FROM afv_system_config " +
			"WHERE deleted = 0 AND (config_key LIKE 'replicate.%' OR config_key IN (?, ?))"
		if err := conn.QueryRowsCtx(ctx, &rows, q, KeyVisionModel, KeyWriterModel); err != nil {
			return nil, err
		}
		out := make(map[string]string, len(rows))
		for _, r := range rows {
			out[r.Key] = strings.TrimSpace(r.Value)
		}
		return out, nil
	}
}

// applyParams 把库里的键覆盖到 base 上（base 为 yaml 装配的内核配置）。非法值忽略并告警，返回覆盖后的参数。
func applyParams(base core.Config, longformYAML bool, kv map[string]string) Params {
	p := Params{Cfg: base, Rate: DefaultRate()}
	warn := func(key, raw, why string) {
		logx.Errorf("replicate: 运行参数拒收 判据=%s 键=%s 原值=%q，用配置默认", why, key, raw)
	}
	boolOf := func(key string) (bool, bool) {
		raw, ok := kv[key]
		if !ok {
			return false, false
		}
		switch strings.ToLower(raw) {
		case "true", "1", "on", "yes":
			return true, true
		case "false", "0", "off", "no", "":
			return false, true
		}
		warn(key, raw, "不是布尔值")
		return false, false
	}
	intOf := func(key string, lo, hi int) (int, bool) {
		raw, ok := kv[key]
		if !ok {
			return 0, false
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < lo || v > hi {
			warn(key, raw, "不是 "+strconv.Itoa(lo)+"~"+strconv.Itoa(hi)+" 的整数")
			return 0, false
		}
		return v, true
	}
	if v, ok := boolOf(KeyEnabled); ok {
		p.Enabled = v
	}
	lf := longformYAML
	if v, ok := boolOf(KeyLongformEnabled); ok {
		lf = v
	}
	p.Longform = &lf
	if v, ok := intOf(KeyAutoRetryMax, 0, 3); ok {
		p.Cfg.AutoRetryMax = v
	}
	if v, ok := intOf(KeyRedoMaxPerSegment, 1, 10); ok {
		p.Cfg.RedoMaxPerSegment = v
	}
	if v, ok := intOf(KeyRedoMaxPerJob, 1, 50); ok {
		p.Cfg.RedoMaxPerJob = v
	}
	if v, ok := intOf(KeyContinueHours, 1, 168); ok {
		p.Cfg.ContinueWindow = time.Duration(v) * time.Hour
	}
	if v, ok := intOf(KeyPlanTTLHours, 1, 720); ok {
		p.Cfg.PlanTTL = time.Duration(v) * time.Hour
	}
	if v, ok := intOf(KeyMaxInFlight, 1, 6); ok {
		p.Cfg.MaxInFlight = v
	}
	if v, ok := intOf(KeyMaxSegments, 1, 6); ok {
		p.Cfg.Limits.MaxSegments = v
		p.Cfg.Limits.MaxDurationSec = 0 // 由 Normalize 按段数重算
	}
	if v, ok := intOf(KeyOverlapMs, 0, 2000); ok {
		p.Cfg.Limits.OverlapSec = float64(v) / 1000
		p.Cfg.Limits.MaxDurationSec = 0
	}
	if raw, ok := kv[KeyMaxCostCNY]; ok {
		if f, err := strconv.ParseFloat(raw, 64); err != nil || f <= 0 {
			warn(KeyMaxCostCNY, raw, "不是正的十进制金额")
		} else {
			p.Cfg.MaxCostCNY = raw
		}
	}
	if raw, ok := kv[KeyVisionModel]; ok && raw != "" {
		p.Cfg.VisionModelGroup = raw
	}
	if raw, ok := kv[KeyWriterModel]; ok && raw != "" {
		p.Cfg.WriterModelGroup = raw
	}
	if v, ok := boolOf(KeyWriterEnabled); ok {
		switch {
		case !v:
			p.Cfg.WriterModelGroup = ""
		case p.Cfg.WriterModelGroup == "":
			p.Cfg.WriterModelGroup = p.Cfg.VisionModelGroup
		}
	}
	if raw, ok := kv[KeyModels]; ok {
		var ms []string
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimSpace(g); g != "" {
				ms = append(ms, g)
			}
		}
		p.Cfg.Models = ms
	}
	if raw, ok := kv[KeyDefaultModel]; ok && raw != "" {
		p.Cfg.DefaultModel = raw
	}
	if raw, ok := kv[KeyFontRid]; ok {
		p.Cfg.FontRid = raw
	}
	if v, ok := boolOf(KeyModerationEnabled); ok {
		p.Cfg.ModerationOff = !v
	}
	if v, ok := boolOf(KeyModerationFailOpen); ok {
		p.Cfg.ModerationFailOpen = v
	}
	if v, ok := boolOf(KeyModerationReviewBlock); ok {
		p.Cfg.ModerationReviewPass = !v
	}
	if raw, ok := kv[KeyModerationTextGroup]; ok && raw != "" {
		p.Cfg.ModerationTextGroup = raw
	}
	if raw, ok := kv[KeyModerationImageGroup]; ok && raw != "" {
		p.Cfg.ModerationImageGroup = raw
	}
	if v, ok := intOf(KeyModerationRejectTTL, 1, 720); ok {
		p.Cfg.ModerationRejectTTL = time.Duration(v) * time.Hour
	}
	if v, ok := intOf(KeyModerationConfirmWait, 500, 10000); ok {
		p.Cfg.ModerationConfirmWait = time.Duration(v) * time.Millisecond
	}
	if v, ok := intOf(KeyModerationPlanWait, 0, 5000); ok {
		p.Cfg.ModerationPlanWait = time.Duration(v) * time.Millisecond
	}
	for _, x := range []struct {
		key string
		dst *int
	}{
		{KeyRateAnalysisPerMinute, &p.Rate.AnalysisPerMinute}, {KeyRateAnalysisPerDay, &p.Rate.AnalysisPerDay},
		{KeyRatePlanPerMinute, &p.Rate.PlanPerMinute}, {KeyRatePlanPerDay, &p.Rate.PlanPerDay},
		{KeyRateConfirmPerMinute, &p.Rate.ConfirmPerMinute}, {KeyRateConfirmPerDay, &p.Rate.ConfirmPerDay},
		{KeyRateMaxInFlightJobs, &p.Rate.MaxInFlightJobs},
		{KeyRateAnnotatePerMinute, &p.Rate.AnnotatePerMinute}, {KeyRateAnnotatePerDay, &p.Rate.AnnotatePerDay},
	} {
		if v, ok := intOf(x.key, 0, 100000); ok {
			*x.dst = v
		}
	}
	if raw, ok := kv[KeyConfirmThreshold]; ok {
		if v, err := strconv.ParseInt(raw, 10, 64); err != nil || v < 0 || v > 100000000 {
			warn(KeyConfirmThreshold, raw, "不是 0~100000000 的整数")
		} else {
			p.Cfg.ConfirmThresholdPoints = v
		}
	}
	if raw, ok := kv[KeyAnalysisRules]; ok {
		if rc, err := analysisfix.ParseConfig(raw); err != nil {
			warn(KeyAnalysisRules, raw, "纠偏规则配置非法（"+err.Error()+"）")
		} else {
			p.Cfg.AnalysisRules = rc
		}
	}
	if raw, ok := kv[KeyPlaceholderText]; ok {
		if n := len([]rune(raw)); n > 40 {
			warn(KeyPlaceholderText, raw, "超过 40 字")
		} else {
			p.Cfg.PlaceholderText = raw
		}
	}
	return p
}

// Runtime 按运行参数持有当前的内核实例：参数变了（60 秒内）就重建一个 core.Service（无状态，重建便宜）。
type Runtime struct {
	base         core.Config
	longformYAML bool
	// enabledYAML 总开关的文件默认值（Replicate.Enabled）：库里 replicate.enabled 缺省或非法时用它
	enabledYAML bool
	build       func(cfg core.Config, longform bool) *core.Service
	load        loader
	now         func() time.Time

	mu      sync.Mutex
	at      time.Time
	svc     *core.Service
	enabled bool
	rate    RateParams
	loaded  bool // 至少成功读过一次运行参数（后台强制收尾据此拒绝在没读到参数时动钱）
	sig     string
	warned  time.Time
}

func newRuntime(base core.Config, longformYAML bool, load loader, build func(core.Config, bool) *core.Service) *Runtime {
	return &Runtime{base: base, longformYAML: longformYAML, build: build, load: load, now: time.Now}
}

// Service 当前内核与运行时总开关。
func (r *Runtime) Service(ctx context.Context) (*core.Service, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.svc != nil && now.Sub(r.at) < runtimeTTL {
		return r.svc, r.enabled
	}
	r.at = now
	var kv map[string]string
	if r.load != nil {
		m, err := r.load(ctx)
		if err != nil {
			if now.Sub(r.warned) > 10*time.Minute {
				r.warned = now
				logx.WithContext(ctx).Errorf("replicate: 读运行参数失败，沿用上一次: %v", err)
			}
			if r.svc != nil {
				return r.svc, r.enabled
			}
		} else {
			kv = m
			r.loaded = true
		}
	}
	p := applyParams(r.base, r.longformYAML, kv)
	if !validBool(kv[KeyEnabled]) { // 库里没配（或读库失败 / 值非法）：用文件配置
		p.Enabled = r.enabledYAML
	}
	sig := signature(p)
	if r.svc == nil || sig != r.sig {
		r.svc, r.sig = r.build(p.Cfg, *p.Longform), sig
		if r.svc != nil {
			logx.WithContext(ctx).Infof("replicate: 运行参数生效 enabled=%v longform=%v autoRetry=%d inFlight=%d font=%v",
				p.Enabled, *p.Longform, p.Cfg.AutoRetryMax, p.Cfg.MaxInFlight, p.Cfg.FontRid != "")
			if p.Cfg.FontRid == "" {
				logx.WithContext(ctx).Infof("replicate: WARN 没配 %s：失败段占位用英文文案，成片不叠字", KeyFontRid)
			}
		}
	}
	r.enabled = p.Enabled
	r.rate = p.Rate
	return r.svc, r.enabled
}

// validBool 库里的布尔值能否识别（与 applyParams 的 boolOf 同一套写法；空串缺省按「没配」）。
func validBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "on", "yes", "false", "0", "off", "no":
		return true
	}
	return false
}

// Loaded 是否至少成功读过一次 afv_system_config（没有 loader 时视为已读）。
func (r *Runtime) Loaded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded || r.load == nil
}

// Rate 当前限频参数（随 Service 一起 60 秒刷新；还没读过时为默认）。
func (r *Runtime) Rate() RateParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.svc == nil {
		return DefaultRate()
	}
	return r.rate
}

func signature(p Params) string {
	c := p.Cfg
	return strings.Join([]string{
		strconv.FormatBool(*p.Longform), strconv.Itoa(c.AutoRetryMax), strconv.Itoa(c.RedoMaxPerSegment), strconv.Itoa(c.RedoMaxPerJob),
		c.ContinueWindow.String(), c.PlanTTL.String(), strconv.Itoa(c.MaxInFlight), strconv.Itoa(c.Limits.MaxSegments),
		strconv.FormatFloat(c.Limits.OverlapSec, 'f', 3, 64), c.MaxCostCNY, c.VisionModelGroup, c.WriterModelGroup, strings.Join(c.Models, ","), c.DefaultModel, c.FontRid, c.PlaceholderText,
		strconv.FormatBool(c.ModerationOff), strconv.FormatBool(c.ModerationFailOpen), strconv.FormatBool(c.ModerationReviewPass),
		c.ModerationTextGroup, c.ModerationImageGroup, c.ModerationRejectTTL.String(),
		c.ModerationConfirmWait.String(), c.ModerationPlanWait.String(),
		strconv.FormatInt(c.ConfirmThresholdPoints, 10), analysisfix.VersionOf(c.AnalysisRules),
	}, "|")
}
