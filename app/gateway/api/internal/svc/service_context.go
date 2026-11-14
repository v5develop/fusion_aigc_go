package svc

import (
	assetlib "aigc-local-life/app/assetlib/rpc/assetlib_client"
	canvas "aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/app/plaza/core"
	"aigc-local-life/app/plaza/headline/httpapi"
	replicate "aigc-local-life/app/replicate/httpapi"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/appuser"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/dbpool"
	"aigc-local-life/pkg/fusion"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/unionauth"
	"aigc-local-life/pkg/userquota"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config          config.Config
	Auth            rest.Middleware
	CopywritingRpc  copywriting.Copywriting
	DigitalHumanRpc digitalhuman.DigitalHuman
	NoteRpc         note.Note
	ImageGenRpc     imagegen.ImageGen
	StudioRpc       studio.Studio
	AssetLibRpc     assetlib.Assetlib
	CanvasRpc       canvas.Canvas
	// CanvasBearer 画布组：把 Authorization 原文透传给 canvas-rpc（调 Java 兼容工具）
	CanvasBearer rest.Middleware
	Storage      storage.Store
	// Plaza 广场模板域；没配 DataSource 时为 nil，接口回「功能暂未开放」
	Plaza *core.Service
	// Headline 本周头条用户端（不带后台任务）；没配 DataSource 时接口回「功能暂未开放」
	Headline *httpapi.Headline
	// Replicate 视频做同款；Replicate.Enabled=false 或没配 DataSource 时为 nil（接口回 7003，不启动轮询器）
	Replicate *replicate.Module
	Union     *unionauth.Client
	AppUser   *appuser.Model
	Session   *session.Store
	// PosePreviewQuota 姿态配对预览的按用户配额（Config.Studio.PosePreview*，计数在会话 Redis）
	PosePreviewQuota *userquota.Limiter
	// MaskQuota 区域蒙版的按用户配额（Config.Studio.Mask*，计数在会话 Redis）
	MaskQuota *userquota.Limiter
	// ColorFixAutoQuota 参考图自动校色的按用户配额（Config.Studio.ColorFixAuto*，计数在会话 Redis）
	ColorFixAutoQuota *userquota.Limiter
	// DetailsQuota 商拍「AI 找细节」的按用户配额（Config.Studio.Details*，计数在会话 Redis）
	DetailsQuota *userquota.Limiter
	// CanvasAgentQuota / CanvasAgentGenQuota 画布智能体工具调用 / 生成调用的按用户配额（Config.CanvasAgent.*，计数在会话 Redis）
	CanvasAgentQuota    *userquota.Limiter
	CanvasAgentGenQuota *userquota.Limiter
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
		dbpool.MustApply("gateway-api", conn, c.DB)
	}
	sess := session.New(c.Auth.Session)
	var quotaRedis redis.UniversalClient
	if rc := sess.Redis(); rc != nil { // 别把 nil 指针装进接口
		quotaRedis = rc
	} else {
		logx.Info("gateway: 未配置会话 Redis，姿态配对预览配额退回进程内计数（多实例各算各的，仅开发）")
	}
	plazaSvc := newPlaza(c, conn, sess)
	store := storage.New(c.Storage, conn)
	return &ServiceContext{
		Plaza:               plazaSvc,
		Headline:            httpapi.MustBuild(c.DataSource, quotaRedis, c.PlazaHeadline, false, c.Plaza.AdminWriteEnabled),
		Replicate:           replicate.MustBuild(c.Replicate, conn, replicate.WithStorage(store), replicate.WithRedis(quotaRedis)),
		Config:              c,
		Auth:                middleware.NewAuthMiddleware(c.Auth.Mode, c.Auth.Jwt, sess).Handle,
		CopywritingRpc:      copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc:     digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:             note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:         imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		StudioRpc:           studio.NewStudio(zrpc.MustNewClient(c.StudioRpc)),
		AssetLibRpc:         assetlib.NewAssetlib(zrpc.MustNewClient(c.AssetLibRpc)),
		CanvasRpc:           canvas.NewCanvas(zrpc.MustNewClient(c.CanvasRpc)),
		CanvasBearer:        middleware.NewCanvasBearerMiddleware().Handle,
		Storage:             store,
		Union:               unionauth.New(c.Union),
		AppUser:             appuser.New(conn).WithRegistrar(fusionRegistrar{fusion.New(c.Fusion)}),
		Session:             sess,
		PosePreviewQuota:    userquota.New("studio:pose_preview", c.Studio.PosePreviewPerMinute, c.Studio.PosePreviewPerDay, quotaRedis),
		MaskQuota:           userquota.New("studio:mask", c.Studio.MaskPerMinute, c.Studio.MaskPerDay, quotaRedis),
		ColorFixAutoQuota:   userquota.New("studio:colorfix_auto", c.Studio.ColorFixAutoPerMinute, c.Studio.ColorFixAutoPerDay, quotaRedis),
		DetailsQuota:        userquota.New("studio:details", c.Studio.DetailsPerMinute, c.Studio.DetailsPerDay, quotaRedis),
		CanvasAgentQuota:    userquota.New("canvas_agent:call", c.CanvasAgent.CallsPerMinute, c.CanvasAgent.CallsPerDay, quotaRedis),
		CanvasAgentGenQuota: userquota.New("canvas_agent:generate", c.CanvasAgent.GeneratePerMinute, c.CanvasAgent.GeneratePerDay, quotaRedis),
	}
}

// newPlaza 广场模板域：共用 DataSource 的连接池（bun），会话 Redis 用于目录版本号、配额与缓存，报价走 Java 内网。
func newPlaza(c config.Config, conn sqlx.SqlConn, sess *session.Store) *core.Service {
	if conn == nil {
		logx.Info("gateway: 未配置 DataSource，广场模板域接口回「功能暂未开放」")
		return nil
	}
	// 生产沿用默认 IP 盐直接拒绝启动（匿名限频 / 去重 / 缺口统计都按这个哈希）
	logx.Must(c.Plaza.CheckIPHashSalt(c.Mode))
	db, err := bundb.New(conn)
	if err != nil {
		logx.Errorf("gateway: 广场模板域初始化失败: %v", err)
		return nil
	}
	var rd redis.UniversalClient
	if r := sess.Redis(); r != nil {
		rd = r
	}
	s := core.New(db, rd, c.Plaza)
	if q := core.NewJavaQuoteClient(c.Plaza.Quote.JavaBaseURL, c.Plaza.Quote.ProbeUserID, sess); q != nil {
		s.SetQuoteSource(q)
	}
	if urls := c.Plaza.Upload.Mediasvc.BaseURLs; len(urls) > 0 {
		var assets core.OverlayAssets
		if c.Plaza.Overlay.AssetEnabled {
			assets = asset.NewWriter(conn)
		}
		s.SetOverlay(mediasvc.New(urls, c.Plaza.Upload.Mediasvc.Token), assets)
	} else {
		logx.Info("gateway: Plaza.Upload.Mediasvc.BaseURLs 未配置，模板文字叠加接口回「暂未开放」")
	}
	return s
}
