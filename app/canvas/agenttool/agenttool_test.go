package agenttool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestRegistryWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tl := range Tools() {
		if seen[tl.Name] {
			t.Fatalf("工具重名: %s", tl.Name)
		}
		seen[tl.Name] = true
		if tl.Description == "" || tl.Title == "" {
			t.Errorf("%s 缺说明 / 标题", tl.Name)
		}
		if tl.Input["type"] != "object" || tl.Input["additionalProperties"] != false {
			t.Errorf("%s 入参必须是封闭对象", tl.Name)
		}
		if tl.Output["type"] != "object" {
			t.Errorf("%s 出参必须是对象", tl.Name)
		}
		checkRequired(t, tl.Name, tl.Input)
		if _, err := json.Marshal(tl.Input); err != nil {
			t.Errorf("%s 入参 Schema 不能序列化: %v", tl.Name, err)
		}
	}
	for _, op := range BatchOps {
		if s, ok := BatchArgSchema(op); !ok {
			t.Errorf("batch op %s 没有 args Schema", op)
		} else {
			checkRequired(t, "batch."+op, s)
		}
	}
	// 设计要求的最小工具集
	for _, n := range []string{ToolCreate, ToolGet, ToolAddNodes, ToolUpdateNodes, ToolDeleteNodes, ToolMoveNodes, ToolResizeNodes,
		ToolConnect, ToolDisconnect, ToolAutoLayout, ToolRunNode, ToolGetRun} {
		if !seen[n] {
			t.Errorf("缺少工具 %s", n)
		}
	}
	if tl, _ := Find(ToolRunNode); !tl.Generates || tl.ReadOnly {
		t.Error("canvas_run_node 必须标为生成类写工具")
	}
	if tl, _ := Find(ToolGet); !tl.ReadOnly || tl.Mutates() {
		t.Error("canvas_get 必须只读")
	}
}

// checkRequired required 里的键都在 properties 里（递归）。
func checkRequired(t *testing.T, name string, s Schema) {
	t.Helper()
	props, _ := s["properties"].(Props)
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if _, ok := props[k.(string)]; !ok {
				t.Errorf("%s: required %v 不在 properties 里", name, k)
			}
		}
	}
	for k, v := range props {
		if ps, ok := v.(Schema); ok {
			checkRequired(t, name+"."+k, ps)
			if it, ok := ps["items"].(Schema); ok {
				checkRequired(t, name+"."+k+"[]", it)
			}
		}
	}
}

func TestDesignDocListsEveryTool(t *testing.T) {
	b, err := os.ReadFile("../../../docs/design/canvas-agent-tools.md")
	if err != nil {
		t.Skip("设计文档不在预期位置")
	}
	for _, tl := range Tools() {
		if !strings.Contains(string(b), "`"+tl.Name+"`") {
			t.Errorf("设计文档没有列出工具 %s", tl.Name)
		}
	}
}

func TestRenderFormats(t *testing.T) {
	n := len(Tools())
	oa := Render(FormatOpenAI)
	if len(oa) != n || oa[0]["type"] != "function" {
		t.Fatalf("openai 格式不对: %v", oa[0])
	}
	fn := oa[0]["function"].(map[string]any)
	if fn["name"] != ToolList || fn["parameters"] == nil {
		t.Fatalf("openai function 缺字段: %v", fn)
	}
	an := Render(FormatAnthropic)
	if an[0]["input_schema"] == nil || an[0]["name"] != ToolList {
		t.Fatalf("anthropic 格式不对: %v", an[0])
	}
	mcp := Render(FormatMCP)
	ann := mcp[2]["annotations"].(map[string]any)
	if mcp[2]["name"] != ToolGet || ann["readOnlyHint"] != true || mcp[2]["inputSchema"] == nil || mcp[2]["outputSchema"] == nil {
		t.Fatalf("mcp 格式不对: %v", mcp[2])
	}
	if ParseFormat("xx") != FormatOpenAI || ParseFormat("mcp") != FormatMCP {
		t.Fatal("ParseFormat")
	}
}

func TestParseArgumentsValidation(t *testing.T) {
	add, _ := Find(ToolAddNodes)
	cases := []struct {
		name, args, want string
	}{
		{"缺必填", `{"canvas_id":"cv_0123456789abcdef"}`, "nodes: 缺少必填字段"},
		{"多余字段", `{"canvas_id":"cv_0123456789abcdef","nodes":[{"type":"text"}],"user_id":9}`, "user_id: 不认识的字段"},
		{"枚举", `{"canvas_id":"cv_0123456789abcdef","nodes":[{"type":"robot"}]}`, "nodes[0].type: 取值必须是"},
		{"pattern", `{"canvas_id":"x","nodes":[{"type":"text"}]}`, "canvas_id: 格式不对"},
		{"越界", `{"canvas_id":"cv_0123456789abcdef","nodes":[{"type":"text","width":1}]}`, "nodes[0].width: 不能小于 80"},
		{"整数", `{"canvas_id":"cv_0123456789abcdef","nodes":[{"type":"image_model","count":1.5}]}`, "必须是整数"},
		{"非对象", `[1]`, "arguments 必须是 JSON 对象"},
	}
	for _, c := range cases {
		_, err := ParseArguments(add, json.RawMessage(c.args))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 想要 %q，得到 %v", c.name, c.want, err)
		}
	}
	// 批量上限
	var many []string
	for i := 0; i < MaxAddNodes+1; i++ {
		many = append(many, `{"type":"text"}`)
	}
	if _, err := ParseArguments(add, json.RawMessage(`{"canvas_id":"cv_0123456789abcdef","nodes":[`+strings.Join(many, ",")+`]}`)); err == nil || !strings.Contains(err.Error(), "最多 50 项") {
		t.Errorf("批量上限没生效: %v", err)
	}
	// 驼峰归一 + 字符串形式的 arguments（OpenAI）+ null 视同未传
	m, err := ParseArguments(add, json.RawMessage(`"{\"canvasId\":\"cv_0123456789abcdef\",\"baseRevision\":null,\"nodes\":[{\"type\":\"text\",\"imageUrl\":\"https://x/1.png\"}]}"`))
	if err != nil {
		t.Fatal(err)
	}
	if m["canvas_id"] != "cv_0123456789abcdef" || m["nodes"].([]any)[0].(map[string]any)["image_url"] != "https://x/1.png" {
		t.Fatalf("驼峰没归一: %v", m)
	}
	if _, has := m["base_revision"]; has {
		t.Fatal("null 应视同未传")
	}
	// 自由对象 data 不下钻、不归一
	m, err = ParseArguments(add, json.RawMessage(`{"canvas_id":"cv_0123456789abcdef","nodes":[{"type":"text","data":{"fooBar":1}}]}`))
	if err != nil || m["nodes"].([]any)[0].(map[string]any)["data"].(map[string]any)["fooBar"] == nil {
		t.Fatalf("data 不应被改写: %v %v", m, err)
	}
}

func TestParseFunctionCall(t *testing.T) {
	c, err := ParseFunctionCall([]byte(`{"id":"call_1","type":"function","function":{"name":"canvas_get","arguments":"{\"canvas_id\":\"cv_0123456789abcdef\"}"}}`))
	if err != nil || c.Tool != ToolGet || c.CallID != "call_1" || c.Client != "function" {
		t.Fatalf("openai tool_call: %+v %v", c, err)
	}
	c, err = ParseFunctionCall([]byte(`{"id":"toolu_1","type":"tool_use","name":"canvas_list","input":{"page":1}}`))
	if err != nil || c.Tool != ToolList || c.CallID != "toolu_1" || string(c.Arguments) != `{"page":1}` {
		t.Fatalf("anthropic tool_use: %+v %v", c, err)
	}
	c, err = ParseFunctionCall([]byte(`{"name":"canvas_create","arguments":{},"call_id":"k1"}`))
	if err != nil || c.CallID != "k1" {
		t.Fatalf("通用形状: %+v %v", c, err)
	}
	if _, err := ParseFunctionCall([]byte(`{"arguments":{}}`)); err == nil {
		t.Fatal("缺工具名应报错")
	}
}

// ---------------------------------------------------------------- MCP

type fakeCaller struct{ calls []Call }

func (f *fakeCaller) call(_ context.Context, c Call) (*Result, error) {
	f.calls = append(f.calls, c)
	if c.Tool == ToolDeleteNodes {
		return ErrorResult(ErrorData{ErrorCode: ErrNotFound, Message: "画布不存在"}), nil
	}
	return &Result{Data: json.RawMessage(`{"canvas_id":"cv_0123456789abcdef","revision":"1-a"}`)}, nil
}

func mcpPost(t *testing.T, s *MCPServer, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %s", rec.Body.String())
		}
	}
	return rec, out
}

func TestMCPProtocol(t *testing.T) {
	fc := &fakeCaller{}
	s := &MCPServer{Name: "fusion-canvas", Version: "1", Call: fc.call, Instructions: "hi"}

	rec, out := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
	res := out["result"].(map[string]any)
	if rec.Code != 200 || res["protocolVersion"] != "2025-03-26" || res["capabilities"].(map[string]any)["tools"] == nil || res["instructions"] != "hi" {
		t.Fatalf("initialize: %d %v", rec.Code, out)
	}
	if rec.Header().Get(RawJSONHeader) != "off" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("响应头不对: %v", rec.Header())
	}
	// 不认识的版本协商到最新
	_, out = mcpPost(t, s, `{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if out["result"].(map[string]any)["protocolVersion"] != ProtocolVersions[0] || out["id"] != "a" {
		t.Fatalf("版本协商: %v", out)
	}
	// 通知 → 202 无响应体
	if rec, _ := mcpPost(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("通知应 202: %d %q", rec.Code, rec.Body.String())
	}
	// tools/list
	_, out = mcpPost(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(Tools()) || tools[0].(map[string]any)["inputSchema"] == nil {
		t.Fatalf("tools/list: %v", out)
	}
	// tools/call：幂等键取 _meta，结果有 structuredContent 与 text
	_, out = mcpPost(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"canvas_create","arguments":{"title":"x"},"_meta":{"idempotency_key":"k-1"}}}`,
		map[string]string{"Idempotency-Key": "header-key", "X-Agent-Name": "unit-agent"})
	res = out["result"].(map[string]any)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["revision"] != "1-a" {
		t.Fatalf("tools/call: %v", out)
	}
	if txt := res["content"].([]any)[0].(map[string]any)["text"].(string); !strings.Contains(txt, "cv_0123456789abcdef") {
		t.Fatalf("text 内容: %s", txt)
	}
	last := fc.calls[len(fc.calls)-1]
	if last.CallID != "k-1" || last.Client != "mcp" || last.Agent != "unit-agent" || string(last.Arguments) != `{"title":"x"}` {
		t.Fatalf("透传给执行器的调用不对: %+v", last)
	}
	// 头里的幂等键（没有 _meta 时）
	mcpPost(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"canvas_create","arguments":{}}}`, map[string]string{"Idempotency-Key": "header-key"})
	if fc.calls[len(fc.calls)-1].CallID != "header-key" {
		t.Fatal("Idempotency-Key 头没生效")
	}
	// 业务错误 → isError=true（不是 JSON-RPC error）
	_, out = mcpPost(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"canvas_delete_nodes","arguments":{}}}`, nil)
	if out["error"] != nil || out["result"].(map[string]any)["isError"] != true {
		t.Fatalf("业务错误应在结果里: %v", out)
	}
	// 协议错误
	for body, code := range map[string]float64{
		`{"jsonrpc":"2.0","id":6,"method":"resources/list"}`:                      rpcMethodNotFound,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope"}}`: rpcInvalidParams,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{}}`:              rpcInvalidParams,
		`[{"jsonrpc":"2.0","id":9,"method":"ping"}]`:                              rpcInvalidRequest,
		`{not json`: rpcParseError,
		`{"jsonrpc":"1.0","id":10,"method":"ping"}`: rpcInvalidRequest,
	} {
		_, out = mcpPost(t, s, body, nil)
		if e, ok := out["error"].(map[string]any); !ok || e["code"] != code {
			t.Errorf("%s → 想要 %v，得到 %v", body, code, out)
		}
	}
	// ping
	if _, out = mcpPost(t, s, `{"jsonrpc":"2.0","id":11,"method":"ping"}`, nil); out["result"] == nil {
		t.Fatalf("ping: %v", out)
	}
}

func TestMCPTransportGuards(t *testing.T) {
	s := &MCPServer{Call: (&fakeCaller{}).call, AllowedOrigins: []string{"https://app.example.com"}, MaxBodyBytes: 64}
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405: %d", rec.Code)
	}
	if rec, _ := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("非白名单 Origin 应 403: %d", rec.Code)
	}
	if rec, _ := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://app.example.com"}); rec.Code != 200 {
		t.Fatalf("白名单 Origin 应放行: %d", rec.Code)
	}
	if rec, _ := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2020-01-01"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("不支持的协议版本应 400: %d", rec.Code)
	}
	if rec, _ := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"canvas_get","arguments":{"canvas_id":"cv_0123456789abcdef"}}}`, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应 413: %d", rec.Code)
	}
}

func TestSanitizeAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"svc:java-1":          "svc_java-1",
		"  SVC: svc:java-1  ": "svc_svc:java-1",
		"Svc:x":               "svc_x",
		"claude\r\ndesktop":   "claudedesktop",
		"cursor/1.0":          "cursor/1.0",
		"":                    "",
	} {
		if got := SanitizeAgentName(in); got != want {
			t.Fatalf("%q → %q, want %q", in, got, want)
		}
	}
	if n := len([]rune(SanitizeAgentName(strings.Repeat("长", 100)))); n != MaxAgentNameLen {
		t.Fatalf("应截到 %d: %d", MaxAgentNameLen, n)
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("X-Agent-Name", "svc:prod")
	if got := DefaultAgentName(r); got != "svc_prod" {
		t.Fatal(got)
	}
}
