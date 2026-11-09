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
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/appuser"
	"aigc-local-life/pkg/fusion"
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
	Union        *unionauth.Client
	AppUser      *appuser.Model
	Session      *session.Store
	// PosePreviewQuota 姿态配对预览的按用户配额（Config.Studio.PosePreview*，计数在会话 Redis）
	PosePreviewQuota *userquota.Limiter
	// MaskQuota 区域蒙版的按用户配额（Config.Studio.Mask*，计数在会话 Redis）
	MaskQuota *userquota.Limiter
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
	}
	sess := session.New(c.Auth.Session)
	var quotaRedis redis.UniversalClient
	if rc := sess.Redis(); rc != nil { // 别把 nil 指针装进接口
		quotaRedis = rc
	} else {
		logx.Info("gateway: 未配置会话 Redis，姿态配对预览配额退回进程内计数（多实例各算各的，仅开发）")
	}
	return &ServiceContext{
		Config:           c,
		Auth:             middleware.NewAuthMiddleware(c.Auth.Mode, c.Auth.Jwt, sess).Handle,
		CopywritingRpc:   copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc:  digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:          note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:      imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		StudioRpc:        studio.NewStudio(zrpc.MustNewClient(c.StudioRpc)),
		AssetLibRpc:      assetlib.NewAssetlib(zrpc.MustNewClient(c.AssetLibRpc)),
		CanvasRpc:        canvas.NewCanvas(zrpc.MustNewClient(c.CanvasRpc)),
		CanvasBearer:     middleware.NewCanvasBearerMiddleware().Handle,
		Storage:          storage.New(c.Storage, conn),
		Union:            unionauth.New(c.Union),
		AppUser:          appuser.New(conn).WithRegistrar(fusionRegistrar{fusion.New(c.Fusion)}),
		Session:          sess,
		PosePreviewQuota: userquota.New("studio:pose_preview", c.Studio.PosePreviewPerMinute, c.Studio.PosePreviewPerDay, quotaRedis),
		MaskQuota:        userquota.New("studio:mask", c.Studio.MaskPerMinute, c.Studio.MaskPerDay, quotaRedis),
	}
}
