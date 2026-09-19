package svc

import (
	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/config"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/semaphore"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/tts"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Config    config.Config
	Human     model.AfvDigitalHumanModel
	Portrait  model.AfvDigitalHumanPortraitModel
	Voice     model.AfvDigitalHumanVoiceModel
	Option    model.AfvDigitalHumanOptionModel
	Agreement model.AfvUserAgreementModel
	Shared    *model.SharedModel

	Aigc       *aigc.Client
	TTS        *tts.Client
	Storage    storage.Store
	Moderation moderation.Checker
	// Sem 按模型限制并发（afv_ai_model.max_concurrency）
	Sem *semaphore.Group
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		Config:     c,
		Human:      model.NewAfvDigitalHumanModel(conn),
		Portrait:   model.NewAfvDigitalHumanPortraitModel(conn),
		Voice:      model.NewAfvDigitalHumanVoiceModel(conn),
		Option:     model.NewAfvDigitalHumanOptionModel(conn),
		Agreement:  model.NewAfvUserAgreementModel(conn),
		Shared:     shared,
		Aigc:       aigc.New(c.Aigc),
		TTS:        tts.New(),
		Storage:    storage.New(c.Storage, conn),
		Moderation: moderation.New(c.Moderation.Mode, shared.BlockWords),
		Sem:        semaphore.NewGroup(),
	}
}
