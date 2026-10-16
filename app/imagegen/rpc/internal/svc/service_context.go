package svc

import (
	"aigc-life/pkg/billing"
	"time"

	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/linkparse"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Billing     *billing.Service
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
	TaskCenter      *taskcenter.Syncer
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		TaskCenter:      taskcenter.New(conn, 10*time.Minute, taskSources(c)...),
		Config:          c,
		Billing:         billing.New(c.Billing, conn),
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

// taskSources 本服务的成品同步进任务中心。入口码与素材库一致（配置里的 AssetEntryCode），没配就用 kind。
func taskSources(c config.Config) []taskcenter.Source {
	code := func(v, kind string) string {
		if v != "" {
			return v
		}
		return kind
	}
	typ := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	image := func(kind string, k config.KindConf, def string) taskcenter.Source {
		t := typ(k.AssetType, def)
		s := taskcenter.Source{
			Table: "afv_image_creation", Prefix: "img", Kind: kind, EntryCode: code(k.AssetEntryCode, kind), Type: t,
			Status: "CASE status WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE 3 END",
			Result: "JSON_UNQUOTE(JSON_EXTRACT(image_urls, '$[0]'))", Fail: "fail_reason", Prompt: "content",
		}
		if t == "video" {
			s.Cover = "input_image_url" // 成品是视频：人物图当封面
		} else {
			s.Results = "image_urls"
		}
		return s
	}
	video := func(kind, entry, t string) taskcenter.Source {
		return taskcenter.Source{
			Table: "afv_video_creation", Prefix: "vid", Kind: kind, EntryCode: code(entry, kind), Type: typ(t, "video"),
			// 剧本阶段（构思中 / 待确认）不算任务，开始出片才进任务中心
			Include: "status IN (2,3) OR (status = 4 AND gen_task_id IS NOT NULL)",
			Status:  "CASE status WHEN 2 THEN 1 WHEN 3 THEN 2 ELSE 3 END",
			Result:  "video_url", Cover: "COALESCE(char_url, cover_url, video_url)", Fail: "fail_reason",
			Prompt: "COALESCE(user_text, title)",
		}
	}
	return []taskcenter.Source{
		image("poster", c.Poster, "image"),
		image("longImage", c.LongImage, "image"),
		image("actionTransfer", c.ActionTransfer, "video"),
		image("voiceover", c.Voiceover, "video"),
		video("exploreShop", c.ExploreShop.AssetEntryCode, c.ExploreShop.AssetType),
		video("shortDrama", c.ShortDrama.AssetEntryCode, c.ShortDrama.AssetType),
		video("videoRemake", c.VideoRemake.AssetEntryCode, c.VideoRemake.AssetType),
	}
}
