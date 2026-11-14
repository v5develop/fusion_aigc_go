package logic

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// agentCaller 直接调 core.AgentCall（模拟网关 Caller，按来源带 auth_source）。
type agentCaller struct {
	t   *testing.T
	c   *core
	uid int64
}

func (a agentCaller) call(src, tool string, args map[string]any, callID string) (map[string]any, bool) {
	a.t.Helper()
	b, _ := json.Marshal(args)
	in := &canvas.AgentCallReq{UserId: a.uid, Tool: tool, Arguments: string(b), CallId: callID, Client: "function", AuthSource: src}
	if src == mcptoken.AuthSourceMcpToken {
		in.Client, in.TokenScope = "mcp", mcptoken.ScopeRWGenerate
	}
	if src == mcptoken.AuthSourceSvc {
		in.Client, in.Agent = "svc", "svc:test"
	}
	out, err := a.c.AgentCall(context.Background(), in)
	if err != nil {
		a.t.Fatalf("%s %s: %v", src, tool, err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out.Result), &m); err != nil {
		a.t.Fatalf("%s %s 结果不是 JSON: %s", src, tool, out.Result)
	}
	return m, out.IsError
}

// build 建画布 + 节点 + 连线，返回 canvas_id 与 ref → node_id。
func (a agentCaller) build(nodes []map[string]any, edges []map[string]any) (string, map[string]string) {
	a.t.Helper()
	doc, err := a.c.CreateCanvas(context.Background(), &canvas.CreateCanvasReq{UserId: a.uid, Title: "agent tool"})
	if err != nil {
		a.t.Fatal(err)
	}
	out, isErr := a.call(mcptoken.AuthSourceUser, agenttool.ToolAddNodes, map[string]any{"canvas_id": doc.Id, "nodes": nodes}, "")
	if isErr {
		a.t.Fatalf("加节点失败: %v", out)
	}
	refs := map[string]string{}
	for _, x := range out["nodes"].([]any) {
		m := x.(map[string]any)
		refs[m["ref"].(string)] = m["node_id"].(string)
	}
	if len(edges) > 0 {
		for _, e := range edges {
			e["source"], e["target"] = refs[e["source"].(string)], refs[e["target"].(string)]
		}
		if out, isErr := a.call(mcptoken.AuthSourceUser, agenttool.ToolConnect, map[string]any{"canvas_id": doc.Id, "edges": edges}, ""); isErr {
			a.t.Fatalf("连线失败: %v", out)
		}
	}
	return doc.Id, refs
}

// 工具节点（Java 计费：高清放大）的来源规则：mcp_token 专用错误、svc 无缓存令牌 LOGIN_REQUIRED、svc 有缓存令牌按原链路提交给 Java（带该用户 Bearer）。
func TestAgentRunJavaToolNodeSources(t *testing.T) {
	c := e2eCore(t)
	var (
		mu    sync.Mutex
		auths []string
		paths []string
		body  []byte
	)
	java, _ := fakeServer(t, func(w http.ResponseWriter, r *http.Request, b []byte) {
		if strings.HasSuffix(r.URL.Path, javatool.QuoteSuffix) { // 旧版 Java：没有报价接口（回落 price_known=false）
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		auths, paths, body = append(auths, r.Header.Get("Authorization")), append(paths, r.URL.Path), b
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"taskId":"AiCreation_T1"}}`))
	})
	c.s.Java = javatool.New(java.URL, 5*time.Second)
	a := agentCaller{t: t, c: c, uid: 9301}
	cid, refs := a.build([]map[string]any{
		{"ref": "img", "type": "image", "image_url": "https://img.example.com/a.png"},
		{"ref": "up", "type": "tool", "tool": "upscale"},
	}, []map[string]any{{"source": "img", "target": "up", "target_handle": "image"}})
	args := map[string]any{"canvas_id": cid, "node_id": refs["up"]}

	// MCP 令牌：Java 工具一律专用错误，不组请求、不碰 Java
	if out, isErr := a.call(mcptoken.AuthSourceMcpToken, agenttool.ToolRunNode, args, "mcp-1"); !isErr || out["error_code"] != agenttool.ErrMcpTokenJavaToolUnsupported {
		t.Fatalf("mcp_token 执行 Java 工具应回 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED: %v", out)
	}
	if q, isErr := a.call(mcptoken.AuthSourceMcpToken, agenttool.ToolQuoteNode, args, ""); isErr || q["available"] != false ||
		q["reason_code"] != agenttool.ErrMcpTokenJavaToolUnsupported || q["billing"] != "java" {
		t.Fatalf("mcp_token 报价应标不可执行: %v", q)
	}
	// 服务间入口、缓存里没有该用户的登录令牌：明确的「去网页登录」
	if out, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, args, "svc-1"); !isErr || out["error_code"] != agenttool.ErrLoginRequired ||
		out["message"] != msgLoginRequired {
		t.Fatalf("svc 无登录令牌应回 LOGIN_REQUIRED: %v", out)
	}
	if q, _ := a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, ""); q["available"] != false || q["reason_code"] != agenttool.ErrLoginRequired ||
		q["requires_login_token"] != true || q["login_available"] != false {
		t.Fatalf("svc 无登录令牌报价应标需要登录: %v", q)
	}
	mu.Lock()
	if len(paths) != 0 {
		t.Fatalf("被拒的调用不应到 Java: %v", paths)
	}
	mu.Unlock()

	// 服务间入口、该用户网页登录留下的令牌在缓存里：报价给出 java_request，执行按原链路提交给 Java（Java 计费）
	c.s.Bearers.Put(a.uid, "tok-9301")
	q, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, "")
	jr, _ := q["java_request"].(map[string]any)
	if isErr || q["available"] != true || q["billing"] != "java" || q["price_known"] != false || q["login_available"] != true ||
		jr == nil || jr["route"] != "/tools/ai-image-upscaler" {
		t.Fatalf("svc 有令牌报价: %v", q)
	}
	if b, _ := jr["body"].(map[string]any); b["imageUrl"] != "https://img.example.com/a.png" || b["resolution"] != "2k" {
		t.Fatalf("java_request.body 应与提交同形: %v", jr)
	}
	out, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, args, "svc-2")
	if isErr || out["request_id"] == "" {
		t.Fatalf("svc 有令牌应正常执行: %v", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != javatool.PathUpscale || auths[0] != "Bearer tok-9301" {
		t.Fatalf("应带该用户的 Bearer 提交 Java 放大: paths=%v auths=%v", paths, auths)
	}
	var sent map[string]any
	_ = json.Unmarshal(body, &sent)
	if sent["imageUrl"] != "https://img.example.com/a.png" || sent["resolution"] != jr["body"].(map[string]any)["resolution"] {
		t.Fatalf("提交体应与报价给出的 java_request 一致: %s vs %v", body, jr)
	}
	// 显式参数覆盖：size=4k → resolution 4k（不改节点）
	if q, _ := a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, map[string]any{"canvas_id": cid, "node_id": refs["up"], "size": "4k"}, ""); q["java_request"].(map[string]any)["body"].(map[string]any)["resolution"] != "4k" {
		t.Fatalf("size 覆盖应落到放大分辨率: %v", q)
	}
}

// 报价 = 实际冻结（gateway 计费：图片 / 视频 / 反推工具）。计费读本地 fusion_aigc 的真实价目（shadow：只算价、不动账），
// gateway / 媒体服务一律 503（run 留在排队，不会真的出图）。没有 fusion_aigc 或没配画布价目时跳过。
func TestAgentQuoteMatchesFreeze(t *testing.T) {
	c := e2eCore(t)
	dsn := os.Getenv("CANVAS_BILLING_DSN")
	if dsn == "" {
		dsn = "root:123456@tcp(127.0.0.1:43306)/fusion_aigc?parseTime=true&loc=Asia%2FShanghai&charset=utf8mb4&timeout=2s"
	}
	conn := sqlx.NewMysql(dsn)
	var n int
	if err := conn.QueryRowCtx(context.Background(), &n, "SELECT COUNT(*) FROM afv_billing_offer WHERE entry_code IN ('canvas_image','canvas_video','canvas_tool_i2p') AND status='ENABLED' AND deleted=0"); err != nil || n < 3 {
		t.Skipf("本地 fusion_aigc 没有画布价目（%d）: %v", n, err)
	}
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeShadow}, conn)
	unavailable := func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"UNAVAILABLE","message":"down"}}`))
	}
	gw, _ := fakeServer(t, unavailable)
	md, _ := fakeServer(t, unavailable)
	c.s.Gateway = aigateway.New(aigateway.Config{BaseURL: gw.URL, Secret: strings.Repeat("s", 32), Timeout: 2 * time.Second})
	c.s.Media = mediasvc.New([]string{md.URL}, "tok")
	c.s.Config.Poll.MaxSubmitAttempts = 5

	a := agentCaller{t: t, c: c, uid: 9302}
	cid, refs := a.build([]map[string]any{
		{"ref": "txt", "type": "text", "text": "一只橘猫在草地上晒太阳"},
		{"ref": "pic", "type": "image_model", "count": 2},
		{"ref": "vid", "type": "video_model"},
		{"ref": "src", "type": "image", "image_url": "https://img.example.com/b.png"},
		{"ref": "i2p", "type": "tool", "tool": "i2p"},
	}, []map[string]any{
		{"source": "txt", "target": "pic", "target_handle": "prompt"},
		{"source": "txt", "target": "vid", "target_handle": "prompt"},
		{"source": "src", "target": "i2p", "target_handle": "image"},
	})
	for _, ref := range []string{"pic", "vid", "i2p"} {
		args := map[string]any{"canvas_id": cid, "node_id": refs[ref]}
		q, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolQuoteNode, args, "")
		if isErr || q["available"] != true || q["price_known"] != true || q["billing"] != "canvas" {
			t.Fatalf("%s 报价: %v", ref, q)
		}
		points, _ := q["points"].(float64)
		if points <= 0 {
			t.Fatalf("%s 报价应大于 0: %v", ref, q)
		}
		out, isErr := a.call(mcptoken.AuthSourceSvc, agenttool.ToolRunNode, args, "run-"+ref)
		if isErr {
			t.Fatalf("%s 执行: %v", ref, out)
		}
		if credits, _ := out["credits"].(float64); credits != points {
			t.Fatalf("%s 报价 %v 点，实际冻结 %v 点（应一致）: quote=%v run=%v", ref, points, credits, q, out)
		}
		t.Logf("%s 报价 = 冻结 = %v 点（basis=%v）", ref, points, q["basis"])
	}
}

// 工具节点 data → params（前端 toolParamsOf 同口径）与 args 覆盖。
func TestBuildAgentToolRequest(t *testing.T) {
	d := mustDoc(t, `{"nodes":[
	 {"id":"i1","type":"image","data":{"kind":"input","image":"https://x/a.png"}},
	 {"id":"m1","type":"tool","data":{"kind":"tool","tool":"mask","maskPrompt":"换成红色","maskBox":{"x":0.1,"y":0.2,"w":0.3,"h":0.4}}},
	 {"id":"o1","type":"tool","data":{"kind":"tool","tool":"outpaint","ratio":"16:9","sourceNodeId":"i1"}},
	 {"id":"u1","type":"tool","data":{"kind":"tool","tool":"upscale","scale":4}}],
	 "edges":[{"id":"e1","source":"i1","target":"m1","targetHandle":"image"}]}`)
	nodes := d.NodeByID()
	req, err := buildAgentToolRequest(d, nodes["m1"], nil, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal([]byte(req.ToolParams), &p)
	if req.Tool != "mask" || p["mask_prompt"] != "换成红色" || len(p["bbox"].([]any)) != 4 || len(req.References) != 1 {
		t.Fatalf("重绘参数: %+v %v", req, p)
	}
	// sourceNodeId 兜底上游图；ratio 覆盖清掉宽高
	req, err = buildAgentToolRequest(d, nodes["o1"], nil, map[string]any{"ratio": "1:1", "params": map[string]any{"width": 100}})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(req.ToolParams), &p)
	if len(req.References) != 1 || req.References[0].NodeId != "i1" || p["ratio"] != "1:1" || p["width"] != nil {
		t.Fatalf("扩图: %+v %v", req.References, p)
	}
	// 没有输入图：明确提示连线
	if _, err := buildAgentToolRequest(d, nodes["u1"], nil, map[string]any{}); err == nil || !strings.Contains(err.Error(), "canvas_connect") {
		t.Fatalf("没有输入图应提示连线: %v", err)
	}
	// mask_rid 只能服务端写
	if _, err := buildAgentToolRequest(d, nodes["m1"], nil, map[string]any{"params": map[string]any{"maskRid": "x"}}); err == nil {
		t.Fatal("params.mask_rid 应被拒")
	}
}
