package media

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// validConfig 一份通过 Validate 的配置（只改下面几个字段来构造失败用例）。
func validConfig() Config {
	return Config{Listen: "127.0.0.1:18090", OpTimeoutSeconds: 1800, FetchTimeoutSeconds: 1800}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("合法配置被拒绝: %v", err)
	}
}

// 空 Listen 会让 http.Server 监听 :80（全部网卡），必须拒绝启动。
func TestValidateRejectsEmptyListen(t *testing.T) {
	for _, listen := range []string{"", "   "} {
		c := validConfig()
		c.Listen = listen
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "MEDIA_LISTEN") {
			t.Fatalf("Listen=%q: 期望拒绝并指明 MEDIA_LISTEN，得到 %v", listen, err)
		}
	}
}

// OpTimeoutSeconds 为 0 时每个请求的 context 立刻过期；负数同理，必须拒绝。
func TestValidateRejectsNonPositiveOpTimeout(t *testing.T) {
	for _, v := range []int{0, -1} {
		c := validConfig()
		c.OpTimeoutSeconds = v
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "MEDIA_OP_TIMEOUT_SECONDS") {
			t.Fatalf("OpTimeoutSeconds=%d: 期望拒绝并指明 MEDIA_OP_TIMEOUT_SECONDS，得到 %v", v, err)
		}
	}
}

// FetchTimeoutSeconds 为 0 时 http.Client 不设超时，下载可以无限挂住，必须拒绝。
func TestValidateRejectsNonPositiveFetchTimeout(t *testing.T) {
	for _, v := range []int{0, -1} {
		c := validConfig()
		c.FetchTimeoutSeconds = v
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "MEDIA_FETCH_TIMEOUT_SECONDS") {
			t.Fatalf("FetchTimeoutSeconds=%d: 期望拒绝并指明 MEDIA_FETCH_TIMEOUT_SECONDS，得到 %v", v, err)
		}
	}
}

// 端到端：go-zero 的 conf.Load 会在解析后调用 Validate（core/conf/validate.go），所以部署 yaml 里
// ${MEDIA_LISTEN} 留空时，加载本身就失败，启动即退出，不会带着 :80 继续跑。
func TestLoadRejectsEmptyListenFromDeployYaml(t *testing.T) {
	_, err := loadDeployConfigWithEnv(t, map[string]string{"MEDIA_LISTEN": ""})
	if err == nil || !strings.Contains(err.Error(), "MEDIA_LISTEN") {
		t.Fatalf("MEDIA_LISTEN 留空应在加载时被拒绝并指明变量名，得到 %v", err)
	}
}

// 端到端：MEDIA_OP_TIMEOUT_SECONDS=0 经 yaml 加载时被拒绝。
func TestLoadRejectsZeroOpTimeoutFromDeployYaml(t *testing.T) {
	_, err := loadDeployConfigWithEnv(t, map[string]string{"MEDIA_OP_TIMEOUT_SECONDS": "0"})
	if err == nil || !strings.Contains(err.Error(), "MEDIA_OP_TIMEOUT_SECONDS") {
		t.Fatalf("MEDIA_OP_TIMEOUT_SECONDS=0 应被拒绝，得到 %v", err)
	}
}

// loadDeployConfigWithEnv 用一套可通过加载的环境变量加载部署 yaml，再覆盖 overrides。
func loadDeployConfigWithEnv(t *testing.T, overrides map[string]string) (Config, error) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "docker", "media", "media-svc.yaml")
	env := map[string]string{
		"MEDIA_LISTEN":                  "127.0.0.1:18090",
		"MEDIA_SERVICE_TOKEN":           "tok",
		"MEDIA_INSTANCE_ID":             "tst001",
		"MEDIA_IDENTITY_SECRET":         testSecret,
		"MEDIA_NACOS_SERVER_ADDR":       "",
		"MEDIA_NACOS_NAMESPACE":         "",
		"MEDIA_NACOS_USERNAME":          "",
		"MEDIA_NACOS_PASSWORD":          "",
		"MEDIA_ADVERTISE_URL":           "",
		"MEDIA_LOGO_PATH":               "",
		"MEDIA_OSS_ENABLED":             "false",
		"MEDIA_OSS_PRESIGN_MINUTES":     "60",
		"MEDIA_OPS_TOKEN":               "",
		"MEDIA_INGEST_HEADER_HOSTS":     "",
		"MEDIA_MAX_INPUT_BYTES":         "1073741824",
		"MEDIA_STORE_LOCAL_MAX_BYTES":   "2147483648",
		"MEDIA_FETCH_TIMEOUT_SECONDS":   "1800",
		"MEDIA_OP_TIMEOUT_SECONDS":      "1800",
		"MEDIA_OSS_PRIVATE_ENDPOINT":    "",
		"MEDIA_OSS_PRIVATE_BUCKET":      "",
		"MEDIA_OSS_PRIVATE_ACCESS_KEY":  "",
		"MEDIA_OSS_PRIVATE_SECRET_KEY":  "",
		"MEDIA_OSS_ASSET_ENDPOINT":      "",
		"MEDIA_OSS_ASSET_BUCKET":        "",
		"MEDIA_OSS_ASSET_ACCESS_KEY":    "",
		"MEDIA_OSS_ASSET_SECRET_KEY":    "",
		"MEDIA_OSS_ASSET_CUSTOM_DOMAIN": "",
		"MEDIA_OSS_THIRD_ENDPOINT":      "",
		"MEDIA_OSS_THIRD_BUCKET":        "",
		"MEDIA_OSS_THIRD_ACCESS_KEY":    "",
		"MEDIA_OSS_THIRD_SECRET_KEY":    "",
		"MEDIA_IDEMPOTENCY_STORE":       "memory",
		"MEDIA_REDIS_ADDR":              "",
		"MEDIA_REDIS_PASSWORD":          "",
	}
	for k, v := range overrides {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	var cfg Config
	err := conf.Load(path, &cfg, conf.UseEnv())
	return cfg, err
}
