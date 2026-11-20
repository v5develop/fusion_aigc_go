package canvasevents

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/result"
	"aigc-local-life/pkg/xerr"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const cid = "cv_0123456789abcdef"

// fakeCanvas 归属与当前 revision 的替身：只有 owner 能看到。
type fakeCanvas struct {
	mu    sync.Mutex
	owner int64
	rev   string
	gone  bool
}

func (f *fakeCanvas) set(rev string) { f.mu.Lock(); f.rev = rev; f.mu.Unlock() }

func (f *fakeCanvas) source(_ context.Context, uid int64, id string) (*canvas_client.CanvasRevisionResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if uid != f.owner || id != cid || f.gone {
		return nil, xerr.NotFoundf("画布")
	}
	return &canvas_client.CanvasRevisionResp{Revision: f.rev, UpdatedAt: 1, NodeCount: 2}, nil
}

func setup(t *testing.T, withRedis bool) (*Server, *fakeCanvas, *canvasevent.Publisher, *httptest.Server) {
	t.Helper()
	result.Setup()
	fc := &fakeCanvas{owner: 7, rev: "1000-aaaa"}
	var rdb redis.UniversalClient
	if withRedis {
		mr := miniredis.RunT(t)
		c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = c.Close() })
		rdb = c
	}
	s := &Server{Hub: canvasevent.NewHub(rdb), Revision: fc.source, Conns: NewConnLimiter(rdb),
		Opt: Options{MaxConnsPerUser: 5, Heartbeat: 150 * time.Millisecond, MaxLifetime: time.Minute, PollInterval: time.Second}}
	t.Cleanup(func() { _ = s.Hub.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, _ := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/canvas/canvases/"), "/")
		r = pathvar.WithVars(r.WithContext(ctxdata.WithUserID(r.Context(), uid)), map[string]string{"id": parts[0]})
		if len(parts) > 1 && parts[1] == "revision" {
			s.CurrentRevision(w, r)
			return
		}
		s.Events(w, r)
	}))
	t.Cleanup(srv.Close)
	return s, fc, canvasevent.NewPublisher(rdb), srv
}

type frame struct{ id, event, data string }

type stream struct {
	resp   *http.Response
	frames chan frame
	cancel context.CancelFunc
}

func open(t *testing.T, srv *httptest.Server, uid int64, id string, hdr map[string]string) (*stream, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/canvas/canvases/"+id+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Test-User", strconv.FormatInt(uid, 10))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		cancel()
		return nil, resp
	}
	st := &stream{resp: resp, frames: make(chan frame, 64), cancel: cancel}
	go func() {
		defer close(st.frames)
		sc := bufio.NewScanner(resp.Body)
		var f frame
		for sc.Scan() {
			l := sc.Text()
			switch {
			case l == "":
				if f.event != "" || f.data != "" {
					st.frames <- f
				}
				f = frame{}
			case strings.HasPrefix(l, "id: "):
				f.id = l[4:]
			case strings.HasPrefix(l, "event: "):
				f.event = l[7:]
			case strings.HasPrefix(l, "data: "):
				f.data = l[6:]
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	return st, resp
}

func (s *stream) next(t *testing.T, event string) frame {
	t.Helper()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-s.frames:
			if !ok {
				t.Fatalf("流已结束，没等到 %s", event)
			}
			if f.event == event {
				return f
			}
		case <-timeout:
			t.Fatalf("超时没等到 %s", event)
		}
	}
}

func eventOf(t *testing.T, f frame) canvasevent.Event {
	t.Helper()
	var e canvasevent.Event
	if err := json.Unmarshal([]byte(f.data), &e); err != nil {
		t.Fatalf("data: %s", f.data)
	}
	return e
}

func TestEventsOwnership(t *testing.T) {
	_, _, _, srv := setup(t, true)
	for _, c := range []struct {
		uid int64
		id  string
	}{{8, cid}, {7, "cv_ffffffffffffffff"}, {7, "../x"}} {
		st, resp := open(t, srv, c.uid, c.id, nil)
		if st != nil {
			t.Fatalf("user=%d id=%s 不应开流", c.uid, c.id)
		}
		var body struct{ Code int }
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if body.Code != xerr.NotFound {
			t.Fatalf("他人 / 不存在应 4001: %d", body.Code)
		}
	}
}

func TestEventsPushAndRun(t *testing.T) {
	s, fc, pub, srv := setup(t, true)
	st, _ := open(t, srv, 7, cid, nil)
	hello := st.next(t, "hello")
	if !strings.Contains(hello.data, `"mode":"push"`) || !strings.Contains(hello.data, `"revision":"1000-aaaa"`) || hello.id != "" {
		t.Fatalf("hello: %+v", hello)
	}
	waitSubscribed(t, s)
	fc.set("2000-bbbb")
	if err := pub.Publish(context.Background(), canvasevent.Event{CanvasID: cid, Revision: "2000-bbbb", Source: "agent", Actor: "svc:java-1", ChangedNodeIDs: []string{"m_1"}}); err != nil {
		t.Fatal(err)
	}
	f := st.next(t, "canvas")
	e := eventOf(t, f)
	if f.id != "2000-bbbb" || e.Source != "agent" || e.Actor != "svc:java-1" || len(e.ChangedNodeIDs) != 1 || e.At == 0 {
		t.Fatalf("变更事件: %+v %+v", f, e)
	}
	// 旧 revision（乱序）与重复事件不推；run 事件无 id
	_ = pub.Publish(context.Background(), canvasevent.Event{CanvasID: cid, Revision: "1500-cccc", Source: "user"})
	_ = pub.Publish(context.Background(), canvasevent.Event{CanvasID: cid, Revision: "2000-bbbb", Source: "user"})
	_ = pub.Publish(context.Background(), canvasevent.Event{CanvasID: cid, Revision: "2000-bbbb", Source: "run", RequestID: "9", RunStatus: "succeeded", ChangedNodeIDs: []string{"m_1"}})
	r := st.next(t, "run")
	if r.id != "" || eventOf(t, r).RequestID != "9" {
		t.Fatalf("run 事件: %+v", r)
	}
	select {
	case f := <-st.frames:
		if f.event == "canvas" {
			t.Fatalf("旧 / 重复事件不应推: %+v", f)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEventsResumeWithLastEventID(t *testing.T) {
	_, _, _, srv := setup(t, true)
	st, _ := open(t, srv, 7, cid, map[string]string{"Last-Event-ID": "900-zzzz"})
	st.next(t, "hello")
	f := st.next(t, "canvas")
	e := eventOf(t, f)
	if f.id != "1000-aaaa" || !e.Resync || e.Revision != "1000-aaaa" {
		t.Fatalf("续传应先补发最新事件: %+v %+v", f, e)
	}
	// 已是最新则不补发
	st2, _ := open(t, srv, 7, cid, map[string]string{"Last-Event-ID": "1000-aaaa"})
	st2.next(t, "hello")
	select {
	case f := <-st2.frames:
		if f.event == "canvas" {
			t.Fatalf("revision 未变不应补发: %+v", f)
		}
	case <-time.After(80 * time.Millisecond):
	}
	// ?since= 同义（首连时带本地编辑起点）
	st3, _ := open(t, srv, 7, cid+"?since=800-yyyy", nil)
	st3.next(t, "hello")
	if !eventOf(t, st3.next(t, "canvas")).Resync {
		t.Fatal("since 应触发补发")
	}
}

func TestEventsHeartbeatReconcileAndGone(t *testing.T) {
	_, fc, _, srv := setup(t, true)
	st, _ := open(t, srv, 7, cid, nil)
	st.next(t, "hello")
	fc.set("3000-dddd") // 发布端没发事件（未配 Redis / 消息丢失）
	f := st.next(t, "canvas")
	if f.id != "3000-dddd" || !eventOf(t, f).Resync {
		t.Fatalf("心跳对账应补发: %+v", f)
	}
	fc.mu.Lock()
	fc.gone = true
	fc.mu.Unlock()
	st.next(t, "gone")
}

func TestEventsConnLimit(t *testing.T) {
	_, _, _, srv := setup(t, true)
	for i := 0; i < 5; i++ {
		st, _ := open(t, srv, 7, cid, nil)
		st.next(t, "hello")
	}
	st, resp := open(t, srv, 7, cid, nil)
	if st != nil {
		t.Fatal("第 6 条应被拒")
	}
	var body struct {
		Code   int
		Reason string
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if body.Code != xerr.Conflict || body.Reason != ReasonTooMany {
		t.Fatalf("超限: %+v", body)
	}
}

func TestEventsPollFallback(t *testing.T) {
	_, _, _, srv := setup(t, false)
	st, _ := open(t, srv, 7, cid, nil)
	hello := st.next(t, "hello")
	if !strings.Contains(hello.data, `"mode":"poll"`) || !strings.Contains(hello.data, "/revision") {
		t.Fatalf("无 Redis 首帧应提示 poll: %+v", hello)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/canvas/canvases/"+cid+"/revision", nil)
	req.Header.Set("X-Test-User", "7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Code int
		Data map[string]any
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != 0 || body.Data["revision"] != "1000-aaaa" || body.Data["push"] != false {
		t.Fatalf("轮询接口: %+v", body)
	}
}

func TestConnLimiterExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	for _, l := range []ConnLimiter{NewConnLimiter(rdb), NewConnLimiter(nil)} {
		now := time.Now()
		switch x := l.(type) {
		case *redisConns:
			x.now = func() time.Time { return now }
		case *memConns:
			x.now = func() time.Time { return now }
		}
		ctx := context.Background()
		for i := 0; i < 2; i++ {
			if ok, err := l.Acquire(ctx, 1, "c"+strconv.Itoa(i), 2, time.Minute); !ok || err != nil {
				t.Fatal("名额内应占到", err)
			}
		}
		if ok, _ := l.Acquire(ctx, 1, "c3", 2, time.Minute); ok {
			t.Fatal("超限")
		}
		l.Release(ctx, 1, "c0")
		if ok, _ := l.Acquire(ctx, 1, "c3", 2, time.Minute); !ok {
			t.Fatal("释放后应能占")
		}
		now = now.Add(2 * time.Minute) // 崩溃残留过期
		if ok, _ := l.Acquire(ctx, 1, "c4", 2, time.Minute); !ok {
			t.Fatal("过期名额应自动回收")
		}
	}
}

func waitSubscribed(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if s.Hub.Channels() > 0 {
			time.Sleep(20 * time.Millisecond) // SUBSCRIBE 已发出，给 miniredis 一点时间
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("没订阅上")
}

// 心跳复核登录态：连接令牌失效 → bye(REAUTH) 并关闭（前端刷新令牌后重连，不永久停同步）；查询抖动（transient）不断开。
func TestEventsSessionRecheck(t *testing.T) {
	s, _, _, srv := setup(t, true)
	var mu sync.Mutex
	ok, transient := true, false
	s.Session = func(_ *http.Request, uid int64) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return ok && uid == 7, transient
	}
	st, _ := open(t, srv, 7, cid, nil)
	st.next(t, "hello")
	mu.Lock()
	ok, transient = false, true
	mu.Unlock()
	time.Sleep(400 * time.Millisecond) // 两三个心跳：抖动不应断开
	for drained := false; !drained; {
		select {
		case f, more := <-st.frames:
			if !more || f.event == "gone" || f.event == "bye" {
				t.Fatalf("查询抖动不应断开: %+v", f)
			}
		default:
			drained = true
		}
	}
	mu.Lock()
	transient = false
	mu.Unlock()
	g := st.next(t, "bye")
	if !strings.Contains(g.data, CodeReauth) {
		t.Fatalf("应 bye REAUTH（刷新令牌后重连），不是 gone: %+v", g)
	}
	select {
	case _, more := <-st.frames:
		if more {
			// 允许读到残留帧，但流随后必须结束
			for range st.frames {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gone 之后连接应关闭")
	}
}

type flakyConns struct {
	ConnLimiter
	acquireErr, refreshErr error
}

func (f *flakyConns) Acquire(ctx context.Context, uid int64, id string, limit int, ttl time.Duration) (bool, error) {
	if f.acquireErr != nil {
		return false, f.acquireErr
	}
	return f.ConnLimiter.Acquire(ctx, uid, id, limit, ttl)
}

func (f *flakyConns) Refresh(ctx context.Context, uid int64, id string, ttl time.Duration) error {
	if f.refreshErr != nil {
		return f.refreshErr
	}
	return f.ConnLimiter.Refresh(ctx, uid, id, ttl)
}

// 名额计数 / 续期出错：发 bye(CONN_SLOT_UNAVAILABLE) 结束，而不是放行。
func TestEventsConnSlotErrors(t *testing.T) {
	s, _, _, srv := setup(t, true)
	s.Conns = &flakyConns{ConnLimiter: NewConnLimiter(nil), acquireErr: context.DeadlineExceeded}
	st, _ := open(t, srv, 7, cid, nil)
	if b := st.next(t, "bye"); !strings.Contains(b.data, CodeSlotFailed) {
		t.Fatalf("计数出错应 bye: %+v", b)
	}
	s.Conns = &flakyConns{ConnLimiter: NewConnLimiter(nil), refreshErr: context.DeadlineExceeded}
	st2, _ := open(t, srv, 7, cid, nil)
	st2.next(t, "hello")
	if b := st2.next(t, "bye"); !strings.Contains(b.data, CodeSlotFailed) {
		t.Fatalf("续期出错应 bye: %+v", b)
	}
}

// 续期用 ZADD（不带 XX）：名额被当作残留清掉后续期能加回，计数不失真。
func TestRedisRefreshReAdds(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	l := NewConnLimiter(rdb)
	ctx := context.Background()
	if err := l.Refresh(ctx, 9, "c1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if n, _ := rdb.ZCard(ctx, connKey(9)).Result(); n != 1 {
		t.Fatalf("续期应加回名额: %d", n)
	}
	mr.Close()
	if err := l.Refresh(ctx, 9, "c1", time.Minute); err == nil {
		t.Fatal("Redis 不可用时续期应报错")
	}
}
