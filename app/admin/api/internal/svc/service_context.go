package svc

import (
	"aigc-life/app/admin/api/internal/config"
	"aigc-life/app/admin/api/internal/middleware"
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/imagegen/rpc/imagegen"
	"aigc-life/app/note/rpc/note"
	"aigc-life/pkg/adminuser"

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
	AdminUser       *adminuser.Model
}

func NewServiceContext(c config.Config) *ServiceContext {
	var conn sqlx.SqlConn
	if c.DataSource != "" {
		conn = sqlx.NewMysql(c.DataSource)
	}
	return &ServiceContext{
		Config:          c,
		AdminAuth:       middleware.NewAdminAuthMiddleware(c.Auth.Mode, c.Auth.Jwt).Handle,
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		NoteRpc:         note.NewNote(zrpc.MustNewClient(c.NoteRpc)),
		ImageGenRpc:     imagegen.NewImageGen(zrpc.MustNewClient(c.ImageGenRpc)),
		AdminUser:       adminuser.New(conn),
	}
}
