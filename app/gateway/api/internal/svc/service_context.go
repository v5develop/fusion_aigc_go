package svc

import (
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/gateway/api/internal/config"
	"aigc-life/app/gateway/api/internal/middleware"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/app/note/rpc/note"
	"aigc-life/pkg/appuser"
	"aigc-life/pkg/fusion"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/unionauth"

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
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
	}
	return &ServiceContext{
		Config:          c,
		Auth:            middleware.NewAuthMiddleware(c.Auth.Mode, c.Auth.Jwt).Handle,
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:         note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:     imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		Storage:         storage.New(c.Storage, conn),
		Union:           unionauth.New(c.Union),
		AppUser:         appuser.New(conn).WithRegistrar(fusionRegistrar{fusion.New(c.Fusion)}),
	}
}
