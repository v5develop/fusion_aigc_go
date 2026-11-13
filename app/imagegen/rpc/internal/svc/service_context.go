package svc

import (
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/dbpool"
	"strings"
	"time"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/linkparse"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

type ServiceContext struct {
	Billing     *billing.Service
	Config      config.Config
	Creation    model.LocalLifeCreationModel
	ActionVideo model.AfvActionVideoModel
	// DigitalHumanRpc 解析动作迁移的人物图来源
	DigitalHumanRpc digitalhuman.DigitalHuman
	CopywritingRpc  copywriting.Copywriting
	Video           model.AfvVideoCreationModel
	Shared          *model.SharedModel
	GenOpts         *model.GenOptionModel
	Aigc            *aigc.Client
	// MediaSvc 共享媒体服务；未配置为 nil
	MediaSvc   *mediasvc.Client
	Asset      *asset.Writer
	Storage    storage.Store
	LinkParse  *linkparse.Client
	Moderation moderation.Checker
	TaskCenter *taskcenter.Syncer
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	dbpool.MustApply("imagegen", conn, c.DB)
	shared := model.NewSharedModel(conn)
	return &ServiceContext{
		TaskCenter:      taskcenter.New(conn, 10*time.Minute, taskSources(c)...),
		Config:          c,
		Billing:         billing.New(c.Billing, conn),
		Creation:        model.NewLocalLifeCreationModel(conn),
		ActionVideo:     model.NewAfvActionVideoModel(conn),
		DigitalHumanRpc: digitalhuman.NewDigitalHuman(zrpc.MustNewClient(c.DigitalHumanRpc)),
		CopywritingRpc:  copywriting.NewCopywriting(zrpc.MustNewClient(c.CopywritingRpc)),
		Video:           model.NewAfvVideoCreationModel(conn),
		Shared:          shared,
		GenOpts:         model.NewGenOptionModel(conn),
		Aigc:            aigc.New(c.Aigc).WithRecorder(taskcenter.NewSubmissionRecorder(conn)),
		MediaSvc:        newMediaSvc(c.VideoRemake.MediaSvc),
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
			Table: "afv_local_life_creation", Prefix: "img", Trace: "imagegen", Kind: kind, EntryCode: code(k.AssetEntryCode, kind), Type: t,
			Status: "CASE status WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE 3 END",
			Result: "JSON_UNQUOTE(JSON_EXTRACT(image_urls, '$[0]'))", Fail: "fail_reason", Prompt: "content",
			BillingRequest: "s.`request_id`",
			RequestPairs: "'prompt', content, 'kind', kind, 'size', size, 'model', model, 'duration', duration, 'imageCount', image_count, " +
				"'posterType', poster_type, 'posterStyle', poster_style, 'templateAssetId', template_asset_id, 'templateUrl', template_url, " +
				"'materials', material_urls, 'inputImageUrl', input_image_url, 'inputVideoUrl', input_video_url, 'inputBgUrl', input_bg_url, " +
				"'copywritingId', copywriting_id",
			// 第一个执行面任务（多张图的其余任务在 tasks 明细里）
			ExecTaskID: "JSON_UNQUOTE(JSON_EXTRACT(tasks, '$[0].task_id'))",
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
			Table: "afv_video_creation", Prefix: "vid", Trace: "imagegen", Kind: kind, EntryCode: code(entry, kind), Type: typ(t, "video"),
			// 剧本阶段（构思中 / 待确认）不算任务，开始出片才进任务中心
			Include: "status IN (2,3) OR (status = 4 AND gen_task_id IS NOT NULL)",
			Status:  "CASE status WHEN 2 THEN 1 WHEN 3 THEN 2 ELSE 3 END",
			Result:  "video_url", Cover: "COALESCE(char_url, cover_url, video_url)", Fail: "fail_reason",
			Prompt:         "COALESCE(user_text, title)",
			BillingRequest: "s.`gen_task_id`",
			RequestPairs: "'prompt', COALESCE(user_text, title), 'kind', kind, 'sourceUrl', source_url, 'sourceVideoUrl', source_video_url, " +
				"'title', title, 'userText', user_text, 'sellingPoints', selling_points, 'shopProfileId', shop_profile_id, " +
				"'copywritingId', copywriting_id, 'charUrl', char_url, 'env1Url', env1_url, 'env2Url', env2_url, 'propUrl', prop_url, " +
				"'duration', duration, 'size', size, 'model', model",
			ExecTaskID: "COALESCE(gen_task_id, plan_task_id)",
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

// newMediaSvc 媒体服务客户端：配了注册中心就订阅实例，否则用静态地址；都没配返回 nil（复刻不出封面）。
func newMediaSvc(c config.MediaSvcConf) *mediasvc.Client {
	fallback := append([]string{c.BaseURL}, c.BaseURLs...)
	if strings.TrimSpace(c.NacosServerAddr) != "" {
		return mediasvc.Discover(mediasvc.DiscoveryConfig{
			ServerAddr:  c.NacosServerAddr,
			Namespace:   c.NacosNamespace,
			Group:       c.NacosGroup,
			ServiceName: c.NacosServiceName,
			Username:    c.NacosUsername,
			Password:    c.NacosPassword,
		}, c.Token, fallback)
	}
	return mediasvc.New(fallback, c.Token)
}
