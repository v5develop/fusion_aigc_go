package svc

import (
	"context"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/config"
	"aigc-life/pkg/llm"
	"aigc-life/pkg/moderation"
	"aigc-life/pkg/semaphore"

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
