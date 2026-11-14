package canvasevent

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// Hub 订阅端（网关）：每个网关实例只占一条 Redis pub/sub 连接，按画布引用计数地 SUBSCRIBE / UNSUBSCRIBE，
// 收到的消息扇出给本实例上订阅该画布的 SSE 连接。比「每个 SSE 一条 Subscribe」少占 Redis 连接（go-redis 的 PubSub 各占一条专用连接）。
type Hub struct {
	ps *redis.PubSub

	mu   sync.Mutex
	subs map[string]map[*Sub]struct{} // channel → 订阅者

	closed atomic.Bool
	done   chan struct{}
}

// Sub 一个订阅者（一条 SSE 连接）。C 有缓冲；消费太慢时新消息丢弃并置 Lost，由消费方用 revision 对账补齐。
type Sub struct {
	C        chan []byte
	channel  string
	lost     atomic.Bool
	hub      *Hub
	released atomic.Bool
}

// TakeLost 是否丢过消息（读后清零）。
func (s *Sub) TakeLost() bool { return s.lost.Swap(false) }

// Close 退订（幂等）。
func (s *Sub) Close() {
	if s == nil || !s.released.CompareAndSwap(false, true) {
		return
	}
	s.hub.remove(s)
}

// subBuffer 每个订阅者的缓冲条数。
const subBuffer = 16

// NewHub rdb 为 nil 时返回 nil（网关降级为轮询）。
func NewHub(rdb redis.UniversalClient) *Hub {
	if rdb == nil {
		return nil
	}
	h := &Hub{ps: rdb.Subscribe(context.Background()), subs: map[string]map[*Sub]struct{}{}, done: make(chan struct{})}
	go h.loop()
	return h
}

func (h *Hub) loop() {
	defer close(h.done)
	for msg := range h.ps.Channel() {
		h.mu.Lock()
		for s := range h.subs[msg.Channel] {
			select {
			case s.C <- []byte(msg.Payload):
			default:
				s.lost.Store(true)
			}
		}
		h.mu.Unlock()
	}
}

// Subscribe 订阅一张画布（首个订阅者才真正 SUBSCRIBE）。返回后即可收到之后发布的事件。
func (h *Hub) Subscribe(ctx context.Context, canvasID string) (*Sub, error) {
	ch := Channel(canvasID)
	s := &Sub{C: make(chan []byte, subBuffer), channel: ch, hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[ch]
	if set == nil {
		// 持锁 SUBSCRIBE：与同频道的并发 remove（UNSUBSCRIBE）串行，避免「刚订上就被退订」
		if err := h.ps.Subscribe(ctx, ch); err != nil {
			return nil, err
		}
		set = map[*Sub]struct{}{}
		h.subs[ch] = set
	}
	set[s] = struct{}{}
	return s, nil
}

func (h *Hub) remove(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[s.channel]
	delete(set, s)
	if len(set) == 0 && set != nil {
		delete(h.subs, s.channel)
		if !h.closed.Load() {
			if err := h.ps.Unsubscribe(context.Background(), s.channel); err != nil {
				logx.Errorf("canvasevent: UNSUBSCRIBE %s 失败: %v", s.channel, err)
			}
		}
	}
}

// Channels 当前订阅中的频道数（测试 / 指标用）。
func (h *Hub) Channels() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close 关掉 pub/sub 连接（进程退出时）。
func (h *Hub) Close() error {
	if h == nil || !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := h.ps.Close()
	<-h.done
	return err
}
