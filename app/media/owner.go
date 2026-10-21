package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log"
	"sync"
	"time"
)

// ownerCacheTTL 存活实例列表的缓存时间：资源 ID 的创建不必每次都查注册中心，成员变化后约一个周期内生效。
const ownerCacheTTL = 5 * time.Second

// ownerRetryBackoff 注册中心查询失败后，在这段时间内不再重查，直接用上一次的结果（或只有本实例）。
const ownerRetryBackoff = 5 * time.Second

// ownerPicker 为新资源选择所属实例（设计 §5）：在存活实例的 ID 集合上做 rendezvous 哈希，按幂等键选出 owner。
// 同一个幂等键在成员不变时总得到同一个 owner，所以重试得到同一个 rid；成员增减时只有映射到变动实例的键会换 owner。
type ownerPicker struct {
	self string
	// list 查询存活实例的 ID；为 nil 表示没有注册中心，owner 恒为本实例
	list func(ctx context.Context) ([]string, error)

	// now 时钟，测试用；为 nil 时用 time.Now
	now func() time.Time

	mu         sync.Mutex
	at         time.Time
	ids        []string
	refreshing bool      // 有一个请求正在锁外查询注册中心，其它请求先用缓存结果
	retryAt    time.Time // 查询失败后的退避截止时间
}

func (p *ownerPicker) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// ownerFor 返回幂等键对应的 owner 实例 ID。
func (p *ownerPicker) ownerFor(ctx context.Context, key string) string {
	return pickOwner(p.candidates(ctx), key)
}

// candidates 候选 owner：注册中心里健康的实例 ID，加上本实例（本实例可能还没注册完成）。
// 注册中心查询在锁外进行，查询期间其它请求拿到的是缓存结果（不排队等 I/O）。
// 查询失败后进入退避，退避期间不重查；失败后沿用上一次的结果，从未成功过则只有本实例。
func (p *ownerPicker) candidates(ctx context.Context) []string {
	if p.list == nil {
		return []string{p.self}
	}
	p.mu.Lock()
	now := p.clock()
	fresh := p.ids != nil && now.Sub(p.at) < ownerCacheTTL
	if fresh || p.refreshing || now.Before(p.retryAt) {
		ids := p.currentLocked()
		p.mu.Unlock()
		return ids
	}
	p.refreshing = true
	p.mu.Unlock()

	live, err := p.list(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshing = false
	if err != nil {
		p.retryAt = p.clock().Add(ownerRetryBackoff)
		log.Printf("media: 查询注册中心存活实例失败，%s 内不再重查: %v", ownerRetryBackoff, err)
		return p.currentLocked()
	}
	p.ids = p.merge(live)
	p.at = now
	p.retryAt = time.Time{}
	return p.ids
}

// currentLocked 当前可用的候选：有缓存用缓存，否则只有本实例。调用方必须持有 p.mu。
func (p *ownerPicker) currentLocked() []string {
	if p.ids == nil {
		return []string{p.self}
	}
	return p.ids
}

// merge 本实例加上查询结果（去重，本实例排第一）。
func (p *ownerPicker) merge(live []string) []string {
	seen := map[string]bool{p.self: true}
	ids := []string{p.self}
	for _, id := range live {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// pickOwner 在候选 ID 中按 sha256(id|key) 取最大者（rendezvous 哈希）。候选为空时返回空串。
func pickOwner(ids []string, key string) string {
	var best string
	var bestScore []byte
	for _, id := range ids {
		sum := sha256.Sum256([]byte(id + "|" + key))
		if bestScore == nil || bytes.Compare(sum[:], bestScore) > 0 {
			best, bestScore = id, sum[:]
		}
	}
	return best
}

// newOwnerPicker 配置了注册中心时，候选 owner 来自注册中心的存活实例；否则只有本实例。
// 注册中心客户端创建失败不致命：退回只有本实例（与注册失败不致命的口径一致）。
func newOwnerPicker(cfg Config, self string, closers *[]func()) *ownerPicker {
	p := &ownerPicker{self: self}
	if cfg.Registry.ServerAddr == "" {
		return p
	}
	rc, err := newRegistryClients(cfg.Registry)
	if err != nil {
		log.Printf("media: 归属选择无法连接注册中心，只用本实例作 owner: %v", err)
		return p
	}
	*closers = append(*closers, rc.close)
	p.list = func(ctx context.Context) ([]string, error) {
		live, err := liveInstances(rc.naming, cfg.Registry)
		if err != nil {
			return nil, err
		}
		var ids []string
		for _, in := range live {
			if id := in.Metadata[instanceIDMetaKey]; id != "" {
				ids = append(ids, id)
			}
		}
		return ids, nil
	}
	return p
}
