package core

import (
	"fmt"
	"strings"
)

// Config 广场模板域配置（挂在 gateway-api / admin-api 配置的 Plaza 下）。
type Config struct {
	// Industry 行业功能开关：关闭时 /plaza/industries 返回 gateEnabled=false 且 industries=[]，
	// 新 Web 显示「行业模板整理中」，不提供任何全量兜底（用户侧没有跨行业视角）。归类完再开。
	Industry struct {
		Enabled bool `json:",default=false"`
	}
	// AdminWriteEnabled 后台对 afv_compat_plaza_template 的写权属开关（交接前 Java 仍是写者）。
	// 为 false 时后台模板写接口返回 ADMIN_WRITE_DISABLED；行业 / 渠道 / 示例素材 / 建议等 Go 独写表不受影响。
	// 与 Java 的 app.plaza.legacy-admin-write-enabled 永不同时为 true（先关 Java，再开这里）。
	AdminWriteEnabled bool `json:",default=false"`
	// V1Compat creaft v1 兼容三接口（/api/v1/plaza-templates/list、/tags、/api/v1/plaza-headlines）的 Go 复刻
	// （app/plaza/v1compat）。默认 false = 不注册，nginx 仍指向 Java；开启只是让网关能接，切流靠 nginx，
	// 步骤见 docs/api/api-plaza.md「v1 兼容三接口」。只在 gateway-api 生效。
	V1Compat struct {
		Enabled bool `json:",default=false"`
	}
	// UsageDisplayMin 复刻次数低于它时不下发具体数字（usageVisible=false）。
	UsageDisplayMin int `json:",default=20"`
	// FreshDays 创建不超过这么多天算「新上架」。
	FreshDays int `json:",default=14"`
	Media     struct {
		// ThumbRule 公有桶缩略图规则（如 ?x-oss-process=image/resize,w_640/format,webp/quality,q_80）；
		// 缺失或非 OSS 地址退回原图。
		ThumbRule string `json:",optional"`
	}
	// Upload 后台模板素材上传（presign / confirm）；只有 admin-api 用。
	Upload UploadConfig `json:",optional"`
	// Overlay 模板文字叠加（出图后把用户填的文字叠到成品图上）；媒体服务地址沿用 Upload.Mediasvc。只有 gateway-api 用。
	Overlay OverlayConfig `json:",optional"`
	Quote   struct {
		// JavaBaseURL Java 内网地址（不经 nginx）。空 = 报价不可用（返回 QUOTE_UNAVAILABLE，不编造价格）。
		JavaBaseURL string `json:",optional"`
		// ProbeUserID 报价探针系统用户（无余额、无权益、无档位优惠），匿名报价以它的身份调 Java。
		ProbeUserID int64 `json:",optional"`
		// EtaEntryCodes 出图入口码（afv_task_latency.entry_code），用来算预计耗时；空 = 不给 etaSeconds。
		EtaEntryCodes []string `json:",optional"`
		// AnonPerMinute / AnonPerDay 匿名报价按 IP 哈希的配额；GlobalPerMinute 全站兜底。<=0 不限。
		AnonPerMinute   int `json:",default=30"`
		AnonPerDay      int `json:",default=600"`
		GlobalPerMinute int `json:",default=600"`
	}
	// IPHashSalt IP 加盐哈希的盐（行业建议限频 / 匿名报价配额 / 搜索缺口去重用；不可逆）。
	// 生产（Mode=pro）必须改成自己的随机值：默认盐是公开的，IP 空间又小，用它哈希等于没哈希。
	IPHashSalt string `json:",default=plaza-ip-salt"` // 与 DefaultIPHashSalt 保持一致
}

// DefaultIPHashSalt 仓库里写死的默认盐（公开值）。
const DefaultIPHashSalt = "plaza-ip-salt"

// CheckIPHashSalt 生产模式（go-zero Mode=pro，与 note / studio 判断 dev/test 的方式同一个字段）下不允许沿用默认盐或空盐。
// 返回非 nil 时调用方应拒绝启动。
func (c *Config) CheckIPHashSalt(mode string) error {
	if mode != "pro" {
		return nil
	}
	if salt := strings.TrimSpace(c.IPHashSalt); salt == "" || salt == DefaultIPHashSalt {
		return fmt.Errorf("plaza: 拒绝启动 判据=Mode=pro 且 Plaza.IPHashSalt 为空或仍是默认值 %q（公开盐等于不哈希），请配置随机盐", DefaultIPHashSalt)
	}
	return nil
}

func (c *Config) usageMin() int {
	if c.UsageDisplayMin <= 0 {
		return 20
	}
	return c.UsageDisplayMin
}

func (c *Config) freshDays() int {
	if c.FreshDays <= 0 {
		return 14
	}
	return c.FreshDays
}
