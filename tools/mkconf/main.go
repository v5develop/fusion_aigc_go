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
//     mkconf -src deploy/etc -out /opt/aigc-life/etc -logs /opt/aigc-life/logs \
//     -dsn 'user:pass@tcp(127.0.0.1:3306)/fusion_aigc?...' -aigc http://127.0.0.1:15055 \
//     -ports copywriting=19201,digitalhuman=19202,note=19203,imagegen=19204,gateway=18888,admin=18889
package main

import (
	"bytes"
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
	flag.Parse()
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
		e := editor{name: name, ports: portOf, dsn: *dsn, aigc: *aigc, logDir: filepath.Join(*logs, name), keepDays: *keepDays}
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
	name     string
	ports    map[string]int
	dsn      string
	aigc     string
	logDir   string
	keepDays int
}

func (e editor) apply(root *yaml.Node) error {
	set(root, "Mode", scalar("pro"))
	set(root, "Log", mapping(
		"Mode", scalar("file"), "Path", scalar(e.logDir), "KeepDays", scalar(strconv.Itoa(e.keepDays)),
		"Encoding", scalar("plain"), "Rotation", scalar("daily"),
	))
	port := strconv.Itoa(e.ports[e.name])
	if get(root, "ListenOn") != nil { // rpc 服务端：只在本机监听，不注册 etcd
		set(root, "ListenOn", scalar("127.0.0.1:"+port))
		del(root, "Etcd")
	}
	if get(root, "Port") != nil { // api 网关：对外
		set(root, "Host", scalar("0.0.0.0"))
		set(root, "Port", scalar(port))
	}
	if get(root, "DataSource") != nil {
		set(root, "DataSource", scalar(e.dsn))
	}
	if a := get(root, "Aigc"); a != nil {
		set(a, "Endpoint", scalar(e.aigc))
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

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

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
