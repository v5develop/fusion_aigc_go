package svc

import (
	"aigc-local-life/app/admin/api/internal/config"
	"aigc-local-life/app/admin/api/internal/middleware"
	assetlib "aigc-local-life/app/assetlib/rpc/assetlib_client"
	canvas "aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/adminuser"
	"aigc-local-life/pkg/dbpool"
	"aigc-local-life/pkg/session"

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
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
		dbpool.MustApply("admin-api", conn, c.DB)
	}
	return &ServiceContext{
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
