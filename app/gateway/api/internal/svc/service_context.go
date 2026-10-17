package svc

import (
	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/config"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/pkg/appuser"
	"aigc-local-life/pkg/fusion"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/unionauth"

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
	Storage         storage.Store
	Union           *unionauth.Client
	AppUser         *appuser.Model
	Session         *session.Store
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
	}
	sess := session.New(c.Auth.Session)
	return &ServiceContext{
		Config:          c,
		Auth:            middleware.NewAuthMiddleware(c.Auth.Mode, c.Auth.Jwt, sess).Handle,
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:         note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:     imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		Storage:         storage.New(c.Storage, conn),
		Union:           unionauth.New(c.Union),
		AppUser:         appuser.New(conn).WithRegistrar(fusionRegistrar{fusion.New(c.Fusion)}),
		Session:         sess,
	}
}
