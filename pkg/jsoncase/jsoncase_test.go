package jsoncase

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/rest/httpx"
)

func run(t *testing.T, h http.HandlerFunc, method, body, ct string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	Middleware(h)(rec, req)
	return rec
}

func jsonHandler(resp string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp))
	}
}

func TestResponseGetsCamelAliasesKeepingSnake(t *testing.T) {
	rec := run(t, jsonHandler(`{"code":0,"data":{"put_url":"u","items":[{"task_id":"t1"}]}}`), "GET", "", "")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	data := got["data"].(map[string]any)
	if data["put_url"] != "u" || data["putUrl"] != "u" {
		t.Errorf("put_url/putUrl not both present: %v", data)
	}
	item := data["items"].([]any)[0].(map[string]any)
	if item["task_id"] != "t1" || item["taskId"] != "t1" {
		t.Errorf("nested alias missing: %v", item)
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Errorf("Content-Length not set")
	}
}

func TestNonJSONPassesThrough(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"put_url\":\"x\"}\n\n")
	}
	rec := run(t, h, "GET", "", "")
	if rec.Body.String() != "data: {\"put_url\":\"x\"}\n\n" {
		t.Errorf("SSE body changed: %q", rec.Body.String())
	}
}

func TestRequestCamelMappedOnlyForKnownNames(t *testing.T) {
	var seen map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}
	// content_type 是白名单内的字段名：camel 请求补上 snake
	run(t, h, "POST", `{"kind":"image","contentType":"image/png","myKey":"keep"}`, "application/json")
	if seen["content_type"] != "image/png" || seen["contentType"] != "image/png" {
		t.Errorf("content_type not added from contentType: %v", seen)
	}
	// 非白名单的自由键不改名
	if _, ok := seen["my_key"]; ok {
		t.Errorf("non-whitelisted key was rewritten: %v", seen)
	}
	if seen["myKey"] != "keep" {
		t.Errorf("free-form key lost: %v", seen)
	}
}

func TestRequestSnakeWinsWhenBothGiven(t *testing.T) {
	var seen map[string]any
	h := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&seen)
	}
	run(t, h, "POST", `{"content_type":"image/jpeg","contentType":"image/png"}`, "application/json")
	if seen["content_type"] != "image/jpeg" {
		t.Errorf("snake value overwritten: %v", seen)
	}
}

func TestToSnakeToCamel(t *testing.T) {
	if toSnake("contentType") != "content_type" || toCamel("content_type") != "contentType" {
		t.Errorf("case conversion wrong: %s / %s", toSnake("contentType"), toCamel("content_type"))
	}
}

// ---------------------------------------------------------------- query

// runQuery 发一个只带 query 的 GET，返回 handler 看到的 RawQuery。
func runQuery(t *testing.T, rawQuery string) string {
	t.Helper()
	var seen string
	h := func(w http.ResponseWriter, r *http.Request) { seen = r.URL.RawQuery }
	req := httptest.NewRequest("GET", "/x?"+rawQuery, nil)
	Middleware(h)(httptest.NewRecorder(), req)
	return seen
}

func TestQueryCamelMappedToSnake(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "pageSize=20&billingStatus=HELD&page=2"))
	if q.Get("page_size") != "20" || q.Get("billing_status") != "HELD" {
		t.Errorf("camel query not mapped: %v", q)
	}
	if q.Get("pageSize") != "20" || q.Get("page") != "2" {
		t.Errorf("original params lost: %v", q)
	}
}

func TestQuerySnakeUntouched(t *testing.T) {
	raw := "page_size=20&billing_status=HELD"
	if got := runQuery(t, raw); got != raw {
		t.Errorf("snake query changed: %q", got)
	}
}

func TestQuerySnakeWinsWhenBothGiven(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "pageSize=50&page_size=20"))
	if got := q["page_size"]; len(got) != 1 || got[0] != "20" {
		t.Errorf("snake value overwritten / duplicated: %v", got)
	}
}

func TestQueryUnknownKeysUntouched(t *testing.T) {
	raw := "myKey=1&keyword=abc&someCamelThing=x"
	if got := runQuery(t, raw); got != raw {
		t.Errorf("unknown keys rewritten: %q", got)
	}
}

func TestQueryRepeatedKeys(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "taskIds=a&taskIds=b&tagIds=1"))
	if got := q["task_ids"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("repeated camel values not all mapped in order: %v", got)
	}
	if got := q["tag_ids"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("tag_ids not mapped: %v", got)
	}
}

func TestQueryEncodingPreserved(t *testing.T) {
	raw := "keyword=%E4%B8%AD%E6%96%87+a&entryCode=a%26b%3Dc%2Fd&userId"
	got := runQuery(t, raw)
	want := raw + "&entry_code=a%26b%3Dc%2Fd&user_id"
	if got != want {
		t.Errorf("encoding not preserved:\n got  %q\n want %q", got, want)
	}
	q, _ := url.ParseQuery(got)
	if q.Get("entry_code") != "a&b=c/d" || q.Get("keyword") != "中文 a" {
		t.Errorf("decoded values wrong: %v", q)
	}
	if _, ok := q["user_id"]; !ok {
		t.Errorf("valueless key not mapped: %v", q)
	}
}

func TestQueryEncodedCamelKeyMapped(t *testing.T) {
	q, _ := url.ParseQuery(runQuery(t, "page%53ize=7"))
	if q.Get("page_size") != "7" {
		t.Errorf("percent-encoded camel key not mapped: %v", q)
	}
}

func TestQueryBindsThroughGoZero(t *testing.T) {
	type listReq struct {
		Page          int64    `form:"page,optional"`
		PageSize      int64    `form:"page_size,optional"`
		BillingStatus string   `form:"billing_status,optional"`
		TaskIds       []string `form:"task_ids,optional"`
	}
	var got listReq
	var perr error
	h := func(w http.ResponseWriter, r *http.Request) { perr = httpx.Parse(r, &got) }
	req := httptest.NewRequest("GET", "/x?page=2&pageSize=20&billingStatus=HELD&taskIds=a&taskIds=b", nil)
	Middleware(h)(httptest.NewRecorder(), req)
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	if got.Page != 2 || got.PageSize != 20 || got.BillingStatus != "HELD" || len(got.TaskIds) != 2 {
		t.Errorf("go-zero binding did not see mapped query: %+v", got)
	}
}

func TestQueryMalformedPairsLeftAlone(t *testing.T) {
	// 键编码非法 / 空段 / 分号：不参与改写也不报错，合法的 camel 参数照常补 snake
	got := runQuery(t, "bad%zzKey=1&&a;b=2&pageSize=3")
	want := "bad%zzKey=1&&a;b=2&pageSize=3&page_size=3"
	if got != want {
		t.Errorf("malformed query handling:\n got  %q\n want %q", got, want)
	}
}

// ---------------------------------------------------------------- 自由格式子树

func captureBody(t *testing.T, body string) (string, map[string]any) {
	t.Helper()
	var raw []byte
	h := func(w http.ResponseWriter, r *http.Request) { raw, _ = io.ReadAll(r.Body) }
	run(t, h, "POST", body, "application/json")
	var seen map[string]any
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatalf("bad json %q: %v", raw, err)
	}
	return string(raw), seen
}

func TestRequestSnapshotSubtreeUntouched(t *testing.T) {
	// 快照节点 data 里的 taskId / runId / requestId 都在白名单形态内，但属于前端私有 JSON，不得补 snake
	snap := `{"nodes":[{"id":"n1","data":{"taskId":"t1","runId":"r1","requestId":"9007199254740993","note":"<b>&</b>"}}],"edges":[]}`
	raw, seen := captureBody(t, `{"workflowId":"w1","ifMatchRevision":"x","snapshot":`+snap+`}`)
	if seen["workflow_id"] != "w1" {
		t.Errorf("typed top-level camel field not mapped: %v", seen)
	}
	if !strings.Contains(raw, `"snapshot":`+snap) {
		t.Errorf("snapshot bytes changed:\n%s", raw)
	}
	for _, k := range []string{"task_id", "run_id", "request_id"} {
		if strings.Contains(raw, `"`+k+`"`) {
			t.Errorf("snapshot polluted with %s: %s", k, raw)
		}
	}
}

func TestRequestCamelFreeFormKeyAlsoSkipped(t *testing.T) {
	// 自由格式键按 snake 形式匹配：posePrompts 的 map 键（用户数据）不改名
	raw, _ := captureBody(t, `{"posePrompts":{"taskId":"x"}}`)
	if strings.Contains(raw, `"task_id"`) {
		t.Errorf("free-form value rewritten: %s", raw)
	}
	if !strings.Contains(raw, `"pose_prompts":{"taskId":"x"}`) {
		t.Errorf("pose_prompts alias missing or value changed: %s", raw)
	}
}

func TestRequestCanvasToolParamsStillMapped(t *testing.T) {
	// 画布工具 params 的键是接口字段：web 试穿发 aspectRatio，canvas rpc 读 aspect_ratio
	_, seen := captureBody(t, `{"kind":"tool","tool":"tryon","params":{"aspectRatio":"9:16","items":["top"]}}`)
	if p := seen["params"].(map[string]any); p["aspect_ratio"] != "9:16" {
		t.Errorf("params.aspectRatio not mapped: %v", p)
	}
}

func TestRequestNestedTypedStillMapped(t *testing.T) {
	// 具名结构体里的嵌套对象 / 数组照常补 snake
	_, seen := captureBody(t, `{"references":[{"nodeId":"n1","assetId":"a1"}],"modelId":"m"}`)
	ref := seen["references"].([]any)[0].(map[string]any)
	if ref["asset_id"] != "a1" || seen["model_id"] != "m" {
		t.Errorf("typed nested camel not mapped: %v", seen)
	}
}

func TestRequestUnchangedBodyKeepsBytes(t *testing.T) {
	body := `{"snapshot":{"nodes":[{"data":{"taskId":"t"}}]},  "name":"a<b"}`
	raw, _ := captureBody(t, body)
	if raw != body {
		t.Errorf("body without typed camel keys was re-encoded:\n got  %s\n want %s", raw, body)
	}
}

func TestResponseFreeFormNotAliased(t *testing.T) {
	resp := `{"code":0,"data":{"canvas_id":"c1","snapshot":{"nodes":[{"data":{"run_id":"r","source_node_id":"s"}}]},` +
		`"java_image_configs":[{"model_code":"m","max_refs":3}],"items":[{"task_id":"t"}]}}`
	rec := run(t, jsonHandler(resp), "GET", "", "")
	body := rec.Body.String()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	data := got["data"].(map[string]any)
	if data["canvasId"] != "c1" || data["javaImageConfigs"] == nil {
		t.Errorf("typed keys not aliased: %v", data)
	}
	if data["items"].([]any)[0].(map[string]any)["taskId"] != "t" {
		t.Errorf("typed nested alias missing: %v", data)
	}
	for _, k := range []string{"runId", "sourceNodeId", "modelCode", "maxRefs"} {
		if strings.Contains(body, `"`+k+`"`) {
			t.Errorf("free-form blob aliased with %s: %s", k, body)
		}
	}
	if !strings.Contains(body, `"snapshot":{"nodes":[{"data":{"run_id":"r","source_node_id":"s"}}]}`) {
		t.Errorf("snapshot bytes changed: %s", body)
	}
}

func TestFreeFormKeysGenerated(t *testing.T) {
	for _, k := range []string{"snapshot", "java_image_configs"} {
		if !freeFormKeys[k] {
			t.Errorf("%s missing from freeFormKeys; rerun scripts/jsoncase/gen_known.py", k)
		}
	}
	for _, k := range []string{"code", "msg", "data", "params"} {
		if freeFormKeys[k] {
			t.Errorf("envelope key %s must not be free-form", k)
		}
	}
}

func TestRequestLargeSnapshotSaveStillMapped(t *testing.T) {
	// 快照 ≤2MB：超过旧 1MB 上限时 ifMatchRevision 不补 snake，保存会被拒「缺少 if_match_revision」
	snap := `{"nodes":[{"data":{"note":"` + strings.Repeat("x", 1800<<10) + `"}}],"edges":[]}`
	raw, seen := captureBody(t, `{"ifMatchRevision":"rev-1","snapshot":`+snap+`}`)
	if seen["if_match_revision"] != "rev-1" {
		t.Errorf("if_match_revision not mapped for large snapshot body")
	}
	if !strings.Contains(raw, `"snapshot":`+snap) {
		t.Errorf("large snapshot bytes changed")
	}
}

// verbatim 前缀白名单：/api/v1/plaza/ 下的响应由 v1wire 手写，不得被补 snake 别名；后台前缀与其它路径行为不变。
func TestVerbatimPrefix(t *testing.T) {
	h := Middleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"put_url":"u","data":[{"task_id":"t"}]}`))
	})
	call := func(path string) string {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", path, nil))
		return rec.Body.String()
	}
	if got := call("/api/v1/plaza/templates"); got != `{"put_url":"u","data":[{"task_id":"t"}]}` {
		t.Fatalf("plaza 用户端响应必须原样: %s", got)
	}
	if got := call("/api/v1/plaza/admin/templates"); !strings.Contains(got, "putUrl") {
		t.Fatalf("plaza 后台沿用双写别名: %s", got)
	}
	if got := call("/api/v1/studio/batches"); !strings.Contains(got, "putUrl") {
		t.Fatalf("其它路径行为不变: %s", got)
	}
	for _, p := range []string{"/api/v1/plaza-templates/list", "/api/v1/plaza", "/api/v1/plazax/a"} {
		if isVerbatimPath(p) {
			t.Errorf("%s 不应被放行", p)
		}
	}
}

func TestSkipHeaderPassesJSONThrough(t *testing.T) {
	body := `{"jsonrpc":"2.0","result":{"inputSchema":{"properties":{"canvas_id":{"type":"string"}}}}}`
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(SkipHeader, "off")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
	rec := run(t, h, "POST", "", "")
	if rec.Body.String() != body {
		t.Errorf("X-Jsoncase: off 的响应被改写: %q", rec.Body.String())
	}
	if rec.Header().Get(SkipHeader) != "" {
		t.Errorf("X-Jsoncase 头不应发给客户端")
	}
}

// 画布智能体入口（MCP 与 Function Call）请求、响应都原样放行：键名是协议本身，补别名会破坏
// additionalProperties:false 的入参校验与 MCP JSON-RPC 结构。其它画布路径行为不变。
func TestCanvasAgentPathsVerbatim(t *testing.T) {
	const reqBody = `{"name":"canvas_get","arguments":{"canvasId":"c1"},"ifMatchRevision":"r"}`
	const respBody = `{"jsonrpc":"2.0","result":{"call_id":"x","is_error":false}}`
	var seenBody, seenQuery string
	h := Middleware(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody, seenQuery = string(b), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	})
	call := func(path string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path+"?pageSize=1", strings.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		h(rec, req)
		return rec.Body.String()
	}
	for _, p := range []string{"/api/v1/canvas/mcp", "/api/v1/canvas/agent/call", "/api/v1/canvas/agent/tools"} {
		if got := call(p); got != respBody {
			t.Errorf("%s 响应被改写: %s", p, got)
		}
		if seenBody != reqBody {
			t.Errorf("%s 请求体被改写: %s", p, seenBody)
		}
		if seenQuery != "pageSize=1" {
			t.Errorf("%s query 被改写: %s", p, seenQuery)
		}
	}
	// 其它画布路径照旧双写
	if got := call("/api/v1/canvas/list"); !strings.Contains(got, "callId") {
		t.Errorf("其它画布路径应补别名: %s", got)
	}
	if !strings.Contains(seenBody, "if_match_revision") {
		t.Errorf("其它画布路径请求应补 snake 别名: %s", seenBody)
	}
	for _, p := range []string{"/api/v1/canvas/mcpx", "/api/v1/canvas/agent", "/api/v1/canvas/agents/x"} {
		if isVerbatimPath(p) {
			t.Errorf("%s 不应被放行", p)
		}
	}
	if !isVerbatimPath("/api/v1/canvas/mcp/") {
		t.Errorf("/api/v1/canvas/mcp/ 应放行")
	}
}
