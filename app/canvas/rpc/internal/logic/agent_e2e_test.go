package logic

// 端到端：模拟智能体经 MCP（Streamable HTTP）操作画布 —— agenttool.MCPServer → core.AgentCall → 真 MySQL。
// 建一个临时库（只建画布相关表 + 本次的审计表），结束删库，不碰共享库里的数据。
// 无 MySQL 时跳过；CANVAS_AGENT_E2E_DSN 可指定（不带库名，如 root:123456@tcp(127.0.0.1:43306)/）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/bundb"

	_ "github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func e2eCore(t *testing.T) *core {
	t.Helper()
	base := os.Getenv("CANVAS_AGENT_E2E_DSN")
	if base == "" {
		base = "root:123456@tcp(127.0.0.1:43306)/"
	}
	opts := "?multiStatements=true&parseTime=true&loc=Asia%2FShanghai&charset=utf8mb4&timeout=2s"
	admin, err := sql.Open("mysql", base+opts)
	if err != nil {
		t.Skipf("无 MySQL：%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("无 MySQL（%s）：%v", base, err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "scratch_canvas_agent_e2e_" + hex.EncodeToString(b)
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Skipf("建临时库失败：%v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = admin.Close()
	})
	db, err := sql.Open("mysql", base+name+opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, f := range []string{"Go_V1.0.25__canvas.sql", "Go_V1.0.29__canvas_copy_job.sql", "Go_V1.0.31__copy_job_source_refs.sql", "Go_V1.0.38__canvas_agent_call.sql", "Go_V1.0.40__canvas_revision.sql"} {
		raw, err := os.ReadFile("../../../../../deploy/sql/" + f)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(string(raw), ";\n") {
			s := strings.TrimSpace(stripSQLComments(stmt))
			up := strings.ToUpper(s)
			if !strings.HasPrefix(up, "CREATE TABLE") && !(strings.HasPrefix(up, "ALTER TABLE") && strings.Contains(s, "afv_canvas")) {
				continue
			}
			if _, err := db.Exec(s); err != nil {
				t.Fatalf("%s 建表失败: %v\n%s", f, err, s)
			}
		}
	}
	conn := sqlx.NewSqlConnFromDB(db)
	bdb, err := bundb.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	return &core{s: &svc.ServiceContext{Config: testConfig(), Conn: conn, DB: bdb, Repo: repo.New(bdb),
		Gateway: aigateway.New(aigateway.Config{}), Java: javatool.New("", 0), Bearers: svc.NewBearerCache(time.Hour)}, now: time.Now}
}

func stripSQLComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

type userKey struct{}

// e2eMCP MCP 服务端：用户取自请求头 X-Test-User（模拟网关 Auth 中间件），执行直接进 core.AgentCall（模拟网关 Caller → gRPC）。
func e2eMCP(c *core) *httptest.Server {
	s := &agenttool.MCPServer{Name: "fusion-canvas", Version: "e2e", Call: func(ctx context.Context, call agenttool.Call) (*agenttool.Result, error) {
		uid, _ := ctx.Value(userKey{}).(int64)
		args := string(call.Arguments)
		if args == "" {
			args = "{}"
		}
		out, err := c.AgentCall(ctx, &canvas.AgentCallReq{UserId: uid, Tool: call.Tool, Arguments: args, CallId: call.CallID, Client: call.Client, Agent: call.Agent, ClientIp: "127.0.0.1"})
		if err != nil {
			return nil, err
		}
		return &agenttool.Result{Data: json.RawMessage(out.Result), IsError: out.IsError, Replayed: out.Replayed}, nil
	}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
		s.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
	}))
}

type mcpClient struct {
	t    *testing.T
	url  string
	user int64
	seq  int
	mu   sync.Mutex
}

// call tools/call，返回 structuredContent 与 isError。
func (m *mcpClient) call(tool string, args any, idem string) (map[string]any, bool) {
	m.t.Helper()
	m.mu.Lock()
	m.seq++
	id := m.seq
	m.mu.Unlock()
	params := map[string]any{"name": tool, "arguments": args}
	if idem != "" {
		params["_meta"] = map[string]any{"idempotency_key": idem}
	}
	res := m.rpc(id, "tools/call", params)
	return res["structuredContent"].(map[string]any), res["isError"].(bool)
}

func (m *mcpClient) rpc(id int, method string, params any) map[string]any {
	m.t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, m.url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	req.Header.Set("X-Test-User", strconv.FormatInt(m.user, 10))
	req.Header.Set("X-Agent-Name", "e2e-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		m.t.Fatalf("%s 响应不是 JSON: %v", method, err)
	}
	if out["error"] != nil {
		m.t.Fatalf("%s JSON-RPC 错误: %v", method, out["error"])
	}
	return out["result"].(map[string]any)
}

func TestAgentE2EOverMCP(t *testing.T) {
	c := e2eCore(t)
	srv := e2eMCP(c)
	defer srv.Close()
	alice := &mcpClient{t: t, url: srv.URL, user: 7001}
	bob := &mcpClient{t: t, url: srv.URL, user: 7002}

	// 0. 握手 + 工具清单
	if v := alice.rpc(1, "initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "e2e", "version": "1"}})["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("initialize: %v", v)
	}
	if n := len(alice.rpc(2, "tools/list", map[string]any{})["tools"].([]any)); n != len(agenttool.Tools()) {
		t.Fatalf("tools/list 数量 %d", n)
	}

	// 1. 创建画布（带幂等键：重试拿到同一张）
	out, isErr := alice.call(agenttool.ToolCreate, map[string]any{"title": "智能体画布"}, "e2e-create")
	if isErr {
		t.Fatalf("create: %v", out)
	}
	cid := out["canvas_id"].(string)
	again, _ := alice.call(agenttool.ToolCreate, map[string]any{"title": "智能体画布"}, "e2e-create")
	if again["canvas_id"] != cid {
		t.Fatalf("同 call_id 重试应回放同一张画布: %v vs %s", again, cid)
	}
	if list, _ := alice.call(agenttool.ToolList, map[string]any{}, ""); list["total"].(float64) != 1 {
		t.Fatalf("应只有一张画布（幂等）: %v", list)
	}

	// 2. 加节点（文字 + 图片模型 + 参考图）
	out, isErr = alice.call(agenttool.ToolAddNodes, map[string]any{"canvas_id": cid, "nodes": []any{
		map[string]any{"ref": "p", "type": "text", "text": "海边日落，胶片质感"},
		map[string]any{"ref": "m", "type": "image_model", "model": "Banana", "ratio": "16:9"},
		map[string]any{"ref": "r", "type": "image", "image_url": "https://cdn.example.com/ref.png"},
	}}, "e2e-add-1")
	if isErr {
		t.Fatalf("add_nodes: %v", out)
	}
	ids := map[string]string{}
	for _, x := range out["nodes"].([]any) {
		n := x.(map[string]any)
		ids[n["ref"].(string)] = n["node_id"].(string)
	}
	rev1 := out["revision"].(string)
	// 幂等回放：同 call_id 同参数不重复加
	replay, _ := alice.call(agenttool.ToolAddNodes, map[string]any{"canvas_id": cid, "nodes": []any{
		map[string]any{"ref": "p", "type": "text", "text": "海边日落，胶片质感"},
		map[string]any{"ref": "m", "type": "image_model", "model": "Banana", "ratio": "16:9"},
		map[string]any{"ref": "r", "type": "image", "image_url": "https://cdn.example.com/ref.png"},
	}}, "e2e-add-1")
	if replay["revision"] != rev1 || replay["node_count"].(float64) != 3 {
		t.Fatalf("幂等回放应返回原结果: %v", replay)
	}
	// 同 call_id 不同参数
	if out, isErr = alice.call(agenttool.ToolAddNodes, map[string]any{"canvas_id": cid, "nodes": []any{map[string]any{"type": "text"}}}, "e2e-add-1"); !isErr || out["error_code"] != agenttool.ErrIdempotencyConflict {
		t.Fatalf("同 key 不同参数应 IDEMPOTENCY_CONFLICT: %v", out)
	}

	// 3. 连线（目标口推断：文字 → prompt，图片 → image）
	out, isErr = alice.call(agenttool.ToolConnect, map[string]any{"canvas_id": cid, "base_revision": rev1, "edges": []any{
		map[string]any{"source": ids["p"], "target": ids["m"]},
		map[string]any{"source": ids["r"], "target": ids["m"]},
	}}, "")
	if isErr {
		t.Fatalf("connect: %v", out)
	}
	rev2 := out["revision"].(string)
	if rev2 == rev1 {
		t.Fatal("连线后 revision 应变化")
	}

	// 4. 读取完整结构
	got, isErr := alice.call(agenttool.ToolGet, map[string]any{"canvas_id": cid}, "")
	if isErr || got["revision"] != rev2 {
		t.Fatalf("get: %v", got)
	}
	nodes, edges := got["nodes"].([]any), got["edges"].([]any)
	if len(nodes) != 3 || len(edges) != 2 {
		t.Fatalf("结构不对: %d 节点 %d 连线", len(nodes), len(edges))
	}
	handles := map[string]string{}
	for _, e := range edges {
		em := e.(map[string]any)
		handles[em["source"].(string)] = em["target_handle"].(string)
	}
	if handles[ids["p"]] != "prompt" || handles[ids["r"]] != "image" {
		t.Fatalf("连线口不对: %v", handles)
	}
	for _, n := range nodes {
		nm := n.(map[string]any)
		if nm["node_id"] == ids["m"] && (nm["type"] != "image_model" || nm["data"].(map[string]any)["modelId"] != "banana-v1") {
			t.Fatalf("模型节点读出不对: %v", nm)
		}
	}
	// 存储的快照是前端能直接载入的 Vue Flow 形状
	cvRow, _ := c.s.Repo.Canvas.GetOwned(context.Background(), 7001, cid)
	if !strings.Contains(string(cvRow.Snapshot), `"targetHandle": "prompt"`) && !strings.Contains(string(cvRow.Snapshot), `"targetHandle":"prompt"`) {
		t.Fatalf("快照里应有 targetHandle: %s", cvRow.Snapshot)
	}

	// 5. 乐观锁：拿旧 revision 改 → CANVAS_CONFLICT 带最新 revision
	out, isErr = alice.call(agenttool.ToolMoveNodes, map[string]any{"canvas_id": cid, "base_revision": rev1, "moves": []any{map[string]any{"node_id": ids["p"], "dx": 10}}}, "")
	if !isErr || out["error_code"] != agenttool.ErrConflict || out["revision"] != rev2 {
		t.Fatalf("旧 revision 应冲突: %v", out)
	}
	// 不带 base_revision 的并发写：语义补丁自动重放，全部成功
	var wg sync.WaitGroup
	errs := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, e := alice.call(agenttool.ToolAddNodes, map[string]any{"canvas_id": cid, "nodes": []any{map[string]any{"type": "text", "text": fmt.Sprint("并发", i)}}}, "")
			if e {
				errs <- fmt.Sprint(o)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("并发写失败: %s", e)
	}
	if got, _ = alice.call(agenttool.ToolGet, map[string]any{"canvas_id": cid, "include_runtime": false}, ""); len(got["nodes"].([]any)) != 6 {
		t.Fatalf("并发 3 次加节点后应有 6 个节点，实际 %d", len(got["nodes"].([]any)))
	}

	// 6. 越权：别人的画布一律 NOT_FOUND（读 / 写 / 生成）
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{agenttool.ToolGet, map[string]any{"canvas_id": cid}},
		{agenttool.ToolAddNodes, map[string]any{"canvas_id": cid, "nodes": []any{map[string]any{"type": "text"}}}},
		{agenttool.ToolRunNode, map[string]any{"canvas_id": cid, "node_id": ids["m"]}},
	} {
		if out, isErr := bob.call(tc.tool, tc.args, ""); !isErr || out["error_code"] != agenttool.ErrNotFound {
			t.Fatalf("%s 越权应 NOT_FOUND: %v", tc.tool, out)
		}
	}

	// 7. 生成：走 RunNode（生成服务未接入 → NOT_READY，不落 run、不冻结点数）
	out, isErr = alice.call(agenttool.ToolRunNode, map[string]any{"canvas_id": cid, "node_id": ids["m"]}, "e2e-run-1")
	if !isErr || out["error_code"] != agenttool.ErrNotReady {
		t.Fatalf("生成服务未接入应 NOT_READY: %v", out)
	}
	if runs, _ := alice.call(agenttool.ToolListRuns, map[string]any{"canvas_id": cid}, ""); len(runs["runs"].([]any)) != 0 {
		t.Fatalf("没受理不应有 run: %v", runs)
	}
	// 失败的调用释放幂等键：同 key 可以重试（仍 NOT_READY，而不是 CALL_IN_PROGRESS）
	if out, _ = alice.call(agenttool.ToolRunNode, map[string]any{"canvas_id": cid, "node_id": ids["m"]}, "e2e-run-1"); out["error_code"] != agenttool.ErrNotReady {
		t.Fatalf("失败后同 key 应可重试: %v", out)
	}

	// 8. 审计：写类调用都有记录，带改前改后 revision 与客户端
	var ok, failed, withRev int
	if err := c.s.DB.NewRaw("SELECT SUM(status=1), SUM(status=2), SUM(revision_after IS NOT NULL AND status=1) FROM afv_canvas_agent_call WHERE user_id = ? AND client = 'mcp' AND agent = 'e2e-agent'", 7001).
		Scan(context.Background(), &ok, &failed, &withRev); err != nil {
		t.Fatal(err)
	}
	// 成功：create、add、connect、3×并发 add = 6；失败：move 冲突 + 2×run_node = 3（同 key 不同参数在占位时就拒绝，不落行）
	if ok != 6 || failed != 3 || withRev != 6 {
		t.Fatalf("审计行数不对: ok=%d failed=%d withRev=%d", ok, failed, withRev)
	}
	var leaked int
	_ = c.s.DB.NewRaw("SELECT COUNT(*) FROM afv_canvas_agent_call WHERE user_id = 7002").Scan(context.Background(), &leaked)
	if leaked != 2 {
		t.Fatalf("越权写 / 生成也应留审计（读不留）: %d", leaked)
	}
}
