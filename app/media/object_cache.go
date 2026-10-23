package media

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// objectCache 按 rid 缓存本实例读到（或刚写入）的对象字节，让 mr:// 引用在本机命中时不再从对象存储下载。
// 与 inputCache（按 URL 缓存输入视频）是两套独立的缓存，互不影响。
//
// 正确性不依赖缓存（设计文档 §2.3）：
//   - 每次读取都先过 checkReadable（待删除标记，410），再向存储查对象当前的版本（objectVersioner，OSS 为 HEAD 的 ETag）。
//     只有 store、path、版本三者与条目完全一致才用本地文件；不一致、对象已不存在或查询失败都按未命中处理，
//     回落到与原来一模一样的 Fetch。权威状态只在共享桶里，别的实例删除、覆盖都能在下一次读取时看到。
//   - 缓存键是 rid，但 rid 并不严格对应固定字节（同一幂等键在幂等记录过期后重跑、跨路由复用同一个键、
//     清理后用同一键重新入库都会让同一个 rid 指向新字节或新路径），所以条目里存 store/path/版本并逐次核对。
//   - 版本先查再取：版本在 Fetch 之前取得，取到的字节不会比版本旧；对象在两步之间被改写时，
//     条目记的是旧版本，下次核对不一致而失效，不会把新版本号挂到旧字节上。
//
// 容量：实际上限 = min(ObjectCacheMaxBytes, 磁盘可用空间的 objectCacheDiskShare%)；超过时按最近使用时间淘汰空闲条目，
// 正在被任务使用的文件不删（允许暂时超出）；最后一次使用后保留 ttl，由后台清扫删除。单个对象超过上限不缓存。
type objectCache struct {
	mu       sync.Mutex
	root     string // 缓存目录的父目录（TempDir）
	dir      string // 第一次写入时创建
	ttl      time.Duration
	maxBytes int64
	total    int64
	entries  map[string]*objEntry
	now      func() time.Time
}

type objEntry struct {
	store, path, version string
	file                 string
	size                 int64
	refs                 int
	lastUsed             time.Time
	dropped              bool         // 已从表里移除（被替换或删除），最后一个使用者释放时删文件
	known                *cachedFacts // 写穿透时由落桶给出（对这份字节 probe 过、hash 过）；下载回来的条目为 nil
}

// cachedFacts 写穿透条目上已知的事实：sha256 与 probe 得到的媒体元数据，都是对条目文件这份字节算出来的。
// 只在 storeFile（probeMeta）落桶时登记；字体等不经 probe 的写入不登记，读取方照常 probe。
type cachedFacts struct {
	sum  string
	meta MediaMeta
}

// objectCacheDiskShare 对象缓存最多占用磁盘可用空间的比例（百分比）。与输入缓存的 30% 合计不超过一半。
const objectCacheDiskShare = 20

// objectCacheDirPrefix 对象缓存目录的前缀。带 cacheDirPrefix，所以启动时 removeOrphanCaches 一并清理崩溃残留。
const objectCacheDirPrefix = cacheDirPrefix + "obj-"

// objectVersioner 可选的存储能力：返回对象当前的字节数与版本标识。版本为空表示不可比较，该对象不进缓存。
// 不实现它的存储不启用对象缓存（每次读取都 Fetch，与原来一致）。
type objectVersioner interface {
	Version(ctx context.Context, store, path string) (size int64, version string, err error)
}

func newObjectCache(root string, ttl time.Duration, maxBytes int64) *objectCache {
	return &objectCache{root: root, ttl: ttl, maxBytes: maxBytes, entries: make(map[string]*objEntry), now: time.Now}
}

// acquire 条目与 store/path/版本完全一致时返回本地文件与释放函数；否则 ok=false。
// 不一致的旧条目在这里移除（字节已过时）。
func (c *objectCache) acquire(rid, store, path, version string) (string, func(), bool) {
	file, release, _, ok := c.acquireFacts(rid, store, path, version)
	return file, release, ok
}

// acquireFacts 同 acquire，另外返回条目上已知的事实（没有为 nil）。
func (c *objectCache) acquireFacts(rid, store, path, version string) (string, func(), *cachedFacts, bool) {
	if c == nil || version == "" {
		return "", nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[rid]
	if !ok {
		return "", nil, nil, false
	}
	if e.store != store || e.path != path || e.version != version {
		c.dropLocked(rid, e)
		return "", nil, nil, false
	}
	e.refs++
	e.lastUsed = c.now()
	var once sync.Once
	return e.file, func() { once.Do(func() { c.release(e) }) }, e.known, true
}

func (c *objectCache) release(e *objEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.refs--
	if e.refs < 0 {
		e.refs = 0
	}
	e.lastUsed = c.now()
	if e.refs == 0 && e.dropped {
		_ = os.Remove(e.file)
		c.total -= e.size
	}
}

// insert 把本地文件 src 登记为 rid 的缓存（硬链接，跨文件系统时复制；先写临时名再 rename）。
// 失败只记日志：缓存只是加速。src 不会被修改。
func (c *objectCache) insert(rid, store, path, version, src string) {
	c.insertFacts(rid, store, path, version, src, nil)
}

// insertFacts 同 insert，并登记这份字节已知的事实（known 为 nil 即不登记）。
func (c *objectCache) insertFacts(rid, store, path, version, src string, known *cachedFacts) {
	if c == nil || version == "" {
		return
	}
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	size := info.Size()
	if c.maxBytes <= 0 || size > c.maxBytes {
		return
	}
	c.mu.Lock()
	if e, ok := c.entries[rid]; ok && e.store == store && e.path == path && e.version == version {
		e.lastUsed = c.now()
		if e.known == nil {
			e.known = known
		}
		c.mu.Unlock()
		return
	}
	if err := c.ensureDirLocked(); err != nil {
		c.mu.Unlock()
		log.Printf("media: 对象缓存目录不可用（只影响缓存）: %v", err)
		return
	}
	dir := c.dir
	c.mu.Unlock()

	final := filepath.Join(dir, randomName()+"-"+filepath.Base(path))
	if err := linkOrCopyAtomic(src, final); err != nil {
		log.Printf("media: 写入对象缓存失败（只影响缓存）: %v", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[rid]; ok {
		if old.store == store && old.path == path && old.version == version {
			// 并发的另一次写入已经登记了同一版本：用它的，删自己的
			_ = os.Remove(final)
			old.lastUsed = c.now()
			if old.known == nil {
				old.known = known
			}
			return
		}
		c.dropLocked(rid, old)
	}
	c.entries[rid] = &objEntry{store: store, path: path, version: version, file: final, size: size, lastUsed: c.now(), known: known}
	c.total += size
	c.evictLocked()
}

// forget 移除 rid 的条目（本实例删除对象时调用；别的实例删除靠读取时核对版本与待删除标记）。
func (c *objectCache) forget(rid string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[rid]; ok {
		c.dropLocked(rid, e)
	}
}

// dropLocked 从表里移除条目；没人在用就立刻删文件，否则等最后一个使用者释放。调用方必须持有 c.mu。
func (c *objectCache) dropLocked(rid string, e *objEntry) {
	delete(c.entries, rid)
	e.dropped = true
	if e.refs == 0 {
		_ = os.Remove(e.file)
		c.total -= e.size
	}
}

// sweep 删除空闲超过 ttl 的条目，再按上限淘汰。后台定时调用。
func (c *objectCache) sweep(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for rid, e := range c.entries {
		if e.refs == 0 && now.Sub(e.lastUsed) > c.ttl {
			c.dropLocked(rid, e)
		}
	}
	c.evictLocked()
}

// evictLocked 超过上限时按最近使用时间从旧到新淘汰空闲条目。调用方必须持有 c.mu。
func (c *objectCache) evictLocked() {
	if c.total <= c.maxBytes {
		return
	}
	idle := make([]string, 0, len(c.entries))
	for rid, e := range c.entries {
		if e.refs == 0 {
			idle = append(idle, rid)
		}
	}
	sort.Slice(idle, func(i, j int) bool {
		return c.entries[idle[i]].lastUsed.Before(c.entries[idle[j]].lastUsed)
	})
	for _, rid := range idle {
		if c.total <= c.maxBytes {
			return
		}
		c.dropLocked(rid, c.entries[rid])
	}
}

// ensureDirLocked 第一次写入时建缓存目录并登记属主进程（与 inputCache 同一套孤儿清理规则）。调用方必须持有 c.mu。
func (c *objectCache) ensureDirLocked() error {
	if c.dir != "" {
		return nil
	}
	dir, err := os.MkdirTemp(c.root, objectCacheDirPrefix)
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

// linkOrCopyAtomic 在 to 所在目录建临时名（硬链接，失败则复制），再 rename 到 to。读者只会看到完整文件。
func linkOrCopyAtomic(from, to string) error {
	tmp := filepath.Join(filepath.Dir(to), ".tmp-"+randomName())
	if err := os.Link(from, tmp); err != nil {
		if err := streamCopyFile(from, tmp); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, to); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func streamCopyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// readObject 按 rid 读对象到本地（只读路径）。调用方必须先过 checkReadable（待删除 410）。
// 命中条件：存储能给出版本、请求在任务上下文里（有 pins）、条目的 store/path/版本与存储当前状态一致。
// 其余一律走 fetchObject（与没有缓存时完全一样，含错误语义），取回后登记缓存。
func (s *Server) readObject(ctx context.Context, rid, store, path, dir string) (string, error) {
	local, _, err := s.readObjectAt(ctx, rid, store, path, dir, nil)
	return local, err
}

// sourceVersion 调用方刚刚 HEAD 过源对象时拿到的版本（见 resolveSourceStore），readObjectAt 直接用，不再 HEAD 第二次。
type sourceVersion struct {
	version string
}

// readObjectAt 同 readObject；known 非 nil 时用它作为存储当前的版本（调用方在同一请求里刚 HEAD 过，
// 仍满足"版本先查再取"），否则自己查。命中写穿透条目时一并返回条目上已知的 sha256 与元数据（可能为 nil）。
func (s *Server) readObjectAt(ctx context.Context, rid, store, path, dir string, known *sourceVersion) (string, *cachedFacts, error) {
	v, versioned := s.objects.(objectVersioner)
	pn, pinned := ctx.Value(pinKey{}).(*pins)
	if s.objCache == nil || !versioned || !pinned {
		local, err := s.fetchObject(ctx, store, path, dir)
		return local, nil, err
	}
	var version string
	if known != nil {
		version = known.version
	} else {
		_, ver, err := v.Version(ctx, store, path)
		if err != nil {
			if errors.Is(err, errObjectNotFound) {
				s.objCache.forget(rid)
			}
			local, err := s.fetchObject(ctx, store, path, dir)
			return local, nil, err
		}
		version = ver
	}
	if file, release, facts, hit := s.objCache.acquireFacts(rid, store, path, version); hit {
		pn.add(release)
		return file, facts, nil
	}
	local, err := s.fetchObject(ctx, store, path, dir)
	if err != nil {
		return "", nil, err
	}
	s.objCache.insert(rid, store, path, version, local)
	return local, nil, nil
}

// normalizeETag 去掉 ETag 两侧的引号与弱校验前缀，便于 PUT 响应与 HEAD 响应对比。
func normalizeETag(etag string) string {
	return strings.Trim(strings.TrimPrefix(strings.TrimSpace(etag), "W/"), `"`)
}
