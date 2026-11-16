// mkconf 由开发配置（deploy/etc/*.yaml）生成服务器配置，install.sh 调用。
//
// 只改和环境有关的几项，其余（业务参数、注释）原样保留，开发配置改了服务器配置自动跟上：
//
//   - 数据库 DataSource、执行面 Aigc.Endpoint、存储 Storage.Mode=oss
//
//   - 端口：rpc 的 ListenOn、api 的 Port
//
//   - 不用 etcd：rpc 服务端去掉 Etcd，客户端的 Etcd 换成直连 Endpoints（单机部署够用，少装一个组件）
//
//   - 日志写文件（按天切分，保留 KeepDays 天），Mode=pro
//
//     mkconf -src deploy/etc -out /opt/aigc-local-life/etc -logs /opt/aigc-local-life/logs \
//     -dsn 'user:pass@tcp(127.0.0.1:3306)/fusion_aigc?...' -aigc http://127.0.0.1:15055 \
//     -ports copywriting=19201,digitalhuman=19202,note=19203,imagegen=19204,studio=19205,assetlib=19206,canvas=19207,gateway=18888,admin=18889
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// 配置文件 → 端口表里的名字
var services = map[string]string{
	"copywriting-rpc.yaml":  "copywriting",
	"digitalhuman-rpc.yaml": "digitalhuman",
	"note-rpc.yaml":         "note",
	"imagegen-rpc.yaml":     "imagegen",
	"studio-rpc.yaml":       "studio",
	"assetlib-rpc.yaml":     "assetlib",
	"canvas-rpc.yaml":       "canvas",
	"gateway-api.yaml":      "gateway",
	"admin-api.yaml":        "admin",
}

func main() {
	src := flag.String("src", "deploy/etc", "开发配置目录")
	out := flag.String("out", "", "输出目录")
	logs := flag.String("logs", "", "日志目录")
	dsn := flag.String("dsn", "", "MySQL DSN")
	aigc := flag.String("aigc", "", "AIGC 执行面地址（联调期为 mock-aigc）")
	ports := flag.String("ports", "", "端口表，如 copywriting=19201,…,gateway=18888,admin=18889")
	keepDays := flag.Int("keep-days", 7, "日志保留天数")
	jwtSecret := flag.String("jwt-secret", "", "App 令牌签名密钥（只写进网关配置）")
	adminJwtSecret := flag.String("admin-jwt-secret", "", "后台令牌签名密钥（只写进后台配置，与 App 分开）")
	authMode := flag.String("auth-mode", "", "网关鉴权模式：dev / token")
	union := flag.String("union", "", "联盟用户服务地址（登录用）")
	linkParse := flag.String("link-parse", "", "分享链接解析服务地址（爆款视频复刻的链接入口）")
	sessionRedis := flag.String("session-redis", "", "AIGC(Java) 服务的 Redis：配了就发和它同一套的令牌")
	sessionRedisPass := flag.String("session-redis-pass", "", "该 Redis 的密码")
	sessionRedisDB := flag.Int("session-redis-db", 10, "该 Redis 的库号，必须和 AIGC 服务一致（它是 10）")
	fusionURL := flag.String("fusion-url", "", "现有 Java 服务地址（建档交给它；留空则我们自己写表）")
	fusionKey := flag.String("fusion-key", "", "Java 服务分配的 appKey")
	fusionSecret := flag.String("fusion-secret", "", "Java 服务分配的 appSecret")
	fusionKeyVer := flag.String("fusion-key-version", "", "密钥版本，留空则不带")
	taskIDPrefix := flag.String("task-id-prefix", "AiCreationLife", "执行面 task_id 前缀：只能字母数字，不含下划线（执行面按 split('_')[0] 路由）")
	listenHost := flag.String("listen-host", "127.0.0.1", "网关与后台监听地址：只绑回环，公网入口走 nginx")
	accessHours := flag.Int("token-access-hours", 2, "App 令牌有效小时，必须与 Java 的 TOKEN_ACCESS_HOURS 一致")
	refreshDays := flag.Int("token-refresh-days", 7, "刷新令牌天数，必须与 Java 的 TOKEN_REFRESH_DAYS 一致")
	sessionMax := flag.Int("session-max", 0, "每用户并发会话上限，0 = 不限，必须与 Java 的 max-concurrent-sessions 一致")
	logConsole := flag.Bool("log-console", false, "日志输出到标准输出（容器部署用，由 Docker 负责轮转）；默认写文件")
	swagger := flag.Bool("swagger", false, "网关与后台是否开放 /swagger 接口文档（正式环境必须关）")
	billingMode := flag.String("billing-mode", "", "计费档位 off / shadow / on（写进笔记、数字人、图片视频三个服务的 Billing.Mode），留空保持开发配置")
	mediaToken := flag.String("media-token", "", "共享媒体服务鉴权 token（写进 imagegen 的 VideoRemake.MediaSvc.Token，须与 media-svc / Java 的 MEDIA_SERVICE_TOKEN 一致）")
	mediaNacosAddr := flag.String("media-nacos-addr", "", "媒体服务注册中心（r-nacos）地址 host:port；imagegen 用它订阅实例，留空保留模板默认")
	mediaNacosUser := flag.String("media-nacos-user", "", "r-nacos 鉴权用户名（开启 OpenAPI 鉴权时）")
	mediaNacosPass := flag.String("media-nacos-pass", "", "r-nacos 鉴权密码")
	mediaBaseURL := flag.String("media-base-url", "", "媒体服务静态回退地址（imagegen 的 VideoRemake.MediaSvc.BaseURL、studio 的 Media.BaseURL）")
	aiGatewayURL := flag.String("ai-gateway-url", "", "ai-gateway 地址（studio 生图）；留空 = studio 提交返回「暂未开放」")
	aiGatewayKeyID := flag.String("ai-gateway-key-id", "", "ai-gateway 调用方 key_id（如 fusion-go-v1），留空保留开发配置")
	aiGatewaySecret := flag.String("ai-gateway-secret", "", "ai-gateway 调用方 HMAC 密钥（只写进 studio 配置）")
	reverseURL := flag.String("reverse-url", "", "反推 sidecar 地址（studio 的 Reverse.BaseURL），留空保留开发配置的 127.0.0.1:8090")
	colorfixURL := flag.String("colorfix-solver-url", "", "自动校色求解服务地址（studio 的 ColorFixSolver.Endpoint）；留空 = 功能关闭")
	colorfixToken := flag.String("colorfix-solver-token", "", "求解服务 Bearer token（须与求解服务 COLORFIX_TOKEN 一致，只写进 studio 配置）")
	studioInputPrefixes := flag.String("studio-input-url-prefixes", "", "studio / assetlib 额外允许的输入图地址前缀（逗号分隔，如素材库公有桶域名 https://cdn.example.com/），写进 InputURLPrefixes；留空不改")
	freeWatermarkEnabled := flag.String("free-watermark-enabled", "", "studio 免费档读时参数水印开关 true / false（= Java FREE_WATERMARK_ENABLED），写进 studio FreeWatermark.Enabled；留空不改")
	freeWatermarkProcess := flag.String("free-watermark-process", "", "免费水印 OSS 处理串（= Java FREE_WATERMARK_OSS_PROCESS / 启动日志 check=free_watermark.process），写进 studio / assetlib / canvas 的 FreeWatermark.Process；留空不改")
	replicateEnabled := flag.String("replicate-enabled", "", "视频做同款总开关默认值 true / false（写进网关 Replicate.Enabled；afv_system_config.replicate.enabled 存在时以库为准），留空保持模板（关）")
	plazaIPSalt := flag.String("plaza-ip-salt", "", "广场 IP 加盐哈希的盐（写进网关 Plaza.IPHashSalt）；留空 = 由 -dsn 派生一串稳定的私密盐（含库口令，不公开、重装不变）")
	canvasJavaURL := flag.String("canvas-java-base-url", "", "Java 主项目内网地址（canvas 调 creaft v1 兼容工具 / 任务查询 / 价目，透传用户 Bearer）；留空 = 放大 / 扩图 / 重绘返回「暂未开放」")
	flag.Parse()
	for _, r := range *taskIDPrefix {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			log.Fatalf("task-id-prefix 只能是字母数字（不含下划线）: %q", *taskIDPrefix)
		}
	}
	if *taskIDPrefix == "" {
		log.Fatal("task-id-prefix 不能为空")
	}
	if *out == "" || *logs == "" || *dsn == "" || *aigc == "" || *ports == "" {
		flag.Usage()
		os.Exit(2)
	}
	portOf, err := parsePorts(*ports)
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range services {
		if portOf[name] == 0 {
			log.Fatalf("端口表缺少 %s", name)
		}
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	for file, name := range services {
		raw, err := os.ReadFile(filepath.Join(*src, file))
		if err != nil {
			log.Fatal(err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			log.Fatalf("%s: %v", file, err)
		}
		root := doc.Content[0]
		e := editor{name: name, ports: portOf, dsn: *dsn, aigc: *aigc, logDir: filepath.Join(*logs, name), keepDays: *keepDays, logConsole: *logConsole,
			jwtSecret: *jwtSecret, adminJwtSecret: *adminJwtSecret, authMode: *authMode, union: *union,
			linkParse:    *linkParse,
			sessionRedis: *sessionRedis, sessionRedisPass: *sessionRedisPass,
			sessionRedisDB: *sessionRedisDB,
			fusionURL:      *fusionURL, fusionKey: *fusionKey, fusionSecret: *fusionSecret, fusionKeyVer: *fusionKeyVer, billingMode: *billingMode, swagger: *swagger, taskIDPrefix: *taskIDPrefix,
			listenHost: *listenHost, accessHours: *accessHours, refreshDays: *refreshDays, sessionMax: *sessionMax,
			mediaToken: *mediaToken, mediaNacosAddr: *mediaNacosAddr, mediaNacosUser: *mediaNacosUser, mediaNacosPass: *mediaNacosPass, mediaBaseURL: *mediaBaseURL,
			aiGatewayURL: *aiGatewayURL, aiGatewayKeyID: *aiGatewayKeyID, aiGatewaySecret: *aiGatewaySecret, reverseURL: *reverseURL, colorfixURL: strings.TrimSpace(*colorfixURL), colorfixToken: *colorfixToken,
			studioInputPrefixes: splitList(*studioInputPrefixes), canvasJavaURL: *canvasJavaURL, plazaIPSalt: plazaSalt(*plazaIPSalt, *dsn),
			replicateEnabled:     strings.ToLower(strings.TrimSpace(*replicateEnabled)),
			freeWatermarkEnabled: strings.TrimSpace(*freeWatermarkEnabled), freeWatermarkProcess: strings.TrimSpace(*freeWatermarkProcess)}
		if err := e.apply(root); err != nil {
			log.Fatalf("%s: %v", file, err)
		}
		var buf bytes.Buffer
		buf.WriteString("# 由 install.sh 根据 deploy.env 生成，重新安装会覆盖；要改环境相关的值请改 deploy.env\n")
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, file), buf.Bytes(), 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  生成 %s\n", filepath.Join(*out, file))
	}
}

func parsePorts(s string) (map[string]int, error) {
	m := map[string]int{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		n, err := strconv.Atoi(v)
		if !ok || err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("端口表格式不对：%q", kv)
		}
		m[k] = n
	}
	return m, nil
}

type editor struct {
	jwtSecret        string
	adminJwtSecret   string
	authMode         string
	union            string
	sessionRedis     string
	sessionRedisPass string
	sessionRedisDB   int
	linkParse        string
	fusionURL        string
	fusionKey        string
	fusionSecret     string
	fusionKeyVer     string
	billingMode      string
	swagger          bool
	listenHost       string
	accessHours      int
	refreshDays      int
	sessionMax       int
	taskIDPrefix     string
	name             string
	ports            map[string]int
	dsn              string
	aigc             string
	logDir           string
	keepDays         int
	logConsole       bool
	mediaToken       string
	mediaNacosAddr   string
	mediaNacosUser   string
	mediaNacosPass   string
	mediaBaseURL     string
	aiGatewayURL     string
	aiGatewayKeyID   string
	aiGatewaySecret  string
	reverseURL       string
	// colorfixURL / colorfixToken studio 的 ColorFixSolver（空 = 关闭，清空模板里的值）
	colorfixURL   string
	colorfixToken string
	// studioInputPrefixes studio InputURLPrefixes（空 = 不改模板）
	studioInputPrefixes []string
	// canvasJavaURL canvas 的 Java.BaseURL（空 = 清空，Java 工具返回「暂未开放」）
	canvasJavaURL string
	// freeWatermarkEnabled / freeWatermarkProcess 免费档读时参数水印（空 = 不改模板）
	freeWatermarkEnabled string
	freeWatermarkProcess string
	// replicateEnabled 网关 Replicate.Enabled（true / false / 1 / 0；空 = 不改模板）
	replicateEnabled string
	// plazaIPSalt 网关 Plaza.IPHashSalt（Mode=pro 下空盐 / 默认盐网关拒绝启动）
	plazaIPSalt string
}

// plazaSalt 显式给了就用；否则由 DSN（含库口令）派生：稳定（重建容器、升级不变，IP 哈希的限频 / 去重不断档）且不公开，运维不必多录一个密钥。
func plazaSalt(explicit, dsn string) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	sum := sha256.Sum256([]byte("plaza-ip-hash-salt|" + dsn))
	return hex.EncodeToString(sum[:])
}

// splitList 逗号 / 空白分隔的列表，去空项。
func splitList(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (e editor) apply(root *yaml.Node) error {
	set(root, "Mode", scalar("pro"))
	if e.name == "note" || e.name == "digitalhuman" || e.name == "imagegen" { // 执行面 task_id 前缀
		set(root, "TaskIDPrefix", scalar(e.taskIDPrefix))
	}
	if e.name == "gateway" { // 视频做同款：总开关默认值 + 与 studio / canvas 同一组 ai-gateway、媒体服务凭据、同一计费档位
		if err := e.applyReplicate(get(root, "Replicate")); err != nil {
			return err
		}
	}
	if e.name == "admin" && e.billingMode != "" { // 视频做同款后台计费档位必须与网关 Replicate.Billing 同一个，否则人工退点 / 强制收尾回 BILLING_OFF
		if b := get(get(root, "ReplicateAdmin"), "Billing"); b != nil {
			set(b, "Mode", quoted(e.billingMode))
		}
	}
	if e.name == "gateway" { // 广场 IP 加盐哈希：正式模式下空盐 / 默认盐网关拒绝启动（app/plaza/core.CheckIPHashSalt）
		p := get(root, "Plaza")
		if p == nil {
			p = mapping()
			set(root, "Plaza", p)
		}
		set(p, "IPHashSalt", quoted(e.plazaIPSalt))
	}
	if e.name == "gateway" || e.name == "admin" { // 接口文档默认关；只有显式 -swagger 才开
		set(root, "Swagger", scalar(strconv.FormatBool(e.swagger)))
		set(root, "Host", scalar(e.listenHost)) // 只绑回环，公网只经 nginx
	}
	if e.logConsole { // 容器：写标准输出，JSON 便于日志采集，轮转交给 Docker 的日志驱动
		set(root, "Log", mapping("Mode", scalar("console"), "Encoding", scalar("json")))
	} else {
		set(root, "Log", mapping(
			"Mode", scalar("file"), "Path", scalar(e.logDir), "KeepDays", scalar(strconv.Itoa(e.keepDays)),
			"Encoding", scalar("plain"), "Rotation", scalar("daily"),
		))
	}
	port := strconv.Itoa(e.ports[e.name])
	if get(root, "ListenOn") != nil { // rpc 服务端：只在本机监听，不注册 etcd
		set(root, "ListenOn", scalar("127.0.0.1:"+port))
		del(root, "Etcd")
	}
	if get(root, "Port") != nil { // api 网关与后台：只绑本机，公网经 nginx（-listen-host 可改）
		set(root, "Host", scalar(e.listenHost))
		set(root, "Port", scalar(port))
	}
	if get(root, "DataSource") != nil {
		set(root, "DataSource", scalar(e.dsn))
	}
	if a := get(root, "Aigc"); a != nil {
		set(a, "Endpoint", scalar(e.aigc))
	}
	if a := get(root, "Auth"); a != nil { // 网关与后台的鉴权
		if e.authMode != "" {
			set(a, "Mode", scalar(e.authMode))
		}
		// 后台和 App 各用一份密钥，互相拿不过去
		secret := e.jwtSecret
		if e.name == "admin" {
			secret = e.adminJwtSecret
		}
		// 没给密钥就清空：正式环境不用 JWT（令牌走共用会话）。绝不能留开发配置里的占位符，
		// 空密钥时 token 包会拒绝签发与解析（ErrNoSecret），不会出现空密钥伪造。
		if get(a, "Jwt") != nil {
			set(get(a, "Jwt"), "Secret", quoted(secret))
		}
		// 和 AIGC 服务共用的会话：只对网关有意义（后台用自己的 JWT）
		if e.name == "gateway" || e.name == "admin" { // 后台与 App 共用同一份会话（Java 的后台令牌也在这里）
			if e.sessionRedis != "" {
				sn := mapping("RedisAddr", quoted(e.sessionRedis))
				if e.sessionRedisPass != "" {
					set(sn, "RedisPass", quoted(e.sessionRedisPass))
				}
				set(sn, "RedisDB", scalar(strconv.Itoa(e.sessionRedisDB)))
				set(sn, "AccessHours", scalar(strconv.Itoa(e.accessHours)))
				set(sn, "RefreshDays", scalar(strconv.Itoa(e.refreshDays)))
				set(sn, "MaxSessions", scalar(strconv.Itoa(e.sessionMax)))
				set(a, "Session", sn)
			} else {
				del(a, "Session")
			}
		}
	}
	// 下面两个地址：本地模板指向 mock，服务器上**没配就得清空**，
	// 否则会留着 127.0.0.1 的 mock 地址，表现成「接口在但一直失败」，比直接说「未开放」难查得多。
	if u := get(root, "Union"); u != nil {
		set(u, "BaseURL", scalar(e.union))
		// 联盟只走 HTTP（同 Java）：不写 Direct，不连联盟库 / 联盟 Redis
		del(u, "Direct")
	}
	// 建档交给现有 Java 服务；三样齐了才写，缺一样就走我们自己写表那条路
	if get(root, "Union") != nil && e.fusionURL != "" && e.fusionKey != "" && e.fusionSecret != "" {
		f := mapping(
			"BaseURL", scalar(e.fusionURL),
			"AppKey", quoted(e.fusionKey), // appKey 常是纯数字，不加引号会被当成数字
			"AppSecret", quoted(e.fusionSecret),
			"TimeoutSeconds", scalar("10"),
		)
		if e.fusionKeyVer != "" {
			set(f, "KeyVersion", quoted(e.fusionKeyVer))
		}
		set(root, "Fusion", f)
	}
	// 爆款视频复刻的链接入口：配了才开放，没配清空（同上）
	if vr := get(root, "VideoRemake"); vr != nil {
		if lp := get(vr, "LinkParse"); lp != nil {
			set(lp, "BaseURL", scalar(e.linkParse))
		}
		// 共享媒体服务（imagegen 复刻抽封面等）：token 必须与 media-svc / Java 一致。
		// mkconf 原先不碰这段，模板里的 Token: change-me 会原样进生产 → Go 调媒体全部 401。
		if ms := get(vr, "MediaSvc"); ms != nil {
			e.applyMedia(ms)
		}
	}
	// studio：共享媒体服务（顶层 Media 段）、ai-gateway、反推 sidecar。TaskIDPrefix 是按 kind 的映射（tyo/pse/simg），不跟 -task-id-prefix 走
	if e.name == "studio" {
		if ms := get(root, "Media"); ms != nil {
			e.applyMedia(ms)
		}
		if g := get(root, "AiGateway"); g != nil {
			// 地址与密钥始终写入：没配就清空（提交返回「暂未开放」），不留开发配置里的值
			set(g, "BaseURL", scalar(e.aiGatewayURL))
			set(g, "Secret", quoted(e.aiGatewaySecret))
			if e.aiGatewayKeyID != "" {
				set(g, "KeyID", quoted(e.aiGatewayKeyID))
			}
		}
		if r := get(root, "Reverse"); r != nil && e.reverseURL != "" {
			set(r, "BaseURL", scalar(e.reverseURL))
		}
		// 自动校色求解服务：地址与 token 始终写入，没配就清空（功能关闭，不留开发配置里的值）
		if c := get(root, "ColorFixSolver"); c != nil {
			set(c, "Endpoint", quoted(e.colorfixURL))
			set(c, "Token", quoted(e.colorfixToken))
		}
		if len(e.studioInputPrefixes) > 0 {
			seq := &yaml.Node{Kind: yaml.SequenceNode}
			for _, p := range e.studioInputPrefixes {
				seq.Content = append(seq.Content, scalar(p))
			}
			set(root, "InputURLPrefixes", seq)
		}
	}
	// assetlib：版本图片归属白名单，与 studio 共用 -studio-input-url-prefixes
	if e.name == "assetlib" && len(e.studioInputPrefixes) > 0 {
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, p := range e.studioInputPrefixes {
			seq.Content = append(seq.Content, scalar(p))
		}
		set(root, "InputURLPrefixes", seq)
	}
	// 免费档读时参数水印：studio 写开关与处理串，assetlib / canvas 只写处理串（各处与 Java 必须同一个串）
	if e.name == "studio" || e.name == "assetlib" || e.name == "canvas" {
		e.applyFreeWatermark(root)
	}
	// assetlib：共享媒体服务（读时现签 mr://{rid}）
	if e.name == "assetlib" {
		if ms := get(root, "Media"); ms != nil {
			e.applyMedia(ms)
		}
	}
	// canvas：共享媒体服务、ai-gateway（与 studio 同一组调用方凭据）、Java 主项目地址。TaskIDPrefix 固定 cv，不跟 -task-id-prefix 走
	if e.name == "canvas" {
		if ms := get(root, "Media"); ms != nil {
			e.applyMedia(ms)
		}
		if g := get(root, "AiGateway"); g != nil {
			set(g, "BaseURL", scalar(e.aiGatewayURL))
			set(g, "Secret", quoted(e.aiGatewaySecret))
			if e.aiGatewayKeyID != "" {
				set(g, "KeyID", quoted(e.aiGatewayKeyID))
			}
		}
		if j := get(root, "Java"); j != nil {
			set(j, "BaseURL", scalar(e.canvasJavaURL)) // 没配就清空，不留开发配置
		}
	}
	// 计费档位：只有接了计费的三个服务有这段
	if b := get(root, "Billing"); b != nil && e.billingMode != "" {
		switch e.billingMode {
		case "off", "shadow", "on":
			set(b, "Mode", quoted(e.billingMode)) // 必须带引号：YAML 里裸的 on / off 会被当成布尔值,配置加载直接报 type mismatch
		default:
			return fmt.Errorf("-billing-mode 只能是 off / shadow / on，收到 %q", e.billingMode)
		}
	}
	if s := get(root, "Storage"); s != nil {
		set(s, "Mode", scalar("oss")) // 线上必须 oss：执行面要能访问、前端要直传
	}
	// rpc 客户端：Etcd{Key: xxx.rpc} → Endpoints[127.0.0.1:端口]
	for i := 1; i < len(root.Content); i += 2 {
		v := root.Content[i]
		etcd := get(v, "Etcd")
		if v.Kind != yaml.MappingNode || etcd == nil {
			continue
		}
		key := get(etcd, "Key")
		if key == nil {
			return fmt.Errorf("%s.Etcd 缺少 Key", root.Content[i-1].Value)
		}
		target := strings.TrimSuffix(key.Value, ".rpc")
		p := e.ports[target]
		if p == 0 {
			return fmt.Errorf("%s 指向的 %s 没有端口", root.Content[i-1].Value, key.Value)
		}
		del(v, "Etcd")
		set(v, "Endpoints", &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle,
			Content: []*yaml.Node{scalar(fmt.Sprintf("127.0.0.1:%d", p))}})
	}
	return nil
}

// applyReplicate 网关的 Replicate 段。Enabled 只是默认值：afv_system_config.replicate.enabled 存在时以库为准（数据库 > 文件配置）。
// ai-gateway / 媒体服务 / 计费始终跟着部署变量写，不留开发配置里的值（与 studio / canvas 同一组）。
func (e editor) applyReplicate(rp *yaml.Node) error {
	if rp == nil {
		return nil
	}
	switch e.replicateEnabled {
	case "":
	case "true", "1", "on", "yes":
		set(rp, "Enabled", scalar("true"))
	case "false", "0", "off", "no":
		set(rp, "Enabled", scalar("false"))
	default:
		return fmt.Errorf("-replicate-enabled 只能是 true / false / 1 / 0，收到 %q", e.replicateEnabled)
	}
	if ms := get(rp, "Media"); ms != nil {
		e.applyMedia(ms)
	}
	if g := get(rp, "AiGateway"); g != nil {
		set(g, "BaseURL", scalar(e.aiGatewayURL))
		set(g, "Secret", quoted(e.aiGatewaySecret))
		if e.aiGatewayKeyID != "" {
			set(g, "KeyID", quoted(e.aiGatewayKeyID))
		}
	}
	if b := get(rp, "Billing"); b != nil && e.billingMode != "" {
		switch e.billingMode {
		case "off", "shadow", "on":
			set(b, "Mode", quoted(e.billingMode))
		default:
			return fmt.Errorf("-billing-mode 只能是 off / shadow / on，收到 %q", e.billingMode)
		}
	}
	if len(e.studioInputPrefixes) > 0 { // 素材库公有桶等自有域名：参考图从资产库选来时要认
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, p := range e.studioInputPrefixes {
			seq.Content = append(seq.Content, scalar(p))
		}
		set(rp, "InputURLPrefixes", seq)
	}
	return nil
}

// applyMedia 共享媒体服务段：token 必须与 media-svc / Java 一致。
// 模板里的 Token: change-me 不能原样进生产（否则 Go 调媒体全部 401），所以 Token 始终写入（未配置则为空，宁可显式失败）。
func (e editor) applyMedia(ms *yaml.Node) {
	set(ms, "Token", quoted(e.mediaToken))
	if e.mediaBaseURL != "" {
		set(ms, "BaseURL", scalar(e.mediaBaseURL))
	}
	if e.mediaNacosAddr != "" {
		set(ms, "NacosServerAddr", scalar(e.mediaNacosAddr))
	}
	if e.mediaNacosUser != "" {
		set(ms, "NacosUsername", quoted(e.mediaNacosUser))
	}
	if e.mediaNacosPass != "" {
		set(ms, "NacosPassword", quoted(e.mediaNacosPass))
	}
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

// quoted 强制带引号：纯数字的 appKey 不加引号会被解析成数字，服务起不来
func quoted(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}
}

func mapping(kv ...any) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i < len(kv); i += 2 {
		n.Content = append(n.Content, scalar(kv[i].(string)), kv[i+1].(*yaml.Node))
	}
	return n
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// set 覆盖已有键的值（保留键上的注释），没有则追加。
func set(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v.LineComment = ""
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, scalar(key), v)
}

func del(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// applyFreeWatermark 写 FreeWatermark 段（没有就建）。开关只对 studio 有意义。
func (e editor) applyFreeWatermark(root *yaml.Node) {
	if e.freeWatermarkEnabled == "" && e.freeWatermarkProcess == "" {
		return
	}
	fw := get(root, "FreeWatermark")
	if fw == nil {
		fw = mapping()
		set(root, "FreeWatermark", fw)
	}
	if e.name == "studio" && e.freeWatermarkEnabled != "" {
		set(fw, "Enabled", scalar(strconv.FormatBool(strings.EqualFold(e.freeWatermarkEnabled, "true"))))
	}
	if e.freeWatermarkProcess != "" {
		set(fw, "Process", quoted(e.freeWatermarkProcess))
	}
}
