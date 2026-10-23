package media

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingStore 统计数据对象的 Fetch 次数（索引读取不计），用来证明命中缓存时没有下载。
type countingStore struct {
	*LocalStore
	fetches atomic.Int32
	puts    atomic.Int32 // 数据对象的 Put 次数（索引写入不计），用来证明被拒的请求没有落桶

	headMu sync.Mutex
	heads  map[string]int // 数据对象的 HEAD 次数（Head 与 Version 都是一次 HEAD），按 store 计；待删除标记、索引不计
}

func (c *countingStore) countHead(store, path string) {
	if classOfPath(path) == classIndex {
		return
	}
	c.headMu.Lock()
	defer c.headMu.Unlock()
	if c.heads == nil {
		c.heads = map[string]int{}
	}
	c.heads[store]++
}

func (c *countingStore) headCounts() map[string]int {
	c.headMu.Lock()
	defer c.headMu.Unlock()
	out := map[string]int{}
	for k, v := range c.heads {
		out[k] = v
	}
	return out
}

func (c *countingStore) resetHeads() {
	c.headMu.Lock()
	defer c.headMu.Unlock()
	c.heads = nil
}

func (c *countingStore) Head(ctx context.Context, store, path string) (int64, error) {
	c.countHead(store, path)
	return c.LocalStore.Head(ctx, store, path)
}

func (c *countingStore) Version(ctx context.Context, store, path string) (int64, string, error) {
	c.countHead(store, path)
	return c.LocalStore.Version(ctx, store, path)
}

func (c *countingStore) Fetch(ctx context.Context, store, path, dir string) (string, error) {
	if classOfPath(path) != classIndex {
		c.fetches.Add(1)
	}
	return c.LocalStore.Fetch(ctx, store, path, dir)
}

func (c *countingStore) Put(ctx context.Context, store, path, localPath, mime string) (StoredObject, error) {
	if classOfPath(path) != classIndex {
		c.puts.Add(1)
	}
	return c.LocalStore.Put(ctx, store, path, localPath, mime)
}

// newCachedServer 一个媒体实例；多个实例传同一个 objectsDir 即共享"桶"（模拟多实例部署），各自有独立的本机缓存。
func newCachedServer(t *testing.T, objectsDir string, cacheBytes int64) (*Server, *countingStore) {
	t.Helper()
	srv, err := NewServer(Config{
		Token: testToken, InstanceID: "tst001", IdentitySecret: testSecret,
		MaxConcurrent: 2, MaxInputBytes: 50 << 20, MaxComposeInputs: 5,
		FetchTimeoutSeconds: 60, OpTimeoutSeconds: 120, TempDir: t.TempDir(), ObjectDir: objectsDir,
		Idempotency:         IdempotencyConfig{Store: IdemStoreMemory},
		ObjectCacheMaxBytes: cacheBytes, ObjectCacheMinutes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	cs := &countingStore{LocalStore: srv.objects.(*LocalStore)}
	srv.objects = cs
	return srv, cs
}

// putObject 经 storeWithMeta 落桶（与真实写入同一路径，含写穿透），返回 rid。
func putObject(t *testing.T, srv *Server, idemKey string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := srv.storeWithMeta(withIdemKey(context.Background(), idemKey), "intermediate", p,
		MediaMeta{Mime: "font/ttf", Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	return res.Object.RID
}

// readRID 在任务上下文里按 rid 读取（与处理接口的 sourceFn 同一路径）。
func readRID(t *testing.T, srv *Server, rid string) ([]byte, error) {
	t.Helper()
	ctx, dir, done, err := srv.svc.begin(context.Background())
	if err != nil {
		return nil, err
	}
	defer done()
	local, err := srv.sourceFn(SourceRef{Rid: rid})(ctx, dir)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(local)
}

func mustRead(t *testing.T, srv *Server, rid string, want []byte) {
	t.Helper()
	got, err := readRID(t, srv, rid)
	if err != nil {
		t.Fatalf("读取 %s: %v", rid, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("读到的字节不对: got %q want %q", got, want)
	}
}

// 读取：写穿透命中、别的实例写入后首次未命中再命中、缓存关闭时每次都下载。
func TestObjectCacheReadPaths(t *testing.T) {
	data := []byte("object-bytes-v1")
	cases := []struct {
		name        string
		cacheBytes  int64
		sameWriter  bool // true：同一实例写入（写穿透）；false：别的实例写入
		reads       int
		wantFetches int32
	}{
		{name: "同机写入后读取命中写穿透", cacheBytes: 1 << 20, sameWriter: true, reads: 3, wantFetches: 0},
		{name: "别的实例写入首次未命中回落下载之后命中", cacheBytes: 1 << 20, sameWriter: false, reads: 3, wantFetches: 1},
		{name: "缓存关闭每次都下载", cacheBytes: 0, sameWriter: true, reads: 3, wantFetches: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := filepath.Join(t.TempDir(), "objects")
			reader, counter := newCachedServer(t, objects, tc.cacheBytes)
			writer := reader
			if !tc.sameWriter {
				writer, _ = newCachedServer(t, objects, tc.cacheBytes)
			}
			rid := putObject(t, writer, "k-read-"+tc.name, data)
			for i := 0; i < tc.reads; i++ {
				mustRead(t, reader, rid, data)
			}
			if got := counter.fetches.Load(); got != tc.wantFetches {
				t.Fatalf("Fetch 次数 = %d, want %d", got, tc.wantFetches)
			}
		})
	}
}

// 待删除（本实例或别的实例打的标记）的对象即使在本机缓存里也不能读：410，且不下载。
func TestObjectCacheNotServedWhenPendingDelete(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleterIsMe bool
	}{
		{name: "本实例删除", deleterIsMe: true},
		{name: "别的实例删除", deleterIsMe: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := filepath.Join(t.TempDir(), "objects")
			me, counter := newCachedServer(t, objects, 1<<20)
			other, _ := newCachedServer(t, objects, 1<<20)
			data := []byte("to-be-deleted")
			rid := putObject(t, me, "k-pending-"+tc.name, data)
			mustRead(t, me, rid, data) // 确认已在缓存

			deleter := other
			if tc.deleterIsMe {
				deleter = me
			}
			store, path, err := deleter.objectPath(context.Background(), rid)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := deleter.softDelete(context.Background(), store, path); err != nil {
				t.Fatal(err)
			}
			_, err = readRID(t, me, rid)
			var ae *apiError
			if !errors.As(err, &ae) || ae.Code != codeResourceDeleted {
				t.Fatalf("待删除对象必须 410 resource_deleted, got %v", err)
			}
			if got := counter.fetches.Load(); got != 0 {
				t.Fatalf("待删除对象不应下载, Fetch=%d", got)
			}
		})
	}
}

// 别的实例清理（物理删除对象与索引）后，本机缓存与路径缓存都还在：读取必须得到"不存在"，不能用本地副本。
func TestObjectCacheNotServedAfterSweepElsewhere(t *testing.T) {
	objects := filepath.Join(t.TempDir(), "objects")
	me, _ := newCachedServer(t, objects, 1<<20)
	other, _ := newCachedServer(t, objects, 1<<20)
	other.grace = 0
	data := []byte("swept-elsewhere")
	rid := putObject(t, me, "k-swept", data)
	mustRead(t, me, rid, data)

	store, path, err := other.objectPath(context.Background(), rid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.softDelete(context.Background(), store, path); err != nil {
		t.Fatal(err)
	}
	if rep, err := other.sweepOnce(context.Background()); err != nil || rep.Deleted != 1 {
		t.Fatalf("清理失败: %+v %v", rep, err)
	}
	if _, ok := me.paths.get(rid); !ok {
		t.Fatal("前提不成立：本实例的路径缓存应当还在")
	}
	if _, err := readRID(t, me, rid); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("已被物理删除的对象必须读不到, got %v", err)
	}
}

// 同一路径被改写（直传签名地址重用、幂等记录过期后重跑）：版本变了，必须重新下载新字节。
func TestObjectCacheRefetchesWhenObjectRewritten(t *testing.T) {
	objects := filepath.Join(t.TempDir(), "objects")
	me, counter := newCachedServer(t, objects, 1<<20)
	rid := putObject(t, me, "k-rewrite", []byte("version-one"))
	mustRead(t, me, rid, []byte("version-one"))

	store, path, err := me.objectPath(context.Background(), rid)
	if err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(t.TempDir(), "v2")
	if err := os.WriteFile(newer, []byte("version-two-longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 绕过本实例直接改写桶里的对象（模拟别的实例或客户端直传）
	if _, err := NewLocalStoreMust(t, objects).Put(context.Background(), store, path, newer, "font/ttf"); err != nil {
		t.Fatal(err)
	}
	mustRead(t, me, rid, []byte("version-two-longer"))
	mustRead(t, me, rid, []byte("version-two-longer"))
	if got := counter.fetches.Load(); got != 1 {
		t.Fatalf("改写后应只重新下载一次, Fetch=%d", got)
	}
}

func NewLocalStoreMust(t *testing.T, dir string) *LocalStore {
	t.Helper()
	st, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// 并发冷读同一个 rid：都读到完整字节，缓存最终只有一个条目。
func TestObjectCacheConcurrentReaders(t *testing.T) {
	objects := filepath.Join(t.TempDir(), "objects")
	writer, _ := newCachedServer(t, objects, 1<<20)
	reader, _ := newCachedServer(t, objects, 1<<20)
	data := bytes.Repeat([]byte("abcdefgh"), 4096)
	rid := putObject(t, writer, "k-concurrent", data)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := readRID(t, reader, rid)
			if err == nil && !bytes.Equal(got, data) {
				err = errors.New("字节不完整")
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	reader.objCache.mu.Lock()
	n := len(reader.objCache.entries)
	reader.objCache.mu.Unlock()
	if n != 1 {
		t.Fatalf("缓存条目数 = %d, want 1", n)
	}
}

// fakeClockCache 带可控时钟的缓存，用于淘汰与过期测试。
func fakeClockCache(t *testing.T, maxBytes int64) (*objectCache, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	c := newObjectCache(t.TempDir(), time.Minute, maxBytes)
	c.now = func() time.Time { return now }
	return c, &now
}

func srcFile(t *testing.T, n int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// 上限：超过即按最近使用淘汰空闲条目；正在用的不删；单个超过上限的不缓存；空闲超过 ttl 由清扫删除。
func TestObjectCacheEvictionBound(t *testing.T) {
	const path = "v1/work/2026/10/aaaaaaaaaaaaaaaaaaaaaaaaaa.bin"
	c, now := fakeClockCache(t, 10)
	c.insert("a", storePrivate, path, "v", srcFile(t, 4))
	*now = now.Add(time.Second)
	c.insert("b", storePrivate, path, "v", srcFile(t, 4))
	*now = now.Add(time.Second)
	// 用一下 a，让 b 成为最久未用
	fa, rel, ok := c.acquire("a", storePrivate, path, "v")
	if !ok {
		t.Fatal("a 应命中")
	}
	rel()
	fb := c.entries["b"].file
	*now = now.Add(time.Second)
	c.insert("c", storePrivate, path, "v", srcFile(t, 4))
	if _, ok := c.entries["b"]; ok || fileExists(fb) {
		t.Fatal("最久未用的 b 应被淘汰并删除文件")
	}
	if c.total > c.maxBytes || !fileExists(fa) {
		t.Fatalf("总量 %d 超过上限 %d，或 a 被误删", c.total, c.maxBytes)
	}

	// 单个对象超过上限：不缓存
	c.insert("huge", storePrivate, path, "v", srcFile(t, 11))
	if _, ok := c.entries["huge"]; ok {
		t.Fatal("超过上限的对象不应缓存")
	}

	// 正在使用的条目不被淘汰（允许暂时超出）
	_, hold, ok := c.acquire("a", storePrivate, path, "v")
	if !ok {
		t.Fatal("a 应命中")
	}
	c.insert("d", storePrivate, path, "v", srcFile(t, 8))
	if _, ok := c.entries["a"]; !ok || !fileExists(fa) {
		t.Fatal("正在使用的 a 不应被淘汰")
	}

	// 过期清扫：空闲超过 ttl 的删掉，正在用的保留
	*now = now.Add(2 * time.Minute)
	c.sweep(*now)
	if _, ok := c.entries["a"]; !ok {
		t.Fatal("正在使用的 a 不应被过期清扫")
	}
	for _, rid := range []string{"c", "d"} {
		if _, ok := c.entries[rid]; ok {
			t.Fatalf("空闲过期的 %s 应被清扫", rid)
		}
	}
	hold()
	*now = now.Add(2 * time.Minute)
	c.sweep(*now)
	if len(c.entries) != 0 || c.total != 0 || fileExists(fa) {
		t.Fatalf("全部释放并过期后应清空: entries=%d total=%d", len(c.entries), c.total)
	}
}

// 读者持有旧版本时被新版本替换：旧文件保留到最后一个读者释放，读者看到的始终是完整的旧字节。
func TestObjectCacheReplaceWhileInUse(t *testing.T) {
	const path = "v1/work/2026/10/aaaaaaaaaaaaaaaaaaaaaaaaaa.bin"
	c, _ := fakeClockCache(t, 1<<20)
	c.insert("r", storePrivate, path, "v1", srcFile(t, 5))
	old, rel, ok := c.acquire("r", storePrivate, path, "v1")
	if !ok {
		t.Fatal("v1 应命中")
	}
	c.insert("r", storePrivate, path, "v2", srcFile(t, 7))
	fresh, relFresh, ok := c.acquire("r", storePrivate, path, "v2")
	if !ok || fresh == old {
		t.Fatal("新版本应命中且是另一个文件")
	}
	relFresh()
	if !fileExists(old) {
		t.Fatal("读者还在用，旧文件不能删")
	}
	rel()
	if fileExists(old) {
		t.Fatal("最后一个读者释放后旧文件应删除")
	}
	if c.total != 7 {
		t.Fatalf("total = %d, want 7", c.total)
	}
	// 版本不一致的查询会让条目失效（字节已过时）
	if _, _, ok := c.acquire("r", storePrivate, path, "v3"); ok || len(c.entries) != 0 {
		t.Fatal("版本不一致应不命中并移除条目")
	}
}

// 复制：源字节在本机缓存里时不从存储下载；别的实例上首次复制回落下载。删除与用途检查不变。
func TestCopyUsesLocalBytesFromObjectCache(t *testing.T) {
	requireFFmpeg(t)
	clip := filepath.Join(t.TempDir(), "src.mp4")
	makeAVClip(t, clip)
	for _, tc := range []struct {
		name        string
		sameHost    bool
		wantFetches int32
	}{
		{name: "同机写入后复制不下载", sameHost: true, wantFetches: 0},
		{name: "别的实例复制回落下载一次", sameHost: false, wantFetches: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := filepath.Join(t.TempDir(), "objects")
			producer, _ := newCachedServer(t, objects, 1<<30)
			copier, counter := producer, (*countingStore)(nil)
			if tc.sameHost {
				counter = producer.objects.(*countingStore)
			} else {
				copier, counter = newCachedServer(t, objects, 1<<30)
			}
			src, err := producer.storeFile(withIdemKey(context.Background(), "k-copy-src-"+tc.name), "intermediate", clip)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(copier)
			t.Cleanup(ts.Close)
			res := decodeMediaResult(t, postCopy(t, ts.URL, src.Object.RID, "asset_library", "k-copy-"+tc.name))
			if res.Object.Sha256 != src.Object.Sha256 || res.Object.Store != storeAsset {
				t.Fatalf("复制结果不对: %+v", res.Object)
			}
			if got := counter.fetches.Load(); got != tc.wantFetches {
				t.Fatalf("Fetch 次数 = %d, want %d", got, tc.wantFetches)
			}

			// 源进入待删除后，即使字节在缓存里也不能复制（410）
			store, path, err := copier.objectPath(context.Background(), src.Object.RID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := copier.softDelete(context.Background(), store, path); err != nil {
				t.Fatal(err)
			}
			resp := postCopy(t, ts.URL, src.Object.RID, "asset_library", "k-copy-after-delete-"+tc.name)
			assertAPIError(t, resp, 410, codeResourceDeleted)
		})
	}
}
