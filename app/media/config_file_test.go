package media

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestDeployConfigLoads：部署用的 media-svc.yaml 必须能按代码里的 Config 加载（字段名与结构对得上），
// 并且 OSS 段的环境变量能展开。
func TestDeployConfigLoads(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "docker", "media", "media-svc.yaml")
	env := map[string]string{
		"MEDIA_LISTEN":                  "127.0.0.1:18090",
		"MEDIA_SERVICE_TOKEN":           "tok",
		"MEDIA_INSTANCE_ID":             "tst001",
		"MEDIA_IDENTITY_SECRET":         testSecret,
		"MEDIA_NACOS_SERVER_ADDR":       "",
		"MEDIA_NACOS_NAMESPACE":         "",
		"MEDIA_NACOS_USERNAME":          "admin",
		"MEDIA_NACOS_PASSWORD":          "pw",
		"MEDIA_ADVERTISE_URL":           "http://10.0.0.5:18090",
		"MEDIA_LOGO_PATH":               "",
		"MEDIA_OSS_ENABLED":             "true",
		"MEDIA_OSS_PRESIGN_MINUTES":     "30",
		"MEDIA_OPS_TOKEN":               "",
		"MEDIA_INGEST_HEADER_HOSTS":     "",
		"MEDIA_MAX_INPUT_BYTES":         "1073741824",
		"MEDIA_STORE_LOCAL_MAX_BYTES":   "2147483648",
		"MEDIA_FETCH_TIMEOUT_SECONDS":   "1800",
		"MEDIA_OP_TIMEOUT_SECONDS":      "1800",
		"MEDIA_OSS_PRIVATE_ENDPOINT":    "https://oss.example.com",
		"MEDIA_OSS_PRIVATE_BUCKET":      "priv",
		"MEDIA_OSS_PRIVATE_ACCESS_KEY":  "AK1",
		"MEDIA_OSS_PRIVATE_SECRET_KEY":  "SK1",
		"MEDIA_OSS_ASSET_ENDPOINT":      "https://oss.example.com",
		"MEDIA_OSS_ASSET_BUCKET":        "asset",
		"MEDIA_OSS_ASSET_ACCESS_KEY":    "AK2",
		"MEDIA_OSS_ASSET_SECRET_KEY":    "SK2",
		"MEDIA_OSS_ASSET_CUSTOM_DOMAIN": "cdn.example.com",
		"MEDIA_OSS_THIRD_ENDPOINT":      "https://oss.example.com",
		"MEDIA_OSS_THIRD_BUCKET":        "third",
		"MEDIA_OSS_THIRD_ACCESS_KEY":    "AK3",
		"MEDIA_OSS_THIRD_SECRET_KEY":    "SK3",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	var cfg Config
	if err := conf.Load(path, &cfg, conf.UseEnv()); err != nil {
		t.Fatalf("加载部署配置失败: %v", err)
	}
	if !cfg.OSS.Enabled || cfg.OSS.Private.Bucket != "priv" || cfg.OSS.AssetLibrary.CustomDomain != "cdn.example.com" || cfg.OSS.ThirdParty.Bucket != "third" {
		t.Fatalf("OSS 段没有按环境变量展开: %+v", cfg.OSS)
	}
	if cfg.IdempotencyMinutes != 120 {
		t.Fatalf("IdempotencyMinutes = %d", cfg.IdempotencyMinutes)
	}
	if cfg.MaxInputBytes != 1073741824 || cfg.StoreLocalMaxBytes != 2<<30 {
		t.Fatalf("输入上限 %d / 产物上限 %d 没有按环境变量展开", cfg.MaxInputBytes, cfg.StoreLocalMaxBytes)
	}
	if _, err := NewOSSStore(cfg.OSS); err != nil {
		t.Fatalf("部署配置生成的 OSS 配置无效: %v", err)
	}
}
