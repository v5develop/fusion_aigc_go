package svc

import (
	"context"
	"time"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/config"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/llm"
	"aigc-local-life/pkg/moderation"
	"aigc-local-life/pkg/semaphore"
	"aigc-local-life/pkg/taskcenter"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type ServiceContext struct {
	Config      config.Config
	Copywriting model.AfvCopywritingModel
	Option      model.AfvCopywritingOptionModel
	ShopProfile model.AfvShopProfileModel
	Shared      *model.SharedModel
	LLM         *llm.Client
	Moderation  moderation.Checker
	// Sem 按模型限制并发（afv_ai_model.max_concurrency）
	Sem *semaphore.Group
	// Billing 文案计费（预扣 / 确认 / 退回）；Mode=off 时全部是空操作
	Billing *billing.Service
	// TaskCenter 文案同步进任务中心（afv_creation_task），由后台定时任务驱动
	TaskCenter *taskcenter.Syncer
}

func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	option := model.NewAfvCopywritingOptionModel(conn)
	return &ServiceContext{
		Config:      c,
		Copywriting: model.NewAfvCopywritingModel(conn),
		Option:      option,
		ShopProfile: model.NewAfvShopProfileModel(conn),
		Shared:      model.NewSharedModel(conn),
		LLM:         llm.New(),
		Moderation:  moderation.New(c.Moderation.Mode, blockWords(option)),
		Sem:         semaphore.NewGroup(),
		Billing:     billing.New(c.Billing, conn),
		TaskCenter:  taskcenter.New(conn, 10*time.Minute, copywritingSource(c.BillingEntryCode)),
	}
}

// copywritingSource 文案在任务中心的同步源。文案是文本产物：没有媒体地址。
// 入口码与计费 offer 同源（BillingEntryCode），type 固定为 copywriting；
// 计费请求号与 generate_logic.go 的 billingRequestID 一致（copywriting-<id>），预留单可以关联回来。
func copywritingSource(entry string) taskcenter.Source {
	return taskcenter.Source{
		Table: "afv_copywriting", Prefix: "cw", EntryCode: entry, Type: "copywriting",
		// 生成中=1 处理中，已完成=2 成功，失败=3
		Status:         "CASE status WHEN 0 THEN 1 WHEN 1 THEN 2 ELSE 3 END",
		Fail:           "fail_reason",
		Prompt:         "COALESCE(NULLIF(topic, ''), title)",
		BillingRequest: "CONCAT('copywriting-', s.id)",
		ResultText:     "s.`body`",
		RequestPairs: "'prompt', COALESCE(NULLIF(topic, ''), title), 'title', title, 'contentType', content_type, " +
			"'style', style, 'industry', industry, 'targetWords', target_words, 'wordCount', word_count, " +
			"'source', source, 'shopProfileId', shop_profile_id",
	}
}

// blockWords 敏感词来自选项表的 BLOCKWORD 类型，code 即词本身。
func blockWords(option model.AfvCopywritingOptionModel) moderation.WordSource {
	return func(ctx context.Context) ([]string, error) {
		list, err := option.ListByType(ctx, model.OptionBlockWord, true)
		if err != nil {
			return nil, err
		}
		words := make([]string, 0, len(list))
		for _, o := range list {
			words = append(words, o.Code)
		}
		return words, nil
	}
}
