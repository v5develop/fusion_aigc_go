package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// 同一个视频在多个阶段使用：只下载一次。
func TestInputCacheReusesDownloadAcrossStages(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeFile(w, r, clip)
	}))
	t.Cleanup(origin.Close)

	svc := NewService(Config{MaxConcurrent: 2, MaxInputBytes: 50 << 20, MaxComposeInputs: 5,
		OpTimeoutSeconds: 120, FetchTimeoutSeconds: 60, TempDir: t.TempDir(), FFmpegThreads: 1,
		AllowPrivateHosts: true, InputCacheMinutes: 5})
	url := origin.URL + "/a.mp4"

	// 第一阶段：抽封面
	ctx1, dir1, done1, err := svc.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p1, err := svc.FromURL(url)(ctx1, dir1)
	if err != nil {
		t.Fatal(err)
	}
	done1()

	// 第二阶段：探测，同一个 URL
	ctx2, dir2, done2, err := svc.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := svc.FromURL(url)(ctx2, dir2)
	if err != nil {
		t.Fatal(err)
	}
	done2()

	if hits.Load() != 1 {
		t.Fatalf("源站被请求 %d 次，同一视频应只下载一次", hits.Load())
	}
	if p1 != p2 {
		t.Fatalf("两阶段拿到的不是同一个缓存文件: %s / %s", p1, p2)
	}
}

// 任务期间文件不会被清掉；任务结束后保留 ttl，到期才删。
func TestInputCacheKeepsInUseThenExpires(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	origin := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(origin.Close)

	svc := NewService(Config{MaxConcurrent: 2, MaxInputBytes: 50 << 20, MaxComposeInputs: 5,
		OpTimeoutSeconds: 120, FetchTimeoutSeconds: 60, TempDir: t.TempDir(), FFmpegThreads: 1,
		AllowPrivateHosts: true, InputCacheMinutes: 5})
	svc.cache.ttl = 5 * time.Minute

	ctx, jobDir, done, err := svc.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path, err := svc.FromURL(origin.URL+"/a.mp4")(ctx, jobDir)
	if err != nil {
		t.Fatal(err)
	}

	// 任务还在用：即使过了很久也不能删
	svc.cache.sweep(time.Now().Add(time.Hour))
	if _, err := os.Stat(path); err != nil {
		t.Fatal("任务进行中的文件被清掉了")
	}

	done() // 任务结束，开始计时

	// 刚结束：保留期内不删
	svc.cache.sweep(time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Fatal("保留期内的文件被提前清掉了")
	}

	// 超过保留期：删除
	svc.cache.sweep(time.Now().Add(6 * time.Minute))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("超过保留期的文件没有被清掉")
	}
}
