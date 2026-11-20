package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/aigateway"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakePrewarmGateway 假网关：记录每次 /v1/media:prewarm 的请求体；status 为响应码（默认 202）。block 非空时先等它关闭。
type fakePrewarmGateway struct {
	mu     sync.Mutex
	bodies []aigateway.PrewarmRequest
	status int
	block  chan struct{}
	// failAfter >0：前 failAfter 次正常，之后回 503
	failAfter int
}

func (g *fakePrewarmGateway) serve(t *testing.T) *aigateway.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.block != nil {
			<-g.block
		}
		if r.URL.Path != "/v1/media:prewarm" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req aigateway.PrewarmRequest
		_ = json.Unmarshal(raw, &req)
		g.mu.Lock()
		g.bodies = append(g.bodies, req)
		status := g.status
		if g.failAfter > 0 && len(g.bodies) > g.failAfter {
			status = http.StatusServiceUnavailable
		}
		g.mu.Unlock()
		if status == 0 {
			status = http.StatusAccepted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			items := make([]aigateway.PrewarmItem, len(req.Inputs))
			for i, in := range req.Inputs {
				items[i] = aigateway.PrewarmItem{URI: in.URI, Status: aigateway.PrewarmQueued}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		} else {
			_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return aigateway.New(aigateway.Config{BaseURL: srv.URL, Secret: strings.Repeat("s", 32), Timeout: 2 * time.Second})
}

func (g *fakePrewarmGateway) requests() []aigateway.PrewarmRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]aigateway.PrewarmRequest(nil), g.bodies...)
}

// resetPrewarm 每个用例一份干净的进程级状态；结束时等后台任务收尾。
func resetPrewarm(t *testing.T) {
	t.Helper()
	refPrewarm.wg.Wait()
	refPrewarm = &prewarmer{seen: map[string]time.Time{}}
	t.Cleanup(func() { refPrewarm.wg.Wait() })
}

func prewarmCore(t *testing.T, g *fakePrewarmGateway) *core {
	c := testCore()
	c.s.Config.Prewarm.Enabled, c.s.Config.Prewarm.TimeoutSeconds, c.s.Config.Prewarm.DedupeSeconds, c.s.Config.Prewarm.MaxConcurrent = true, 5, 600, 4
	c.s.Gateway = g.serve(t)
	return c
}

func groupOf(t *testing.T, c *core, model string) string {
	t.Helper()
	m, ok := c.s.Config.FindModel(model, "", "image")
	if !ok || m.ModelGroup == "" {
		t.Fatalf("模型 %s 没有模型组", model)
	}
	return m.ModelGroup
}

// 图片素材 a、b → Banana（gateway 图片节点）；a 再连一次（重复）、a → 视频模型、a → 工具、mr:// 引用、文字 → 模型都不算。
const prewarmSnap = `{"nodes":[
 {"id":"a","type":"image","data":{"image":"https://cdn.x/a.png?sig=1"}},
 {"id":"b","type":"image","data":{"image":"https://cdn.x/b.png"}},
 {"id":"r","type":"image","data":{"image":"mr://abcdef-user-aaaaaaaaaaaaaaaaaaaaaaaaaa"}},
 {"id":"t","type":"text","data":{"text":"x"}},
 {"id":"m","type":"model","data":{"model":"Banana"}},
 {"id":"m2","type":"model","data":{"model":"Banana"}},
 {"id":"v","type":"model","data":{"kind":"video","model":"Seedance 2.5"}},
 {"id":"tool","type":"tool","data":{"tool":"upscale"}}],
 "edges":[
 {"id":"e1","source":"a","target":"m","targetHandle":"image"},
 {"id":"e2","source":"b","target":"m","targetHandle":"image"},
 {"id":"e3","source":"a","target":"m2","targetHandle":"image"},
 {"id":"e4","source":"a","target":"v","targetHandle":"first"},
 {"id":"e5","source":"a","target":"tool","targetHandle":"image"},
 {"id":"e6","source":"r","target":"m","targetHandle":"image"},
 {"id":"e7","source":"t","target":"m","targetHandle":"prompt"}]}`

func TestPrewarmJobsOnlyGatewayImageNodes(t *testing.T) {
	c := testCore()
	d, err := ParseSnapshot([]byte(prewarmSnap), 0)
	if err != nil {
		t.Fatal(err)
	}
	jobs := c.prewarmJobs(d)
	if len(jobs) != 1 || jobs[0].group != groupOf(t, c, "Banana") {
		t.Fatalf("只应有 Banana 一个模型组: %+v", jobs)
	}
	if got := strings.Join(jobs[0].urls, ","); got != "https://cdn.x/a.png?sig=1,https://cdn.x/b.png" {
		t.Fatalf("只取图片素材节点的 http 地址、组内去重: %s", got)
	}
}

func TestPrewarmSendsOnceWithinDedupeWindow(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{}
	c := prewarmCore(t, g)
	c.schedulePrewarm("cv_1", []byte(prewarmSnap))
	refPrewarm.wg.Wait()
	c.schedulePrewarm("cv_1", []byte(prewarmSnap)) // 再保存一次：窗口内不重复
	refPrewarm.wg.Wait()
	reqs := g.requests()
	if len(reqs) != 1 {
		t.Fatalf("窗口内同图只应预上传一次，实际 %d 次", len(reqs))
	}
	r := reqs[0]
	if r.ModelGroup != groupOf(t, c, "Banana") || r.Operation != "image.edit" || len(r.Inputs) != 2 ||
		r.Inputs[0].URI != "https://cdn.x/a.png?sig=1" || r.Inputs[1].URI != "https://cdn.x/b.png" {
		t.Fatalf("请求 = %+v", r)
	}
	// 新接入一张图：只发新的那张
	snap := strings.Replace(prewarmSnap, `https://cdn.x/b.png`, `https://cdn.x/c.png`, 1)
	c.schedulePrewarm("cv_1", []byte(snap))
	refPrewarm.wg.Wait()
	reqs = g.requests()
	if len(reqs) != 2 || len(reqs[1].Inputs) != 1 || reqs[1].Inputs[0].URI != "https://cdn.x/c.png" {
		t.Fatalf("变化的参考图应只发新增的: %+v", reqs)
	}
}

func TestPrewarmOldGateway404DisablesQuietly(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{status: http.StatusNotFound}
	c := prewarmCore(t, g)
	c.schedulePrewarm("cv_1", []byte(prewarmSnap))
	refPrewarm.wg.Wait()
	if !refPrewarm.off.Load() {
		t.Fatal("网关 404 后应关闭预上传")
	}
	snap := strings.Replace(prewarmSnap, `https://cdn.x/b.png`, `https://cdn.x/c.png`, 1)
	c.schedulePrewarm("cv_1", []byte(snap))
	refPrewarm.wg.Wait()
	if n := len(g.requests()); n != 1 {
		t.Fatalf("关闭后不应再调网关，实际 %d 次", n)
	}
}

func TestPrewarmDisabledByConfig(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{}
	c := prewarmCore(t, g)
	c.s.Config.Prewarm.Enabled = false
	c.schedulePrewarm("cv_1", []byte(prewarmSnap))
	refPrewarm.wg.Wait()
	if n := len(g.requests()); n != 0 {
		t.Fatalf("关闭开关后不应调网关，实际 %d 次", n)
	}
}

func TestPrewarmFailureIsRetriedOnNextSave(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{status: http.StatusServiceUnavailable}
	c := prewarmCore(t, g)
	c.schedulePrewarm("cv_1", []byte(prewarmSnap))
	refPrewarm.wg.Wait()
	g.mu.Lock()
	g.status = 0
	g.mu.Unlock()
	c.schedulePrewarm("cv_1", []byte(prewarmSnap))
	refPrewarm.wg.Wait()
	if reqs := g.requests(); len(reqs) != 2 || refPrewarm.off.Load() {
		t.Fatalf("暂时失败的条目应放回、下次保存重试（且不关闭功能）: %d 次, off=%v", len(reqs), refPrewarm.off.Load())
	}
}

// 网关卡住时保存照常立即返回：预上传在后台协程里，不在保存路径上。
func TestSaveCanvasNotBlockedByPrewarm(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{block: make(chan struct{})}
	c, mock := mockCore(t)
	c.s.Config.Prewarm.Enabled, c.s.Config.Prewarm.TimeoutSeconds, c.s.Config.Prewarm.DedupeSeconds, c.s.Config.Prewarm.MaxConcurrent = true, 5, 600, 4
	c.s.Gateway = g.serve(t)
	expectCanvas(mock, "100-aaaa")
	mock.ExpectQuery("SELECT DISTINCT `node_id` FROM `afv_canvas_node_run`").WillReturnRows(sqlmock.NewRows([]string{"node_id"}))
	mock.ExpectExec("UPDATE `afv_canvas` SET").WillReturnResult(sqlmock.NewResult(0, 1))
	t0 := time.Now()
	resp, err := c.SaveCanvas(context.Background(), &canvas.SaveCanvasReq{UserId: 7, Id: "cv_1", Snapshot: prewarmSnap, IfMatchRevision: "100-aaaa"})
	elapsed := time.Since(t0)
	close(g.block)
	if err != nil || resp.Conflict {
		t.Fatalf("保存应成功: %+v %v", resp, err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("保存被预上传拖慢: %v", elapsed)
	}
	refPrewarm.wg.Wait()
	if n := len(g.requests()); n != 1 {
		t.Fatalf("保存后应在后台预上传一次，实际 %d 次", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 分批预上传（每批 MaxPrewarmInputs 张）：第二批失败时只放回第二批的去重键，第一批下次保存不再重复发。
func TestPrewarmChunkFailureForgetsOnlyFailedChunk(t *testing.T) {
	resetPrewarm(t)
	g := &fakePrewarmGateway{}
	c := prewarmCore(t, g)
	n := aigateway.MaxPrewarmInputs + 2
	var nodes, edges []string
	for i := 0; i < n; i++ {
		nodes = append(nodes, fmt.Sprintf(`{"id":"i%d","type":"image","data":{"image":"https://cdn.x/%d.png"}}`, i, i))
		edges = append(edges, fmt.Sprintf(`{"id":"e%d","source":"i%d","target":"m","targetHandle":"image"}`, i, i))
	}
	nodes = append(nodes, `{"id":"m","type":"model","data":{"model":"Banana"}}`)
	snap := `{"nodes":[` + strings.Join(nodes, ",") + `],"edges":[` + strings.Join(edges, ",") + `]}`
	g.failAfter = 1 // 第 1 次成功，之后 503
	c.schedulePrewarm("cv_1", []byte(snap))
	refPrewarm.wg.Wait()
	reqs := g.requests()
	if len(reqs) != 2 || len(reqs[0].Inputs) != aigateway.MaxPrewarmInputs || len(reqs[1].Inputs) != 2 {
		t.Fatalf("应分两批发（16 + 2）: %d 次", len(reqs))
	}
	g.mu.Lock()
	g.failAfter = 0
	g.mu.Unlock()
	c.schedulePrewarm("cv_1", []byte(snap))
	refPrewarm.wg.Wait()
	reqs = g.requests()
	if len(reqs) != 3 || len(reqs[2].Inputs) != 2 ||
		reqs[2].Inputs[0].URI != reqs[1].Inputs[0].URI || reqs[2].Inputs[1].URI != reqs[1].Inputs[1].URI {
		t.Fatalf("重试只应发上次失败那批的 2 张: %+v", reqs[len(reqs)-1])
	}
}
