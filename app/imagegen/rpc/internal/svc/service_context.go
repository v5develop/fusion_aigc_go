package svc

import (
	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/linkparse"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Config      config.Config
	Creation    model.AfvImageCreationModel
	ActionVideo model.AfvActionVideoModel
	// DigitalHumanRpc 解析动作迁移的人物图来源
	DigitalHumanRpc digitalhuman.DigitalHuman
	CopywritingRpc  copywriting.Copywriting
	Video           model.AfvVideoCreationModel
	Shared          *model.SharedModel
	Aigc            *aigc.Client
	Asset           *asset.Writer
	Storage         storage.Store
	LinkParse       *linkparse.Client
	Moderation      moderation.Checker
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		Config:          c,
		Creation:        model.NewAfvImageCreationModel(conn),
		ActionVideo:     model.NewAfvActionVideoModel(conn),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		Video:           model.NewAfvVideoCreationModel(conn),
		Shared:          shared,
		Aigc:            aigc.New(c.Aigc),
		Asset:           asset.NewWriter(conn),
		Storage:         storage.New(c.Storage, conn),
		LinkParse:       linkparse.New(c.VideoRemake.LinkParse),
		Moderation:      moderation.New(c.Moderation.Mode, shared.BlockWords),
	}
}
