package svc

import (
	"context"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/taskcenter"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config config.Config
	// Conn go-zero 连接池；bun 复用它的 RawDB()（设计方案 §9.5）
	Conn       sqlx.SqlConn
	DB         *bun.DB
	Repo       *repo.Repo
	Billing    *billing.Service
	Asset      *asset.Writer
	TaskCenter *taskcenter.Syncer
	// Gateway ai-gateway（image / video / i2p）；BaseURL 为空时 Enabled()=false
	Gateway *aigateway.Client
	// Media 媒体服务（裁剪 / 转存 / 拉入）；未配置为 nil
	Media *mediasvc.Client
	// Java 主项目兼容接口（放大 / 扩图 / 重绘 / 任务查询 / 价目）
	Java *javatool.Client
	// Studio 试穿走 studio-rpc；未配置为 nil
	Studio studio.Studio
	// Bearers 用户最近一次请求带来的 Bearer（轮询 Java 任务时代用户查询；只在内存，不落库）
	Bearers *BearerCache
	// TaskSync 请求路径上的任务中心同步（异步、合并、带重试），不占 RunNode / CancelRun 的响应时间；nil 时不做
	TaskSync *AsyncSync
	// WatchRun 刚转运行中的 gateway run 交给长轮询协程（Poller 启动时挂上；nil 或名额满时由扫描兜底）。
	// 调用方须已放掉该 run 的租约：长轮询协程要自己抢租约。
	WatchRun func(runID int64)
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	db, err := bundb.New(conn)
	if err != nil {
		logx.Must(err)
	}
	sc := &ServiceContext{
		Config: c, Conn: conn, DB: db, Repo: repo.New(db),
		Billing:    billing.New(c.Billing, conn),
		Asset:      asset.NewWriter(conn),
		TaskCenter: taskcenter.New(conn, 10*time.Minute, TaskSources(c)...),
		Gateway:    newGateway(c),
		Media:      newMediaSvc(c),
		Java:       javatool.New(c.Java.BaseURL, time.Duration(c.Java.Timeout)*time.Second),
		Studio:     newStudio(c),
		Bearers:    NewBearerCache(2 * time.Hour),
	}
	sc.TaskSync = NewAsyncSync(sc.TaskCenter.Sync)
	return sc
}

// TaskSources image / video 两类 run 同步进任务中心（tool_* 不进：裁剪不计费，Java / studio 工具由对方记任务中心，
// 反推是文本中间步骤）。同一张表两个 kind，前缀都是 cv（主键全表唯一，Ref 不会撞）。
func TaskSources(c config.Config) []taskcenter.Source {
	mk := func(kind, entry, typ string) taskcenter.Source {
		return taskcenter.Source{
			Table: "afv_canvas_node_run", Prefix: c.TaskIDPrefix, Kind: kind, EntryCode: entry, Type: typ,
			Include: "task_id IS NOT NULL",
			// 0 排队 / 1 运行中 → 处理中；2 成功；3 失败；4 已取消
			Status:         "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 WHEN 4 THEN 4 ELSE 1 END",
			Result:         "COALESCE(video_url, JSON_UNQUOTE(JSON_EXTRACT(candidates, '$[0].url')))",
			Results:        "IF(candidates IS NULL, NULL, JSON_EXTRACT(candidates, '$[*].url'))",
			Cover:          "JSON_UNQUOTE(JSON_EXTRACT(candidates, '$[0].url'))",
			Fail:           "error_reason",
			Prompt:         "LEFT(JSON_UNQUOTE(JSON_EXTRACT(request, '$.prompt')), 500)",
			BillingRequest: "s.`billing_request_id`",
			RequestPairs: "'prompt', LEFT(JSON_UNQUOTE(JSON_EXTRACT(request, '$.prompt')), 500), 'kind', kind, " +
				"'canvasId', canvas_id, 'nodeId', node_id, 'runId', run_id, 'modelGroup', model_group, 'request', request",
			ExecTaskID:    "task_id",
			CreateTimeCol: "created_at", UpdateTimeCol: "updated_at",
		}
	}
	return []taskcenter.Source{
		mk("image", c.EntryCodes.Image, "image"),
		mk("video", c.EntryCodes.Video, "video"),
	}
}

func newGateway(c config.Config) *aigateway.Client {
	g := c.AiGateway
	return aigateway.New(aigateway.Config{
		BaseURL: g.BaseURL, KeyID: g.KeyID, Secret: g.Secret,
		Timeout:      time.Duration(g.TimeoutSeconds) * time.Second,
		LongPollWait: time.Duration(g.LongPollWait) * time.Second,
	})
}

func newMediaSvc(c config.Config) *mediasvc.Client {
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

// newStudio studio-rpc 客户端：没配 Etcd / Endpoints / Target 时为 nil（试穿返回「暂未开放」）。
// 用 NewClient（不 Must）：studio 暂时不可达不影响画布其余功能启动。
func newStudio(c config.Config) studio.Studio {
	s := c.StudioRpc
	if len(s.Etcd.Hosts) == 0 && len(s.Endpoints) == 0 && s.Target == "" {
		return nil
	}
	cli, err := zrpc.NewClient(s)
	if err != nil {
		logx.Errorf("canvas: studio-rpc 客户端初始化失败（试穿暂不可用）: %v", err)
		return nil
	}
	return studio.NewStudio(cli)
}

// BearerCache 用户 → 最近一次的 Authorization 原文。
//
// 为什么需要：Java 兼容工具（放大 / 扩图 / 重绘）提交后要轮询 GET /api/v1/tasks/{taskUid}，该接口按当前登录用户鉴权。
// 前端每 1.2s 轮询 GET /canvas/runs/{id}，每次都会把最新的 Bearer 带进来（网关 → gRPC metadata），这里记住它，
// 后台轮询器代用户查询时用。只放内存、不落库、过期即丢；进程重启后由下一次前端轮询补上。
type BearerCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[int64]bearerEntry
}

type bearerEntry struct {
	token string
	at    time.Time
}

func NewBearerCache(ttl time.Duration) *BearerCache {
	return &BearerCache{ttl: ttl, m: map[int64]bearerEntry{}}
}

// Put 记住用户的 Bearer（空串忽略）。
func (b *BearerCache) Put(userID int64, token string) {
	if b == nil || userID <= 0 || strings.TrimSpace(token) == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.m[userID] = bearerEntry{token: token, at: time.Now()}
	if len(b.m) > 10000 { // 顺手清过期，防止只增不减
		for k, v := range b.m {
			if time.Since(v.at) > b.ttl {
				delete(b.m, k)
			}
		}
	}
}

// Get 取用户的 Bearer；没有或已过期返回空串。
func (b *BearerCache) Get(userID int64) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[userID]
	if !ok || time.Since(e.at) > b.ttl {
		return ""
	}
	return e.token
}

// Delete 丢掉用户缓存的 Bearer（Java 回 401：令牌已失效，等前端下一次请求带新的进来）。
func (b *BearerCache) Delete(userID int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, userID)
}

// AsyncSync 把任务中心同步移出请求路径：Kick 立即返回；后台同一时刻只跑一轮，跑的期间再有 Kick 只记一个「脏」标记，
// 本轮结束后补跑一轮（合并多次请求）。每轮暂时失败最多再试 2 次（退避 0.5s / 1s），仍失败只记日志——
// Poller 每个扫描间隔也会全量窗口同步一次，这里只是让新 run 更快出现在任务中心。
type AsyncSync struct {
	sync    func(ctx context.Context) error
	backoff []time.Duration
	timeout time.Duration

	mu      sync.Mutex
	running bool
	dirty   bool
	wg      sync.WaitGroup
}

// NewAsyncSync fn 为 nil 时 Kick 是空操作。
func NewAsyncSync(fn func(ctx context.Context) error) *AsyncSync {
	return &AsyncSync{sync: fn, backoff: []time.Duration{500 * time.Millisecond, time.Second}, timeout: 30 * time.Second}
}

// Kick 请求一轮同步（不阻塞）。
func (a *AsyncSync) Kick() {
	if a == nil || a.sync == nil {
		return
	}
	a.mu.Lock()
	if a.running {
		a.dirty = true
		a.mu.Unlock()
		return
	}
	a.running = true
	a.wg.Add(1)
	a.mu.Unlock()
	go a.loop()
}

// Wait 等后台这一轮（含补跑）结束（单测 / 优雅退出用）。
func (a *AsyncSync) Wait() {
	if a != nil {
		a.wg.Wait()
	}
}

func (a *AsyncSync) loop() {
	defer a.wg.Done()
	for {
		a.once()
		a.mu.Lock()
		if !a.dirty {
			a.running = false
			a.mu.Unlock()
			return
		}
		a.dirty = false
		a.mu.Unlock()
	}
}

func (a *AsyncSync) once() {
	defer func() {
		if r := recover(); r != nil {
			logx.Errorf("canvas: 任务中心异步同步 panic: %v", r)
		}
	}()
	var err error
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
		err = a.sync(ctx)
		cancel()
		if err == nil || i >= len(a.backoff) {
			break
		}
		time.Sleep(a.backoff[i])
	}
	if err != nil {
		logx.Errorf("canvas: 任务中心异步同步失败（Poller 下一轮兜底）: %v", err)
	}
}
