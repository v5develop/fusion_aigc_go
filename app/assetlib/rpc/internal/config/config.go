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
	// Media 共享媒体服务：读时把 afv_asset 里的资源引用 mr://{rid}（Java 写入的封面等）现签成可访问地址。
	// 与 studio / canvas 同结构：配了 NacosServerAddr 订阅实例，BaseURL 只作回退；都留空则 mr:// 一律输出为空。
	Media struct {
		NacosServerAddr  string   `json:",optional"`
		NacosNamespace   string   `json:",optional"`
		NacosGroup       string   `json:",default=DEFAULT_GROUP"`
		NacosServiceName string   `json:",default=aigc-media"`
		NacosUsername    string   `json:",optional"`
		NacosPassword    string   `json:",optional"`
		BaseURL          string   `json:",optional"`
		BaseURLs         []string `json:",optional"`
		Token            string   `json:",optional"`
	}
	// FreeWatermark 免费档图片的读时参数水印：Java 把免费用户的图片交付写成水印视图 mrw://{rid}.{ext}（原件在私有桶，
	// 不存副本），读时必须以「签名覆盖 x-oss-process 水印参数」的形态下发。Process 填与 Java 同一个处理串
	// （Java 启动日志 check=free_watermark.process 打印的值，或 FREE_WATERMARK_OSS_PROCESS）；留空则 mrw:// 一律输出为空
	// （失败关闭：绝不签出干净原件）。只允许 [A-Za-z0-9_./,-]，与媒体服务校验一致。
	FreeWatermark struct {
		Process string `json:",optional"`
	} `json:",optional"`
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
