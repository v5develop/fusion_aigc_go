package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// inputCache 按 URL 缓存下载下来的输入视频。同一个视频会被多个阶段用到（封面、探测、抽帧、合成……），
// 第一次下载后保留一段时间，之后的请求直接用本地文件，不再重复下载。
//
// 清理规则：
//   - 有请求正在用的文件（refs > 0）永远不删；
//   - 最后一个使用者释放后，再保留 ttl（默认 5 分钟），到期由后台清扫删除；
//   - 同一个 URL 并发请求只下载一次，其余等待第一次下载的结果；
//   - 总字节超过 maxBytes 时，先淘汰空闲且最早过期的文件（正在使用的不删，允许暂时超出）。
type inputCache struct {
	mu       sync.Mutex
	root     string // 缓存目录的父目录（TempDir）；真正的目录在第一次使用时创建
	dir      string
	fetch    *fetcher
	ttl      time.Duration
	maxBytes int64
	total    int64 // 已下载完成的条目占用的字节数
	entries  map[string]*cacheEntry
}

type cacheEntry struct {
	path    string
	size    int64
	err     error
	ready   chan struct{} // 下载完成后关闭
	refs    int
	expires time.Time // 没有使用者时的到期时间
}

func newInputCache(root string, ttl time.Duration, maxBytes int64, f *fetcher) *inputCache {
	return &inputCache{root: root, ttl: ttl, maxBytes: maxBytes, fetch: f, entries: make(map[string]*cacheEntry)}
}

// acquire 返回缓存文件的本地路径和释放函数。调用方用完必须调用释放函数。
func (c *inputCache) acquire(ctx context.Context, url string) (string, func(), error) {
	return c.acquireWith(ctx, url, nil)
}

// acquireWith 同 acquire，带下载头。缓存键包含头摘要，带头与不带头的下载互不命中。
func (c *inputCache) acquireWith(ctx context.Context, url string, headers http.Header) (string, func(), error) {
	key := cacheKeyFor(url, headers)
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		e.refs++
		c.mu.Unlock()
		return c.wait(ctx, e)
	}
	if err := c.ensureDir(); err != nil {
		c.mu.Unlock()
		return "", nil, internal(err)
	}
	e := &cacheEntry{ready: make(chan struct{}), refs: 1}
	c.entries[key] = e
	dir := c.dir
	c.mu.Unlock()

	name := randomName() + ".mp4"
	path, err := c.fetch.downloadWithHeaders(ctx, url, headers, dir, name)

	c.mu.Lock()
	e.path, e.err = path, err
	close(e.ready)
	if err != nil {
		// 失败的下载不缓存：删掉可能写了一半的文件，下次重新下载
		delete(c.entries, key)
		_ = os.Remove(filepath.Join(dir, name))
		e.refs = 0
		c.mu.Unlock()
		return "", nil, err
	}
	if info, statErr := os.Stat(path); statErr == nil {
		e.size = info.Size()
		c.total += e.size
	}
	c.evictLocked() // e 自己 refs > 0，不会被淘汰
	c.mu.Unlock()
	return c.wait(ctx, e)
}

// wait 等下载完成（已完成则立即返回）。调用时 refs 已经由 acquire 加过一次。
func (c *inputCache) wait(ctx context.Context, e *cacheEntry) (string, func(), error) {
	select {
	case <-e.ready:
	case <-ctx.Done():
		c.release(e)
		return "", nil, unavailable("等待下载超时，请稍后重试")
	}
	if e.err != nil {
		c.release(e)
		return "", nil, e.err
	}
	var once sync.Once
	release := func() { once.Do(func() { c.release(e) }) }
	return e.path, release, nil
}

func (c *inputCache) release(e *cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	if e.refs <= 0 {
		e.refs = 0
		e.expires = time.Now().Add(c.ttl)
	}
}

// sweep 删除已经没人用、且超过保留时间的文件，并在超过字节上限时继续淘汰。后台每 30 秒调用一次。
func (c *inputCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for url, e := range c.entries {
		if e.refs > 0 || e.err != nil || !now.After(e.expires) {
			continue
		}
		c.removeLocked(url, e)
	}
	c.evictLocked()
}

// evictLocked 超过字节上限时，按过期时间从早到晚淘汰空闲条目。调用方必须持有 c.mu。
func (c *inputCache) evictLocked() {
	// maxBytes <= 0 表示不限制（测试与未配置时）
	if c.maxBytes <= 0 || c.total <= c.maxBytes {
		return
	}
	idle := make([]string, 0, len(c.entries))
	for url, e := range c.entries {
		if e.refs == 0 && e.err == nil && e.path != "" {
			idle = append(idle, url)
		}
	}
	sort.Slice(idle, func(i, j int) bool {
		return c.entries[idle[i]].expires.Before(c.entries[idle[j]].expires)
	})
	for _, url := range idle {
		if c.total <= c.maxBytes {
			break
		}
		c.removeLocked(url, c.entries[url])
	}
}

// removeLocked 删除一个空闲条目的文件与记录。调用方必须持有 c.mu，且 e.refs == 0。
func (c *inputCache) removeLocked(url string, e *cacheEntry) {
	_ = os.Remove(e.path)
	delete(c.entries, url)
	c.total -= e.size
}

// ownerFile 记录缓存目录的属主进程号，启动时用来判断目录是否还有人在用。
const ownerFile = "owner"

// ensureDir 第一次使用时建缓存目录并登记属主。调用方必须持有 c.mu。
func (c *inputCache) ensureDir() error {
	if c.dir != "" {
		return nil
	}
	dir, err := os.MkdirTemp(c.root, cacheDirPrefix)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	c.dir = dir
	return nil
}

// cacheDirPrefix 缓存目录的前缀。清扫只认这个前缀，避免误删 TempDir 里别人的东西。
const cacheDirPrefix = "media-cache-"

// removeOrphanCaches 启动时清掉属主进程已经不在的缓存目录（崩溃、被 kill -9 之后残留的）。
// root 为空时用系统临时目录（os.TempDir），绝不扫描当前工作目录。
// 只处理：名字带 media-cache- 前缀、是真正的目录（不跟随符号链接）、mtime 超过一分钟、
// 且 owner 文件记录的属主进程已经不存在。属主还活着的目录不动（同机多实例共用 TempDir 时的情况）；
// 刚创建不久的目录不动（可能正在被登记）；读不到属主的目录也不动（无法证明已经没人用）。
func removeOrphanCaches(root string) {
	if root == "" {
		root = os.TempDir()
	}
	matches, err := filepath.Glob(filepath.Join(root, cacheDirPrefix+"*"))
	if err != nil {
		return
	}
	for _, dir := range matches {
		if !strings.HasPrefix(filepath.Base(dir), cacheDirPrefix) {
			continue
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || time.Since(info.ModTime()) < time.Minute {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ownerFile))
		if err != nil {
			continue
		}
		pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if convErr != nil || pid <= 0 || processAlive(pid) {
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// processAlive 发 0 号信号探测进程是否存在；EPERM 表示进程存在但不属于本用户，也算活着。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pins 一次任务里借到的缓存文件。任务结束时统一释放，保证任务期间文件不会被清掉。
type pinKey struct{}

type pins struct {
	mu       sync.Mutex
	releases []func()
}

func (p *pins) add(release func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releases = append(p.releases, release)
}

func (p *pins) releaseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.releases {
		r()
	}
	p.releases = nil
}

// fetchCached 取 URL 对应的本地文件（命中缓存则直接用），并把引用挂到当前任务上。
func (s *Service) fetchCached(ctx context.Context, url string, headers http.Header) (string, error) {
	p, ok := ctx.Value(pinKey{}).(*pins)
	if !ok {
		return "", internal(errors.New("缺少任务上下文，无法借用缓存文件"))
	}
	path, release, err := s.cache.acquireWith(ctx, url, headers)
	if err != nil {
		return "", err
	}
	p.add(release)
	return path, nil
}

func randomName() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
