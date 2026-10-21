package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 同一个 URL 被并发请求时只下载一次，所有请求拿到同一个本地文件（单飞行）。
func TestInputCacheSingleFlightDownloadsOnce(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	var hits atomic.Int32
	release := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release // 让所有并发请求都在下载进行中到达缓存
		http.ServeFile(w, r, clip)
	}))
	t.Cleanup(origin.Close)

	c := newInputCache(t.TempDir(), 5*time.Minute, 0, newFetcher(Config{AllowPrivateHosts: true,
		FetchTimeoutSeconds: 60, MaxInputBytes: 50 << 20}))
	url := origin.URL + "/a.mp4"

	const n = 8
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, done, err := c.acquire(context.Background(), url)
			errs[i] = err
			if err == nil {
				paths[i] = p
				defer done()
			}
		}(i)
	}
	time.Sleep(200 * time.Millisecond) // 给所有 goroutine 进入 acquire 的时间
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("请求 %d 失败: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("并发请求应只下载一次，源站收到 %d 次", got)
	}
	for i := 1; i < n; i++ {
		if paths[i] != paths[0] {
			t.Fatalf("所有请求应拿到同一个文件，got %s vs %s", paths[i], paths[0])
		}
	}
}
