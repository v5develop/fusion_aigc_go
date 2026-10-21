package media

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// 写一个占 size 字节的缓存文件并登记为已完成的条目。
func seedEntry(t *testing.T, c *inputCache, url string, size int64, refs int, expires time.Time) {
	t.Helper()
	path := filepath.Join(c.root, url+".mp4")
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	c.entries[url] = &cacheEntry{path: path, size: size, ready: ready, refs: refs, expires: expires}
	c.total += size
}

func TestInputCacheEvictsIdleOldestFirstAndKeepsInUse(t *testing.T) {
	c := newInputCache(t.TempDir(), time.Minute, 100, nil)
	now := time.Now()
	seedEntry(t, c, "a", 60, 1, now.Add(time.Minute)) // 正在使用：最早过期也不能删
	seedEntry(t, c, "b", 60, 0, now.Add(2*time.Minute))
	seedEntry(t, c, "c", 60, 0, now.Add(3*time.Minute))

	c.mu.Lock()
	c.evictLocked()
	c.mu.Unlock()

	if _, ok := c.entries["a"]; !ok {
		t.Fatal("in-use entry must not be evicted")
	}
	if _, ok := c.entries["b"]; ok {
		t.Fatal("oldest idle entry should be evicted first")
	}
	if _, ok := c.entries["c"]; ok {
		t.Fatal("second idle entry should be evicted to get under the cap")
	}
	if c.total != 60 {
		t.Fatalf("total = %d, want 60 (only the in-use entry left)", c.total)
	}
}

func TestRemoveOrphanCachesKeepsLiveOwners(t *testing.T) {
	root := t.TempDir()
	mk := func(name, owner string, age time.Duration) string {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, ownerFile), []byte(owner), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	dead := mk("media-cache-dead", "999999", 2*time.Minute)
	live := mk("media-cache-live", strconv.Itoa(os.Getpid()), 2*time.Minute)
	fresh := mk("media-cache-fresh", "999999", 0)

	removeOrphanCaches(root)

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("dead owner's cache directory should be removed")
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal("live owner's cache directory must be kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("recently created directory must be kept")
	}
}

// 空 TempDir 时清扫的必须是系统临时目录，绝不能是当前工作目录。
func TestRemoveOrphanCachesEmptyRootUsesTempDirNotCWD(t *testing.T) {
	tmp := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Chdir(cwd)

	mk := func(base, owner string, age time.Duration) string {
		dir := filepath.Join(base, "media-cache-x")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, ownerFile), []byte(owner), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	inCWD := mk(cwd, "999999", 2*time.Minute)
	inTmp := mk(tmp, "999999", 2*time.Minute)

	removeOrphanCaches("")

	if _, err := os.Stat(inCWD); err != nil {
		t.Fatal("orphan-looking directory in the working directory must not be touched")
	}
	if _, err := os.Stat(inTmp); !os.IsNotExist(err) {
		t.Fatal("orphan cache in os.TempDir() should be removed when TempDir is empty")
	}
}

// 只删带前缀的目录：同名普通文件、无前缀的目录、符号链接都不动。
func TestRemoveOrphanCachesOnlyTouchesPrefixedDirectories(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-2 * time.Minute)

	file := filepath.Join(root, "media-cache-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(file, old, old)

	other := filepath.Join(root, "other-dir")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(other, ownerFile), []byte("999999"), 0o600)
	_ = os.Chtimes(other, old, old)

	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(target, ownerFile), []byte("999999"), 0o600)
	link := filepath.Join(root, "media-cache-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	removeOrphanCaches(root)

	for _, p := range []string{file, other, target} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must not be removed: %v", p, err)
		}
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink must not be removed")
	}
}
