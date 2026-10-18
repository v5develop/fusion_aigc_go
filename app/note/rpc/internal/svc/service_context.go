package svc

import (
	"aigc-local-life/pkg/billing"
	"time"

	"aigc-local-life/pkg/taskcenter"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/config"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Billing         *billing.Service
	Config          config.Config
	Note            model.AfvImageTextNoteModel
	Shared          *model.SharedModel
	Aigc            *aigc.Client
	Asset           *asset.Writer
	Storage         storage.Store
	Moderation      moderation.Checker
	DigitalHumanRpc digitalhuman.DigitalHuman
	CopywritingRpc  copywriting.Copywriting
	TaskCenter      *taskcenter.Syncer
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	entry := c.Asset.EntryCode
	if entry == "" {
		entry = "imageTextNote"
	}
	return &ServiceContext{
		TaskCenter: taskcenter.New(conn, 10*time.Minute, taskcenter.Source{
			Table: "afv_image_text_note", Prefix: "note", EntryCode: entry, Type: "image",
			// 构思阶段不算任务，开始出图才进任务中心
			Include: "status IN (3,4,5)",
			Status:  "CASE status WHEN 3 THEN 1 WHEN 4 THEN 2 ELSE 3 END",
			Result:  "JSON_UNQUOTE(JSON_EXTRACT(image_urls, '$[0]'))", Results: "image_urls", Fail: "fail_reason", Prompt: "title",
			BillingRequest: "gen_task_id",
			RequestPairs: "'prompt', title, 'noteType', note_type, 'topic', topic, 'content', content, 'tags', tags, " +
				"'materials', material_urls, 'digitalHumanId', digital_human_id, 'shopProfileId', shop_profile_id, " +
				"'copywritingId', copywriting_id, 'imageCount', image_count",
			ExecTaskID: "COALESCE(gen_task_id, plan_task_id)",
			ExecParams: "JSON_OBJECT('imageCount', image_count)",
		}),
		Config:          c,
		Billing:         billing.New(c.Billing, conn),
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
