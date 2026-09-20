package svc

import (
	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Config     config.Config
	Creation   model.AfvImageCreationModel
	Shared     *model.SharedModel
	Aigc       *aigc.Client
	Asset      *asset.Writer
	Storage    storage.Store
	Moderation moderation.Checker
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		Config:     c,
		Creation:   model.NewAfvImageCreationModel(conn),
		Shared:     shared,
		Aigc:       aigc.New(c.Aigc),
		Asset:      asset.NewWriter(conn),
		Storage:    storage.New(c.Storage, conn),
		Moderation: moderation.New(c.Moderation.Mode, shared.BlockWords),
	}
}
