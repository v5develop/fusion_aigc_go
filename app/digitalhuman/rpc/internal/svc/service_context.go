package svc

import (
	"aigc-local-life/pkg/billing"
	"time"

	"aigc-local-life/pkg/taskcenter"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/config"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/semaphore"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/tts"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Billing   *billing.Service
	Config    config.Config
	Human     model.AfvDigitalHumanModel
	Portrait  model.AfvDigitalHumanPortraitModel
	Voice     model.AfvDigitalHumanVoiceModel
	Option    model.AfvDigitalHumanOptionModel
	Agreement model.AfvUserAgreementModel
	Shared    *model.SharedModel

	Aigc       *aigc.Client
	Asset      *asset.Writer
	TTS        *tts.Client
	Storage    storage.Store
	Moderation moderation.Checker
	// Sem 按模型限制并发（afv_ai_model.max_concurrency）
	Sem        *semaphore.Group
	TaskCenter *taskcenter.Syncer
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	entry := c.Asset.Video.EntryCode
	if entry == "" {
		entry = "digitalHuman"
	}
	return &ServiceContext{
		TaskCenter: taskcenter.New(conn, 10*time.Minute, taskcenter.Source{
			Table: "afv_digital_human", Prefix: "dh", EntryCode: entry, Type: "video",
			Status: "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 ELSE 1 END",
			Result: "video_url", Cover: "image_url", Fail: "fail_reason", Prompt: "script",
		}),
		Config:     c,
		Billing:    billing.New(c.Billing, conn),
		Human:      model.NewAfvDigitalHumanModel(conn),
		Portrait:   model.NewAfvDigitalHumanPortraitModel(conn),
		Voice:      model.NewAfvDigitalHumanVoiceModel(conn),
		Option:     model.NewAfvDigitalHumanOptionModel(conn),
		Agreement:  model.NewAfvUserAgreementModel(conn),
		Shared:     shared,
		Aigc:       aigc.New(c.Aigc),
		Asset:      asset.NewWriter(conn),
		TTS:        tts.New(),
		Storage:    storage.New(c.Storage, conn),
		Moderation: moderation.New(c.Moderation.Mode, shared.BlockWords),
		Sem:        semaphore.NewGroup(),
	}
}
