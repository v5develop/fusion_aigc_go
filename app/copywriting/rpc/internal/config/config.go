package config

import (
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库
	DataSource string
	// Generate 生成相关的限制
	Generate GenerateConf
	// Billing 计费：off 不计费 / shadow 只算价 / on 真扣。由 deploy.env 的 BILLING_MODE 经 mkconf 写入
	Billing billing.Conf
	// BillingEntryCode 文案计费的入口码（afv_billing_offer.entry_code）。方案见 deploy/sql/Go_V1.0.13__add_copywriting_billing.sql
	BillingEntryCode string `json:",default=aiCopywriting"`
	// ShopProfile 店铺档案：条数与各字段字数上限（字符数，汉字算 2，与设计稿便签一致）
	ShopProfile ShopProfileConf
	// Moderation 内容审核：keyword-关键词过滤（词库见 afv_copywriting_option 的 BLOCKWORD）、off-不审核
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
		// LogToDb 是否把命中记录写进现有的 afv_moderation_log。
		// 默认关闭：写现有表需数据库负责人确认 provider 取值与 task_id 约定。
		LogToDb bool `json:",default=false"`
	}
}

type GenerateConf struct {
	MaxTopicLen     int `json:",default=500"`
	MaxReferenceLen int `json:",default=5000"`
	DefaultWords    int `json:",default=200"`
	// MinWords / MaxWords 自定义字数的范围：太小模型写不出东西，太大会超时且没人看
	MinWords       int `json:",default=50"`
	MaxWords       int `json:",default=1000"` // 产品 2026-09-24 定 50–1000
	TimeoutSeconds int `json:",default=120"`
}

// ShopProfileConf 店铺档案的限制。字段上限来自设计稿便签。
type ShopProfileConf struct {
	MaxCount          int `json:",default=20"`  // 一个账号最多几条
	MaxName           int `json:",default=20"`  // 店铺名称（10 个汉字）
	MaxOpenDate       int `json:",default=10"`  // 开店时间
	MaxAddress        int `json:",default=60"`  // 店铺地址
	MaxProducts       int `json:",default=180"` // 提供商品/服务
	MaxSellingPoints  int `json:",default=220"` // 核心卖点
	MaxSelfTitle      int `json:",default=15"`  // 如何称呼自己
	MaxTargetAudience int `json:",default=40"`  // 主要目标客群
}
