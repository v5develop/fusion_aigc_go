package config

import (
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"
	"aigc-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库
	DataSource string
	// Aigc AIGC 执行面（海报 78、长图 76）。Endpoint 为空时提交返回「暂未开放」。
	Aigc aigc.Conf
	// Storage 须与网关一致：素材只接受本存储签发的上传地址
	Storage storage.Conf
	// Asset 成品写进素材库；Type/EntryCode 按 kind 取下面 Poster / LongImage 的配置
	Asset  asset.Conf
	Poster KindConf
	// LongImage 长图：模板选填，比例固定，Model 留空不传（对接文档里 76 没有 model 参数）
	LongImage    KindConf
	TaskIDPrefix string `json:",default=aigclife"`
	Limits       struct {
		MaxContentLen int `json:",default=500"`
		MaxMaterials  int `json:",default=3"`
		MaxImages     int `json:",default=4"`
		// MaxImageBytes 直传上来的素材大小上限（提交时 HEAD 校验）
		MaxImageBytes int64 `json:",default=10485760"`
	}
	Poll struct {
		IntervalSeconds int `json:",default=5"`
		TimeoutMinutes  int `json:",default=10"`
	}
	Moderation struct {
		Mode string `json:",default=keyword,options=keyword|off"`
	}
}

// KindConf 一种成品（海报 / 长图）的生成与归档参数。
type KindConf struct {
	// Module / Slot 模板在模板素材投放位里的位置（biz_type 见 BizType）
	BizType string `json:",default=localLife"`
	Module  string
	Slot    string `json:",default=template"`
	// Model 出图模型档位；留空则不传，用执行面默认
	Model string `json:",optional"`
	// DefaultSize 未选比例时使用
	DefaultSize string
	// TemplateRequired 模板是否必选
	TemplateRequired bool `json:",default=false"`
	// AssetType / AssetEntryCode 写进素材库时的取值
	AssetType      string `json:",optional"`
	AssetEntryCode string `json:",optional"`
}
