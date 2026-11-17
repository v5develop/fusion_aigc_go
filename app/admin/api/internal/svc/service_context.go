package svc

import (
	"context"
	"strings"

	"aigc-local-life/app/admin/api/internal/config"
	"aigc-local-life/app/admin/api/internal/middleware"
	assetlib "aigc-local-life/app/assetlib/rpc/assetlib_client"
	canvas "aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/app/plaza/core"
	"aigc-local-life/app/plaza/headline/httpapi"
	replicateadmin "aigc-local-life/app/replicate/admin"
	replicatehttp "aigc-local-life/app/replicate/httpapi"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/adminuser"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/dbpool"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/storage"

	"github.com/redis/go-redis/v9"
	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config          config.Config
	AdminAuth       rest.Middleware
	CopywritingRpc  copywriting.Copywriting
	DigitalHumanRpc digitalhuman.DigitalHuman
	NoteRpc         note.Note
	ImageGenRpc     imagegen.ImageGen
	StudioRpc       studio.Studio
	AssetLibRpc     assetlib.Assetlib
	CanvasRpc       canvas.Canvas
	AdminUser       *adminuser.Model
	Session         *session.Store
	// Plaza 广场模板域后台；没配 DataSource 时为 nil
	Plaza *core.Service
	// Headline 本周头条运营端 + 后台任务（Jobs 仅 PlazaHeadline.Jobs=true 时非 nil）；没配 DataSource 时接口回「功能暂未开放」
	Headline *httpapi.Headline

	// ReplicateAdmin 视频做同款后台（检索 / 详情 / 人工退点 / 强制收尾）；没配 DataSource 时为 nil（接口回 7003）
	ReplicateAdmin *replicateadmin.Service
	// ReplicateSwitch 视频做同款总开关读写（afv_system_config.replicate.enabled）；没配 DataSource 时为 nil（接口回 7003）
	ReplicateSwitch *replicatehttp.Switch
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
		dbpool.MustApply("admin-api", conn, c.DB)
	}
	sess := session.New(c.Auth.Session)
	plaza := newPlaza(c, conn, sess)
	hl := httpapi.MustBuild(c.DataSource, sessRedis(sess), c.PlazaHeadline, true, c.Plaza.AdminWriteEnabled)
	wireHeadline(plaza, hl)
	return &ServiceContext{
		ReplicateAdmin:  newReplicateAdmin(c, conn),
		ReplicateSwitch: replicatehttp.NewSwitch(conn, c.ReplicateAdmin.EnabledDefault),
		Plaza:           plaza,
		Headline:        hl,
		Config:          c,
		AdminAuth:       middleware.NewAdminAuthMiddleware(c.Auth.Mode, c.Auth.Jwt, session.New(c.Auth.Session), conn).Handle,
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:         note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:     imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		StudioRpc:       studio.NewStudio(zrpc.MustNewClient(c.StudioRpc)),
		AssetLibRpc:     assetlib.NewAssetlib(zrpc.MustNewClient(c.AssetLibRpc)),
		CanvasRpc:       canvas.NewCanvas(zrpc.MustNewClient(c.CanvasRpc)),
		AdminUser:       adminuser.New(conn),
		Session:         session.New(c.Auth.Session),
	}
}

// newPlaza 广场模板域后台服务：共用 DataSource 的连接池，Redis（目录版本号、定时任务锁）用会话 Redis。
func newPlaza(c config.Config, conn sqlx.SqlConn, sess *session.Store) *core.Service {
	if conn == nil {
		return nil
	}
	db, err := bundb.New(conn)
	if err != nil {
		logx.Errorf("admin: 广场模板域初始化失败: %v", err)
		return nil
	}
	svc := core.New(db, sessRedis(sess), c.Plaza)
	// 模板素材直传（公有桶）：存储配置在 Plaza.Upload.Storage，Mode=oss 时从 afv_system_config 读公有桶；默认 local = 不支持直传。
	if c.Plaza.Upload.Storage.Mode == "oss" {
		svc.SetUploader(core.NewUploader(storage.New(c.Plaza.Upload.Storage, conn), c.Plaza.Upload))
	}
	return svc
}

// sessRedis 会话 Redis（没配为 nil 接口，不是装着 nil 指针的接口）。
func sessRedis(sess *session.Store) redis.UniversalClient {
	if r := sess.Redis(); r != nil {
		return r
	}
	return nil
}

// wireHeadline 把头条域接到广场模板域留的三个调用点：彻底删除模板时清头条规则 / 抑制（同一事务）、
// 后台模板列表的 headlineBrief、详情的 headlineStatus。
func wireHeadline(plaza *core.Service, hl *httpapi.Headline) {
	if plaza == nil || hl == nil || hl.Svc == nil {
		return
	}
	h := hl.Svc
	plaza.SetHeadlineCleaner(func(ctx context.Context, tx bun.IDB, templateID int64) error {
		return h.PurgeTemplate(ctx, tx, templateID)
	})
	plaza.SetHeadlineBrief(func(ctx context.Context, ids []int64) (map[int64]any, error) {
		m, err := h.Briefs(ctx, ids)
		if err != nil || m == nil {
			return nil, err
		}
		out := make(map[int64]any, len(m))
		for id, b := range m {
			if b != nil {
				out[id] = b
			}
		}
		return out, nil
	})
	plaza.SetHeadlineStatus(func(ctx context.Context, id int64) (any, error) {
		v, err := h.StatusForAdmin(ctx, id)
		if err != nil || v == nil {
			return nil, err // 不把 (*TemplateStatusView)(nil) 装进 any
		}
		return v, nil
	})
}

// newReplicateAdmin 视频做同款后台：业务表经 app/replicate/repo（bun），动钱只经 pkg/billing（ReplicateAdmin.Billing）；
// 强制收尾用与 gateway 同源的运行参数（afv_system_config）并装通知器 / 任务中心同步（replicatehttp.AdminForcer）。
func newReplicateAdmin(c config.Config, conn sqlx.SqlConn) *replicateadmin.Service {
	if conn == nil {
		return nil
	}
	forcer, store, bill, err := replicatehttp.NewAdminForcer(conn, c.ReplicateAdmin.Billing, c.ReplicateAdmin.TaskCenterEnabled)
	if err != nil {
		logx.Errorf("admin: 视频做同款后台初始化失败: %v", err)
		return nil
	}
	ra := replicateadmin.New(store, bill, forcer)
	if m := c.ReplicateAdmin.Media; strings.TrimSpace(m.BaseURL) != "" { // 通过用户投稿时把成片复制进素材库桶
		ra.SetShareMedia(mediasvc.New([]string{m.BaseURL}, m.Token))
	}
	return ra
}
