package config

import (
	"aigc-local-life/pkg/storage"

	"github.com/zeromicro/go-zero/zrpc"
)

type Config struct {
	zrpc.RpcServerConf
	// DataSource fusion_aigc 库：读 Java 的 afv_asset（只读），读写 afv_asset_ext / version / relation / tag / tag_link 旁表
	DataSource string
	// VideoTypes 额外判为视频的 afv_asset.type。媒体形态与 Java resolveMediaType 同口径：
	// type=video/image 字面量直接判；已启用的计费 offer 按 afv_billing_offer.category 判；
	// offer 查不到的才看这里与「名字含 video」。这里对应 Java creaft 兼容入口里 assetMediaType=VIDEO 的那些（Go 读不到那份配置）。
	VideoTypes []string `json:",optional"`
	// Storage 须与网关一致：版本图片只接受本存储签发的地址（或 InputURLPrefixes 白名单）
	Storage storage.Conf
	// InputURLPrefixes 除本存储签发的地址外，额外允许的图片地址前缀（素材库公有桶域名，如 https://cdn.example.com/）。
	// 与 studio 同口径；oss 模式下素材库公有桶不在 Storage 签发范围时必须配。
	InputURLPrefixes []string `json:",optional"`
	// Limits 服务端校验上限（与前端一致，改了要同步 docs/api/api-asset-library.md）
	Limits struct {
		TitleLen         int `json:",default=100"`  // 标题字数（afv_asset.name 是 varchar(128)）
		TagNameLen       int `json:",default=20"`   // 标签名字数
		TagsPerUser      int `json:",default=200"`  // 每用户标签数
		TagsPerAsset     int `json:",default=10"`   // 每资产标签数
		VersionsPerAsset int `json:",default=100"`  // 每资产版本数
		MetadataLen      int `json:",default=50"`   // metadata 每个字段字数
		PromptLen        int `json:",default=5000"` // 版本提示词字数
		BatchAssets      int `json:",default=100"`  // 批量打标签一次最多资产数
	}
}
