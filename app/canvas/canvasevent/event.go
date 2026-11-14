// Package canvasevent 画布变更事件（P0-8 实时同步）：canvas-rpc 发布、网关 SSE 订阅，两边共用的载荷与频道约定。
//
//   - 频道：Redis pub/sub `canvas:{canvasId}:events`（pub/sub 不分库：发布方与订阅方连同一台 Redis 即可，DB 号无关）。
//   - 载荷：Event 的 JSON（camelCase，SSE 原样下发给前端）。
//   - 至多一次：pub/sub 不落盘、订阅断开期间的消息会丢，所以网关 SSE 另有心跳对账（每 15 秒比一次当前 revision），
//     断线重连按 Last-Event-ID（= revision）补发最新事件。事件只是「该刷新了」的提示，前端以 GET 画布的结果为准。
//
// 设计见 docs/design/canvas-agent-tools.md §11。
package canvasevent

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 事件来源。
const (
	SourceUser  = "user"  // 用户保存（编辑器 PUT /canvases/:id）与新建画布
	SourceAgent = "agent" // 智能体工具写入（MCP / Function Call / 服务间 HMAC 入口）
	SourceRun   = "run"   // 生成状态变化（受理 / 成功 / 失败 / 取消 / 选中候选）：run 结果不写快照，revision 不变
)

// MaxChangedNodeIDs changedNodeIds 截断上限（超出置 truncated=true，前端按整画布刷新处理）。
const MaxChangedNodeIDs = 200

// Event 一次画布变更。
type Event struct {
	CanvasID string `json:"canvasId"`
	// Revision 变更后的 revision；source=run 时是事件发出时画布的当前 revision（run 不改 revision）
	Revision string `json:"revision"`
	Source   string `json:"source"`
	// Actor 谁改的：user:<userId> / svc:<keyId>（服务间入口）/ <client>:<agent>（MCP、Function Call）/ run:<requestId>
	Actor string `json:"actor"`
	// ChangedNodeIDs 变化的节点（新增 / 删除 / 内容或位置改动 / 连线增删涉及的两端）；≤200，超出截断并置 Truncated
	ChangedNodeIDs []string `json:"changedNodeIds"`
	Truncated      bool     `json:"truncated,omitempty"`
	// At 事件时间（unix 毫秒）
	At int64 `json:"at"`
	// 以下只有 source=run 时有
	RequestID string `json:"requestId,omitempty"`
	RunStatus string `json:"runStatus,omitempty"`
	// Resync 网关补发的对账事件（断线期间错过了变更 / 心跳发现 revision 已变）：没有 changedNodeIds，前端整画布刷新
	Resync bool `json:"resync,omitempty"`
}

// Channel 画布的事件频道。
func Channel(canvasID string) string { return "canvas:" + canvasID + ":events" }

// CanvasOfChannel 频道 → 画布 id（不是本包频道返回空串）。
func CanvasOfChannel(ch string) string {
	if !strings.HasPrefix(ch, "canvas:") || !strings.HasSuffix(ch, ":events") {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(ch, "canvas:"), ":events")
}

// Clip 截断 changedNodeIds（≤MaxChangedNodeIDs），nil 归一为空数组（前端不必判 null）。
func (e *Event) Clip() {
	if len(e.ChangedNodeIDs) > MaxChangedNodeIDs {
		e.ChangedNodeIDs = e.ChangedNodeIDs[:MaxChangedNodeIDs]
		e.Truncated = true
	}
	if e.ChangedNodeIDs == nil {
		e.ChangedNodeIDs = []string{}
	}
}

// RevisionMillis revision（<unix 毫秒>-<4 位 hex>）里的毫秒时间；格式不对返回 false。
// 只用来丢弃乱序到达的旧事件，判「是否变化」一律比字符串相等。
func RevisionMillis(rev string) (int64, bool) {
	head, _, ok := strings.Cut(rev, "-")
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(head, 10, 64)
	return v, err == nil
}

// Older a 是否明确早于 b（两者都能解析出毫秒且 a<b）。
func Older(a, b string) bool {
	x, ok1 := RevisionMillis(a)
	y, ok2 := RevisionMillis(b)
	return ok1 && ok2 && x < y
}

// Publisher 发布端（canvas-rpc）。nil 安全：未配置 Redis 时所有方法都是空操作。
type Publisher struct {
	rdb     redis.UniversalClient
	timeout time.Duration
}

// NewPublisher rdb 为 nil 时返回 nil（不发事件）。
func NewPublisher(rdb redis.UniversalClient) *Publisher {
	if rdb == nil {
		return nil
	}
	return &Publisher{rdb: rdb, timeout: time.Second}
}

// Enabled 是否会真的发。
func (p *Publisher) Enabled() bool { return p != nil && p.rdb != nil }

// Publish 发一条事件（与调用方的取消解耦，独立 1 秒时限）。失败只返回错误，调用方记日志，不影响业务结果。
func (p *Publisher) Publish(ctx context.Context, e Event) error {
	if !p.Enabled() || e.CanvasID == "" {
		return nil
	}
	e.Clip()
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()
	return p.rdb.Publish(pctx, Channel(e.CanvasID), b).Err()
}
