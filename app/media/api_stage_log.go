package media

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

// logStage 打一行阶段耗时（入库链路各阶段）。trace_id 与访问日志对齐，用来定位慢在哪一步。
// 请求上下文里有阶段收集器（withStageTimings）时同时记进去，由 ingest、ingest/upload、resources/copy 写进响应的 timings。
func logStage(ctx context.Context, stage string, start time.Time) {
	d := time.Since(start)
	log.Printf("media stage trace_id=%s stage=%s dur_ms=%d", TraceIDFromContext(ctx), stage, d.Milliseconds())
	if c, ok := ctx.Value(stageTimingsKey{}).(*stageTimings); ok {
		c.add(stage, d)
	}
}

// StageTiming 响应里的一条阶段耗时（契约 C1：{"stage": string, "dur_ms": int}）。
type StageTiming struct {
	Stage string `json:"stage"`
	DurMs int64  `json:"dur_ms"`
}

type stageTimingsKey struct{}

// stageTimings 一次请求的阶段耗时收集器。同一请求里可能有并发的阶段（如多帧落桶），所以加锁。
type stageTimings struct {
	mu    sync.Mutex
	items []StageTiming
}

func (c *stageTimings) add(stage string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, StageTiming{Stage: stage, DurMs: d.Milliseconds()})
}

func (c *stageTimings) snapshot() []StageTiming {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]StageTiming(nil), c.items...)
}

// withStageTimings 在上下文里装一个收集器。没有装收集器的请求只打日志，响应不变。
func withStageTimings(ctx context.Context) (context.Context, *stageTimings) {
	c := &stageTimings{}
	return context.WithValue(ctx, stageTimingsKey{}, c), c
}

func stageTimingsOf(ctx context.Context) *stageTimings {
	c, _ := ctx.Value(stageTimingsKey{}).(*stageTimings)
	return c
}

// stageTimed 路由包装：为该路由的请求装上阶段收集器（ingest、resources/copy；字节上传自己装）。
// 放在 idempotent 外层：幂等记录里存的是不带 timings 的响应体，回放不会带上第一次执行的耗时。
func (s *Server) stageTimed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := withStageTimings(r.Context())
		next(w, r.WithContext(ctx))
	}
}

// appendTimings 把 timings 加进 JSON 对象响应体的末尾（附加字段，契约只增不改）。
// 没有耗时、或响应体不是 JSON 对象时原样返回。
func appendTimings(body []byte, timings []StageTiming) []byte {
	if len(timings) == 0 {
		return body
	}
	trimmed := bytes.TrimRight(body, " \t\r\n")
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return body
	}
	raw, err := json.Marshal(timings)
	if err != nil {
		return body
	}
	out := make([]byte, 0, len(trimmed)+len(raw)+16)
	out = append(out, trimmed[:len(trimmed)-1]...)
	if len(bytes.TrimSpace(trimmed[1:len(trimmed)-1])) > 0 {
		out = append(out, ',')
	}
	out = append(out, `"timings":`...)
	out = append(out, raw...)
	out = append(out, '}', '\n')
	return out
}
