package httpapi

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/notify"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/workspace"

	"github.com/redis/go-redis/v9"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 媒体服务客户端须满足内核的 Media / Cutter（区间切段由媒体服务 /v1/mediasvc/video/cut 提供）。
var (
	_ core.Media       = (*mediasvc.Client)(nil)
	_ core.Cutter      = (*mediasvc.Client)(nil)
	_ core.Gateway     = (*aigateway.Client)(nil)
	_ core.Composer    = (*mediasvc.Client)(nil)
	_ core.Signer      = (*mediasvc.Client)(nil)
	_ core.Librarian   = (*mediasvc.Client)(nil)
	_ core.AssetWriter = (*asset.Writer)(nil)
	_ core.Notifier    = (*notify.Service)(nil)
)

// BuildOption MustBuild 的可选装配。
type BuildOption func(*buildOpts)

type buildOpts struct {
	store storage.Store
	rds   redis.UniversalClient
}

// WithStorage 用调用方（gateway-api）的存储实例校验 http(s) 源视频 / 参考图归属（与上传接口同一份配置）。
func WithStorage(st storage.Store) BuildOption { return func(o *buildOpts) { o.store = st } }

// WithRedis 限频计数用的 Redis（gateway 会话 Redis，多实例共享）；不传则退回进程内计数并 WARN。
func WithRedis(rds redis.UniversalClient) BuildOption { return func(o *buildOpts) { o.rds = rds } }

// Module 装配好的视频做同款。
type Module struct {
	rt     *Runtime
	Poller *Poller
	// limiter 分析 / 方案 / 确认限频（ratelimit.go）
	limiter *rateLimiter
	// taskSync 写接口成功后异步触发一轮任务中心同步（轮询器另有周期同步兜底）
	taskSync *taskcenter.AsyncSync
}

// MustBuild 由配置与库连接装配。conn 为 nil（没配 DataSource）返回 nil：接口回「功能暂未开放」，不启动轮询器。
// c.Enabled 只是运行时总开关的默认值（afv_system_config.replicate.enabled 存在时以库为准，数据库 > 文件配置）：
// 不论开关如何都装配，关着时只拦写接口，历史 / 我的方案照常可读、在途任务照常收尾。
func MustBuild(c Conf, conn sqlx.SqlConn, opts ...BuildOption) *Module {
	var bo buildOpts
	for _, o := range opts {
		o(&bo)
	}
	if conn == nil {
		logx.Error("replicate: 已开启但没配 DataSource，视频做同款不装配")
		return nil
	}
	db, err := bundb.New(conn)
	logx.Must(err)
	store := repo.New(db)
	notifier := notify.New(conn)
	assets := asset.NewWriter(conn)
	words := moderation.New("keyword", blockWords(conn))
	ledger := core.NewBillingLedger(billing.New(c.Billing, conn))
	var gw *aigateway.Client
	if strings.TrimSpace(c.AiGateway.BaseURL) != "" {
		gw = aigateway.New(aigateway.Config{
			BaseURL: c.AiGateway.BaseURL, KeyID: c.AiGateway.KeyID, Secret: c.AiGateway.Secret,
			Timeout:      time.Duration(c.AiGateway.TimeoutSeconds) * time.Second,
			LongPollWait: time.Duration(c.AiGateway.LongPollWait) * time.Second,
		})
	} else {
		logx.Error("replicate: 没配 Replicate.AiGateway.BaseURL，分析的台词 / 画面理解与出片不可用")
	}
	media := newMedia(c)
	if media == nil {
		logx.Error("replicate: 没配 Replicate.Media，分析（落桶 / 抽帧）与区间切段不可用")
	}
	base := core.DefaultConfig()
	// 续期下限按真实预留有效期推导：与 pkg/billing 的 Hold.VideoMinutes 同源，运维调大/调小都不会让续期缩短到期或晚于回收
	if m := c.Billing.Hold.VideoMinutes; m > 0 {
		base.HoldTTL = time.Duration(m) * time.Minute
		base.RenewAfter = 0 // 交给 withDefaults 按新的 HoldTTL 重新推导（它只会上调，调小 HoldTTL 时旧值会晚于回收）
	}
	if p := strings.TrimSpace(c.KeyPrefix); core.ValidKeyPrefix(p) {
		base.KeyPrefix = p
	} else {
		logx.Errorf("replicate: 配置拒收 判据=KEY_PREFIX_INVALID 键=Replicate.KeyPrefix 原值=%q（只允许 [A-Za-z0-9._-]、≤32 字符），按空前缀处理", c.KeyPrefix)
	}
	if c.URLTTLSeconds > 0 {
		base.URLTTL = time.Duration(min(c.URLTTLSeconds, 86400)) * time.Second
	}
	st := bo.store
	if st == nil && c.Storage != nil {
		st = storage.New(*c.Storage, conn)
	}
	owner := urlOwner(st, c.InputURLPrefixes)
	if owner == nil {
		logx.Error("replicate: 没装配存储（WithStorage / Replicate.Storage / InputURLPrefixes 都没配），http(s) 源视频与参考图一律拒收，只接受 mr:// 上传引用")
	}
	base.ASRModelGroup = strings.TrimSpace(c.ASRModelGroup)
	if v := strings.TrimSpace(c.VisionModelGroup); v != "" {
		base.VisionModelGroup = v
	}
	if c.WriterEnabled { // 写手默认开：没单独配模型组就用画面理解的 chat 模型组
		base.WriterModelGroup = strings.TrimSpace(c.WriterModelGroup)
		if base.WriterModelGroup == "" {
			base.WriterModelGroup = base.VisionModelGroup
		}
	}
	base.Models, base.DefaultModel = c.Models, strings.TrimSpace(c.DefaultModel)
	// ETA 历史校准：进程内一份（运行参数变了重建 Service 也共用同一份缓存），按历史成功 job 的每段耗时分桶出 P50 / P90
	etaCal := core.NewEtaCalibrator(store, core.EtaCalibration{})
	build := func(cfg core.Config, longform bool) *core.Service {
		var opts []core.Option
		if gw != nil { // 别把 nil 指针装进接口
			opts = append(opts, core.WithGateway(gw))
		}
		if owner != nil {
			opts = append(opts, core.WithURLOwner(owner))
		}
		if media != nil {
			opts = append(opts, core.WithMedia(media), core.WithComposer(media), core.WithSigner(media))
			if longform {
				opts = append(opts, core.WithCutter(media))
			}
			if c.Asset.Enabled {
				opts = append(opts, core.WithDelivery(store, media, assets, core.DeliveryConfig{EntryCode: c.Asset.EntryCode}))
			}
		}
		opts = append(opts, core.WithModeration(store, store, words), core.WithNotifier(notifier), core.WithLibrary(store), core.WithEtaCalibrator(etaCal),
			core.WithWorkspace(func(ctx context.Context, uid int64) (int64, error) { return workspace.PersonalID(ctx, conn, uid) }))
		return core.New(cfg, store, ledger, opts...)
	}
	rt := newRuntime(base, c.LongformEnabled, systemConfigLoader(conn), build)
	rt.enabledYAML = c.Enabled
	ttl := time.Duration(c.LeaseSeconds) * time.Second
	if ttl < 30*time.Second {
		ttl = 120 * time.Second
	}
	m := &Module{rt: rt, Poller: newPoller(rt, newLeaseGuard(db, ttl), time.Duration(c.PollIntervalMs)*time.Millisecond, c.BatchSize),
		limiter: newRateLimiter(bo.rds, store.CountInFlight)}
	if c.TaskCenterEnabled {
		sy := taskcenter.New(conn, 10*time.Minute, TaskSource())
		m.taskSync = taskcenter.NewAsyncSync("replicate", sy.Sync)
		m.Poller.taskSync = sy.Sync
	} else {
		logx.Info("replicate: Replicate.TaskCenterEnabled=false，视频做同款不进任务中心")
	}
	return m
}

// blockWords 敏感词：与 AI 文案 / studio 共用 afv_copywriting_option 的 BLOCKWORD 词库（运营维护）。
func blockWords(conn sqlx.SqlConn) moderation.WordSource {
	return func(ctx context.Context) ([]string, error) {
		var words []string
		q := "SELECT `code` FROM afv_copywriting_option WHERE `option_type` = 'BLOCKWORD' AND `status` = 1 AND `deleted` = 0"
		return words, conn.QueryRowsCtx(ctx, &words, q)
	}
}

func newMedia(c Conf) *mediasvc.Client {
	m := c.Media
	var fallback []string
	for _, u := range append([]string{m.BaseURL}, m.BaseURLs...) {
		if strings.TrimSpace(u) != "" {
			fallback = append(fallback, u)
		}
	}
	if strings.TrimSpace(m.NacosServerAddr) != "" {
		return mediasvc.Discover(mediasvc.DiscoveryConfig{
			ServerAddr: m.NacosServerAddr, Namespace: m.NacosNamespace, Group: m.NacosGroup,
			ServiceName: m.NacosServiceName, Username: m.NacosUsername, Password: m.NacosPassword,
		}, m.Token, fallback)
	}
	if len(fallback) == 0 {
		return nil
	}
	return mediasvc.New(fallback, m.Token)
}
