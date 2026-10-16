package config

import (
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/billing"
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库
	DataSource string
	// Billing 计费：off 不计费（默认）/ shadow 只算价记日志 / on 真扣。由 deploy.env 的 BILLING_MODE 写入
	Billing billing.Conf
	// Aigc AIGC 执行面（构思 74、出图 75）。Endpoint 为空时构思与生成返回「暂未开放」。
	Aigc aigc.Conf
	// Storage 须与网关一致：素材与编辑时新增的图片只接受本存储签发的上传地址
	Storage storage.Conf
	// DigitalHumanRpc 校验表单里选的数字人（须属于本人且已生成完成），取其形象图
	DigitalHumanRpc zrpc.RpcClientConf
	// CopywritingRpc 取店铺档案拼好的文本（档案由 AI文案服务管理）
	CopywritingRpc zrpc.RpcClientConf
	// Asset 成品写进素材库（afv_asset）：每张图一行，用 source_task_id 串起来
	Asset asset.Conf
	// TaskIDPrefix 提交执行面的 task_id 前缀，不能含下划线
	TaskIDPrefix string `json:",default=aigclife"`
	Limits       struct {
		MaxTopicLen   int `json:",default=500"`
		MaxMaterials  int `json:",default=3"` // 上传素材上限
		DefaultImages int `json:",default=4"` // 套图数量默认值（设计稿为 4）；套图数量不设上限，以前端传入为准
		MaxEditImages int `json:",default=6"` // 编辑时图片上限；生成张数更多时以生成张数为准
		MaxTitleLen   int `json:",default=100"`
		MaxContentLen int `json:",default=3000"`
		MaxTags       int `json:",default=10"`
	}
	Plan struct {
		TimeoutMinutes int `json:",default=5"`
	}
	// Generate 出图（75）。Model 本版固定 max；Size 为空时不传，用执行面默认 3:4
	Generate struct {
		Model          string `json:",optional"`
		Size           string `json:",optional"`
		TimeoutMinutes int    `json:",default=20"`
	}
	Poll struct {
		IntervalSeconds int `json:",default=5"`
	}
	// Moderation 内容审核：keyword-关键词过滤（与 AI文案共用 BLOCKWORD 词库）、off-不审核
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
	}
}
