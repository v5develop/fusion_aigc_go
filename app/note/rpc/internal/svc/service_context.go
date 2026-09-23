package svc

import (
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/note/model"
	"aigc-life/app/note/rpc/internal/config"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config          config.Config
	Note            model.AfvImageTextNoteModel
	Shared          *model.SharedModel
	Aigc            *aigc.Client
	Asset           *asset.Writer
	Storage         storage.Store
	Moderation      moderation.Checker
	DigitalHumanRpc digitalhuman.DigitalHuman
	CopywritingRpc  copywriting.Copywriting
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		Config:          c,
		Note:            model.NewAfvImageTextNoteModel(conn),
		Shared:          shared,
		Aigc:            aigc.New(c.Aigc),
		Asset:           asset.NewWriter(conn),
		Storage:         storage.New(c.Storage, conn),
		Moderation:      moderation.New(c.Moderation.Mode, shared.BlockWords),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
	}
}
