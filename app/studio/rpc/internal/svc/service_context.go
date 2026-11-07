package svc

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/auditdb"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/taskcenter"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Config config.Config
	// Conn go-zero 连接池。bun 与 sqlc 都复用它的 RawDB()，不另开池（设计方案 §9.5）
	Conn       sqlx.SqlConn
	Billing    *billing.Service
	Asset      *asset.Writer
	Storage    storage.Store
	Moderation moderation.Checker
	TaskCenter *taskcenter.Syncer
	// MediaSvc 共享媒体服务（下载 / conform / 裁图 / 组图 / resources/copy）；未配置为 nil
	MediaSvc *mediasvc.Client
	// DB 业务读写的 bun 句柄（bundb.New：复用 Conn 的连接池，挂 logx QueryHook，慢阈值 500ms）
	DB *bun.DB
	// Repo afv_studio_* 三张表的数据访问（基于 DB）
	Repo *repo.Repo
	// Gateway ai-gateway HMAC 客户端（/v1/calls 提交、?wait= 长轮询、batchGet、cancel）。BaseURL 为空时 Enabled()=false
	Gateway *aigateway.Client
	// Reverse 反推 sidecar 客户端（c.Reverse）：crop-plan / crop-check / analyze-batch / pose source / pose prompt / status。
	Reverse *reverse.Client
	// PreviewSlots 姿态配对预览（免费）全进程的 sidecar 读图名额（Config.PreviewSidecarSlots）：预览先占它再进 Reverse
	// 的共享信号量，最多占用这么多个 sidecar 名额，付费编排不会被预览挤到排队超时
	PreviewSlots chan struct{}
	// Audit 只读审计查询（sqlc，pkg/auditdb）：只给后台成本汇总用，业务读写一律走 Repo
	Audit *auditdb.Queries
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	db, err := bundb.New(conn)
	if err != nil {
		logx.Must(err)
	}
	audit, err := auditdb.NewFromSqlConn(conn)
	if err != nil {
		logx.Must(err)
	}
	return &ServiceContext{
		Reverse:      newReverse(c),
		PreviewSlots: make(chan struct{}, c.PreviewSidecarSlots()),
		Audit:        audit,
		DB:           db,
		Repo:         repo.New(db),
		Gateway:      newGateway(c),
		Config:       c,
		Conn:         conn,
		Billing:      billing.New(c.Billing, conn),
		Asset:        asset.NewWriter(conn),
		Storage:      storage.New(c.Storage, conn),
		Moderation:   moderation.New(c.Moderation.Mode, blockWords(conn)),
		TaskCenter:   taskcenter.New(conn, 10*time.Minute, TaskSources(c)...),
		MediaSvc:     newMediaSvc(c),
	}
}

// Prefix 某类型的子任务号 / 任务中心坐标前缀。
func Prefix(c config.Config, k model.Kind) string {
	switch k {
	case model.KindTryon:
		return c.TaskIDPrefix.Tryon
	case model.KindPose:
		return c.TaskIDPrefix.Pose
	case model.KindImageGen:
		return c.TaskIDPrefix.ImageGen
	}
	return ""
}

// EntryCode 某类型的入口码（计费 offer / 素材库分类 / 任务中心 entry_code）。
func EntryCode(c config.Config, k model.Kind) string {
	switch k {
	case model.KindTryon:
		return c.EntryCodes.Tryon
	case model.KindPose:
		return c.EntryCodes.Pose
	case model.KindImageGen:
		return c.EntryCodes.ImageGen
	}
	return ""
}

// TaskSources 子任务同步进任务中心：一张表、三个 kind，各用自己的前缀与入口码。
//
// taskcenter.Source 一个源只有一个 Prefix（Ref = md5("<Prefix>:<id>")、origin_ref = "<Prefix>:<id>"），
// 不支持按行取前缀，所以按 kind 拆成三个 Source，用 Source.Kind 生成 `kind = '…'` 筛选。
// 时间列是 created_at / updated_at（bun 模型），用 CreateTimeCol / UpdateTimeCol 指定。
func TaskSources(c config.Config) []taskcenter.Source {
	out := make([]taskcenter.Source, 0, len(model.Kinds))
	for _, k := range model.Kinds {
		out = append(out, taskcenter.Source{
			Table: "afv_studio_task", Prefix: Prefix(c, k), Kind: string(k), EntryCode: EntryCode(c, k), Type: "image",
			// 任务号插入后回填，回填前不进任务中心
			Include: "task_id IS NOT NULL",
			// 0 排队 / 1 生成中 → 处理中；2 成功；3 失败；4 已取消
			Status:  "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 WHEN 4 THEN 4 ELSE 1 END",
			Result:  "result_url",
			Results: "IF(result_url IS NULL, NULL, JSON_ARRAY(result_url))",
			Fail:    "error_reason", Prompt: "LEFT(prompt_text, 500)",
			BillingRequest: "s.`billing_request_id`",
			// 入参快照取批次 payload（前端全量字段）+ 本任务坐标
			RequestPairs: "'prompt', LEFT(prompt_text, 500), 'kind', kind, 'batchId', batch_id, " +
				"'modelIndex', model_index, 'shotIndex', shot_index, 'poseIndex', pose_index, 'modelGroup', model_group, " +
				"'payload', (SELECT b.payload FROM afv_studio_batch b WHERE b.id = s.batch_id)",
			ExecTaskID:    "task_id",
			CreateTimeCol: "created_at", UpdateTimeCol: "updated_at",
		})
	}
	return out
}

// newGateway ai-gateway 客户端：etc 里的秒数换成 time.Duration，其余空值由 aigateway.New 补默认。
func newGateway(c config.Config) *aigateway.Client {
	g := c.AiGateway
	return aigateway.New(aigateway.Config{
		BaseURL: g.BaseURL, KeyID: g.KeyID, Secret: g.Secret,
		Timeout:      time.Duration(g.TimeoutSeconds) * time.Second,
		LongPollWait: time.Duration(g.LongPollWait) * time.Second,
	})
}

// newReverse 反推 sidecar 客户端：BaseURL 为空时 Enabled()=false，调用返回 reverse.KindNotConfigured。
func newReverse(c config.Config) *reverse.Client {
	r := c.Reverse
	return reverse.New(reverse.Config{
		BaseURL: r.BaseURL, Timeout: time.Duration(r.Timeout) * time.Second, Concurrency: r.Concurrency,
	})
}

// blockWords 敏感词：与 AI文案共用 afv_copywriting_option 的 BLOCKWORD 词库。
func blockWords(conn sqlx.SqlConn) moderation.WordSource {
	return func(ctx context.Context) ([]string, error) {
		var words []string
		q := "SELECT `code` FROM afv_copywriting_option WHERE `option_type` = 'BLOCKWORD' AND `status` = 1 AND `deleted` = 0"
		return words, conn.QueryRowsCtx(ctx, &words, q)
	}
}

// newMediaSvc 媒体服务客户端：配了注册中心就订阅实例，否则用静态地址；都没配返回 nil。
func newMediaSvc(c config.Config) *mediasvc.Client {
	m := c.Media
	var fallback []string
	for _, u := range append([]string{m.BaseURL}, m.BaseURLs...) {
		if strings.TrimSpace(u) != "" {
			fallback = append(fallback, u)
		}
	}
	if strings.TrimSpace(m.NacosServerAddr) != "" {
		return mediasvc.Discover(mediasvc.DiscoveryConfig{
			ServerAddr: m.NacosServerAddr, Namespace: m.NacosNamespace, Group: m.NacosGroup,
			ServiceName: m.NacosServiceName, Username: m.NacosUsername, Password: m.NacosPassword,
		}, m.Token, fallback)
	}
	if len(fallback) == 0 {
		return nil
	}
	return mediasvc.New(fallback, m.Token)
}
