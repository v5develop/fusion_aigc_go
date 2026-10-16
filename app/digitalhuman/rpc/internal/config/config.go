package config

import (
	"aigc-life/pkg/aigc"
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
	// Aigc AIGC 执行面（数字人视频 85、AI 定制出图 75）。Endpoint 为空时这两个功能返回「暂未开放」。
	Aigc aigc.Conf
	// Storage 照片与配音文件存储：local 仅开发用；oss 写现有公有桶（配置从 afv_system_config 读）
	Storage storage.Conf
	// TaskIDPrefix 提交执行面的 task_id 前缀（执行面按它路由回调；我们用轮询，前缀只用于排查）。不能含下划线。
	TaskIDPrefix string `json:",default=AiCreationLife"`
	// PublicModel 公共数字人在现有模板素材投放位（afv_public_template_asset_placement）里的位置
	PublicModel struct {
		BizType string `json:",default=localLife"`
		Module  string `json:",default=digitalHuman"`
		Slot    string `json:",default=publicModel"`
	}
	// Asset 成品写进素材库（afv_asset）。entry_code 同时用于素材库分类归属与计费 Offer。
	Asset struct {
		Enabled  bool `json:",default=false"`
		Portrait struct {
			Type      string `json:",default=customizedModel"`
			EntryCode string `json:",default=digitalHumanPortrait"`
		}
		Video struct {
			Type      string `json:",default=video"`
			EntryCode string `json:",default=digitalHuman"`
		}
	}
	Agreement struct {
		// AigcVersion AIGC 功能使用约定的当前版本；条款改版后 +1，用户需重新同意
		AigcVersion int `json:",default=1"`
	}
	Limits struct {
		MaxNameLen        int `json:",default=20"`
		MaxScriptLen      int `json:",default=50"`
		MaxDescriptionLen int `json:",default=200"`
	}
	Portrait struct {
		Count          int    `json:",default=4"`
		Size           string `json:",default=9:16"`
		Model          string `json:",default=max"`
		TimeoutMinutes int    `json:",default=10"`
	}
	Video struct {
		AspectRatio    string `json:",default=9:16"`
		Resolution     string `json:",default=720p"`
		TimeoutMinutes int    `json:",default=30"`
	}
	TTS struct {
		// TimeoutSeconds 模型未配置 generation_timeout_seconds 时使用
		TimeoutSeconds int `json:",default=60"`
		// PreviewText 后台「生成试听」合成的固定文案
		PreviewText string `json:",default=你好，欢迎光临本店，今天给大家推荐我们的招牌好物。"`
	}
	Poll struct {
		IntervalSeconds int `json:",default=5"`
	}
	// Moderation 内容审核：keyword-关键词过滤（与 AI文案共用 BLOCKWORD 词库）、off-不审核
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
	}
}
