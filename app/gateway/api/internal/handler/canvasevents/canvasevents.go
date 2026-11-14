// Package canvasevents 画布实时同步的网关入口（P0-8）：
//
//	GET /api/v1/canvas/canvases/:id/events    SSE：订阅画布变更事件（Redis pub/sub canvas:{id}:events，canvas-rpc 发布）
//	GET /api/v1/canvas/canvases/:id/revision  当前 revision（无 Redis 时的降级轮询接口；有 Redis 也可用）
//
// 两个入口都在 Auth（会话令牌）之后，并经 canvas-rpc GetCanvasRevision 校验画布归属（他人 / 不存在一律 4001）。
// SSE 协议（设计见 docs/design/canvas-agent-tools.md §11）：
//
//	首帧    event: hello  data: {"mode":"push"|"poll", canvasId, revision, heartbeatMs | pollUrl, pollIntervalMs}
//	变更    id: <revision>  event: canvas  data: canvasevent.Event（source=user|agent；resync=true 为网关补发的对账事件）
//	生成    event: run  data: canvasevent.Event（source=run，无 id：revision 不变）
//	心跳    ": ping"（每 15 秒；同时比一次当前 revision，变了就补发 resync 事件——pub/sub 丢消息、发布端没配 Redis 时靠它兜底）
//	结束    event: gone（不要重连：code=CANVAS_GONE | SESSION_EXPIRED）| event: bye（可重连：code=RECONNECT | CONN_SLOT_UNAVAILABLE）
//	        心跳时还复核登录态（与 Auth 同一套解析），失效即 gone · SESSION_EXPIRED；名额计数 / 续期出错发 bye，不放行。
//
// 续传：重连时带 Last-Event-ID（EventSource 自动带）或 ?since=<revision>；与当前 revision 不同就先补发一条 resync 事件。
// 每用户同时最多 CanvasEvents.MaxConnsPerUser（默认 5）条，超出回 4002 · CANVAS_EVENTS_TOO_MANY。
// 不走 goctl 生成的 routes.go（gateway.api 是共享文件），由 gateway.go 显式调用 Register。
package canvasevents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

// ReasonTooMany 每用户 SSE 连接数超限。
const ReasonTooMany = "CANVAS_EVENTS_TOO_MANY"

var canvasIDRe = regexp.MustCompile(`^cv_[0-9a-f]{16}$`)

// RevisionSource 取画布当前 revision（含归属校验）；网关里是 canvas-rpc GetCanvasRevision，测试里替身。
type RevisionSource func(ctx context.Context, userID int64, canvasID string) (*canvas_client.CanvasRevisionResp, error)

// Options SSE 参数。
type Options struct {
	MaxConnsPerUser int
	Heartbeat       time.Duration
	MaxLifetime     time.Duration
	PollInterval    time.Duration
}

// Server SSE 与轮询处理器。
type Server struct {
	Hub      *canvasevent.Hub // nil = 无 Redis，降级轮询
	Revision RevisionSource
	Conns    ConnLimiter
	// Session 心跳时复核登录态（网关 Auth 同一套解析）；nil 不复核
	Session func(r *http.Request, uid int64) (ok, transient bool)
	Opt     Options
}

// gone / bye 帧里的 code。
const (
	CodeCanvasGone     = "CANVAS_GONE"           // gone：画布已删除 / 不再属于你
	CodeSessionExpired = "SESSION_EXPIRED"       // gone：登录已失效（过期 / 被踢下线），前端去登录，不要重连
	CodeReconnect      = "RECONNECT"             // bye：到达最长存活，带 Last-Event-ID 重连
	CodeSlotFailed     = "CONN_SLOT_UNAVAILABLE" // bye：连接名额计数 / 续期失败，稍后重连
)

// Register 注册两个入口。
func Register(server *rest.Server, svcCtx *svc.ServiceContext) *Server {
	var rdb redis.UniversalClient
	if svcCtx.Session != nil {
		if rc := svcCtx.Session.Redis(); rc != nil {
			rdb = rc
		}
	}
	c := svcCtx.Config.CanvasEvents
	s := &Server{
		Hub: canvasevent.NewHub(rdb),
		Revision: func(ctx context.Context, uid int64, id string) (*canvas_client.CanvasRevisionResp, error) {
			return svcCtx.CanvasRpc.GetCanvasRevision(ctx, &canvas_client.CanvasIdReq{UserId: uid, Id: id})
		},
		Conns:   NewConnLimiter(rdb),
		Session: middleware.NewAuthMiddleware(svcCtx.Config.Auth.Mode, svcCtx.Config.Auth.Jwt, svcCtx.Session).SessionValid,
		Opt: Options{
			MaxConnsPerUser: c.MaxConnsPerUser, Heartbeat: time.Duration(c.HeartbeatSeconds) * time.Second,
			MaxLifetime: time.Duration(c.MaxLifetimeMinutes) * time.Minute, PollInterval: time.Duration(c.PollIntervalMs) * time.Millisecond,
		},
	}
	if s.Hub == nil {
		logx.Info("gateway: 未配置会话 Redis，画布 SSE 降级为轮询（首帧 mode=poll，前端改轮询 /revision）")
	}
	mw := []rest.Middleware{svcCtx.Auth}
	server.AddRoutes(rest.WithMiddlewares(mw, rest.Route{Method: http.MethodGet, Path: "/canvases/:id/events", Handler: s.Events}),
		rest.WithPrefix("/api/v1/canvas"), rest.WithSSE())
	server.AddRoutes(rest.WithMiddlewares(mw, rest.Route{Method: http.MethodGet, Path: "/canvases/:id/revision", Handler: s.CurrentRevision}),
		rest.WithPrefix("/api/v1/canvas"))
	return s
}

func (s *Server) opts() Options {
	o := s.Opt
	if o.MaxConnsPerUser <= 0 {
		o.MaxConnsPerUser = 5
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.MaxLifetime <= 0 {
		o.MaxLifetime = 30 * time.Minute
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	return o
}

// owned 解析画布 id 并校验归属；失败已写 JSON 错误。
func (s *Server) owned(w http.ResponseWriter, r *http.Request) (int64, string, *canvas_client.CanvasRevisionResp) {
	uid := ctxdata.UserID(r.Context())
	if uid <= 0 {
		writeErr(w, r, xerr.New(xerr.Unauthorized, "未登录"))
		return 0, "", nil
	}
	id := pathvar.Vars(r)["id"]
	if !canvasIDRe.MatchString(id) {
		writeErr(w, r, xerr.NotFoundf("画布"))
		return 0, "", nil
	}
	cur, err := s.Revision(r.Context(), uid, id)
	if err != nil {
		writeErr(w, r, err)
		return 0, "", nil
	}
	return uid, id, cur
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Del("Cache-Control")
	w.Header().Del("Connection")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	httpx.ErrorCtx(r.Context(), w, err)
}

// CurrentRevision GET /canvases/:id/revision。
func (s *Server) CurrentRevision(w http.ResponseWriter, r *http.Request) {
	_, id, cur := s.owned(w, r)
	if cur == nil {
		return
	}
	httpx.OkJsonCtx(r.Context(), w, map[string]any{
		"canvas_id": id, "revision": cur.Revision, "updated_at": cur.UpdatedAt, "node_count": cur.NodeCount,
		"push":             s.Hub != nil, // false：没有事件推送，前端按 poll_interval_ms 轮询本接口
		"poll_interval_ms": s.opts().PollInterval.Milliseconds(),
	})
}

// sse 一条 SSE 连接的写端。
type sse struct {
	w http.ResponseWriter
	f http.Flusher
}

func (c *sse) send(id, event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	var sb strings.Builder
	if id != "" {
		sb.WriteString("id: " + id + "\n")
	}
	if event != "" {
		sb.WriteString("event: " + event + "\n")
	}
	sb.WriteString("data: ")
	sb.Write(b)
	sb.WriteString("\n\n")
	return c.raw(sb.String())
}

func (c *sse) raw(s string) error {
	if _, err := c.w.Write([]byte(s)); err != nil {
		return err
	}
	if c.f != nil {
		c.f.Flush()
	}
	return nil
}

// Events GET /canvases/:id/events（SSE）。
func (s *Server) Events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o := s.opts()
	uid, id, cur := s.owned(w, r)
	if cur == nil {
		return
	}
	connID := newConnID()
	ok, err := s.Conns.Acquire(ctx, uid, connID, o.MaxConnsPerUser, o.Heartbeat*3)
	if err != nil {
		// 计数故障不放行（否则上限形同虚设）：开流发 bye，客户端按 retry 间隔重连
		logx.WithContext(ctx).Errorf("gateway: 画布 SSE 连接计数失败 user=%d canvas=%s（check=canvas_events.conn_acquire）: %v", uid, id, err)
		c := startStream(w)
		_ = c.raw(fmt.Sprintf("retry: %d\n", o.PollInterval.Milliseconds()))
		_ = c.send("", "bye", map[string]any{"canvasId": id, "code": CodeSlotFailed, "reason": "实时连接暂不可用，稍后自动重连"})
		return
	} else if !ok {
		logx.WithContext(ctx).Infof("gateway: 画布 SSE 连接数超限 user=%d canvas=%s（check=canvas_events.max_conns limit=%d）", uid, id, o.MaxConnsPerUser)
		writeErr(w, r, xerr.NewReason(xerr.Conflict, ReasonTooMany, fmt.Sprintf("同时打开的画布实时连接太多（最多 %d 个），请关闭其他画布页面后重试", o.MaxConnsPerUser)))
		return
	}
	defer s.Conns.Release(context.WithoutCancel(ctx), uid, connID)

	var sub *canvasevent.Sub
	if s.Hub != nil {
		// 先订阅再发 hello / 对账：之后发布的事件都不会漏
		if sub, err = s.Hub.Subscribe(ctx, id); err != nil {
			logx.WithContext(ctx).Errorf("gateway: 订阅画布事件失败 canvas=%s，本连接降级为轮询: %v", id, err)
		} else {
			defer sub.Close()
		}
	}

	c := startStream(w)

	if sub == nil {
		_ = c.raw(fmt.Sprintf("retry: %d\n", o.PollInterval.Milliseconds()*6))
		_ = c.send("", "hello", map[string]any{
			"mode": "poll", "canvasId": id, "revision": cur.Revision, "updatedAt": cur.UpdatedAt,
			"pollUrl": "/api/v1/canvas/canvases/" + id + "/revision", "pollIntervalMs": o.PollInterval.Milliseconds(),
		})
		return
	}
	if err := c.send("", "hello", map[string]any{
		"mode": "push", "canvasId": id, "revision": cur.Revision, "updatedAt": cur.UpdatedAt, "heartbeatMs": o.Heartbeat.Milliseconds(),
	}); err != nil {
		return
	}
	last := cur.Revision
	since := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if since == "" {
		since = strings.TrimSpace(r.URL.Query().Get("since"))
	}
	if since != "" && since != cur.Revision {
		if c.send(cur.Revision, "canvas", resyncEvent(id, cur)) != nil {
			return
		}
	}

	beat := time.NewTicker(o.Heartbeat)
	defer beat.Stop()
	deadline := time.NewTimer(o.MaxLifetime)
	defer deadline.Stop()
	// reconcile 当前 revision 与已推送的不同 → 补发 resync；画布没了 → gone 并结束。返回 false 表示连接该结束。
	reconcile := func() bool {
		if s.Session != nil {
			if ok, transient := s.Session(r, uid); !ok && !transient {
				logx.WithContext(ctx).Infof("gateway: 画布 SSE 登录态失效，关闭 user=%d canvas=%s（check=canvas_events.session）", uid, id)
				_ = c.send("", "gone", map[string]any{"canvasId": id, "code": CodeSessionExpired, "reason": "登录已失效，请重新登录"})
				return false
			}
		}
		now, err := s.Revision(ctx, uid, id)
		if err != nil {
			if code, _, ok := xerr.Parse(err); ok && (code == xerr.NotFound || code == xerr.Unauthorized) {
				_ = c.send("", "gone", map[string]any{"canvasId": id, "code": CodeCanvasGone, "reason": "画布已删除或不再属于你"})
				return false
			}
			return true // 暂时性失败：下个心跳再比
		}
		if now.Revision != last {
			if c.send(now.Revision, "canvas", resyncEvent(id, now)) != nil {
				return false
			}
			last = now.Revision
		}
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			_ = c.send("", "bye", map[string]any{"canvasId": id, "code": CodeReconnect, "reason": "reconnect"})
			return
		case <-beat.C:
			if c.raw(": ping\n\n") != nil {
				return
			}
			if err := s.Conns.Refresh(ctx, uid, connID, o.Heartbeat*3); err != nil {
				// 续期失败：名额会过期被别人占走，继续放行就超限了——结束连接让客户端重连重新占名额
				logx.WithContext(ctx).Errorf("gateway: 画布 SSE 名额续期失败 user=%d canvas=%s（check=canvas_events.conn_refresh）: %v", uid, id, err)
				_ = c.send("", "bye", map[string]any{"canvasId": id, "code": CodeSlotFailed, "reason": "实时连接暂不可用，稍后自动重连"})
				return
			}
			sub.TakeLost()
			if !reconcile() {
				return
			}
		case msg := <-sub.C:
			var ev canvasevent.Event
			if json.Unmarshal(msg, &ev) != nil || ev.CanvasID != id {
				continue
			}
			ev.Clip()
			if ev.Source == canvasevent.SourceRun {
				if c.send("", "run", ev) != nil {
					return
				}
				continue
			}
			// 乱序到达的旧事件（多实例发布）、重复事件不推；revision 以字符串相等判变化
			if ev.Revision == last || canvasevent.Older(ev.Revision, last) {
				continue
			}
			if c.send(ev.Revision, "canvas", ev) != nil {
				return
			}
			last = ev.Revision
			if sub.TakeLost() && !reconcile() { // 缓冲满丢过消息：立刻对一次账
				return
			}
		}
	}
}

// startStream 写 SSE 响应头。
func startStream(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx 不缓冲
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &sse{w: w, f: f}
}

func resyncEvent(id string, cur *canvas_client.CanvasRevisionResp) canvasevent.Event {
	at := cur.UpdatedAt
	if at == 0 {
		at = time.Now().UnixMilli()
	}
	return canvasevent.Event{CanvasID: id, Revision: cur.Revision, Source: "unknown", Actor: "", ChangedNodeIDs: []string{}, At: at, Resync: true}
}

func newConnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
