package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 一条时间戳损坏的待删除标记，不能让整页列举失败，也不能阻止同页其它标记被清理。
// 标记按键排序：损坏的排在最前，后面的仍要被处理。
var corruptScenarioPaths = []string{
	"v1/work/" + strings.Repeat("a", 26), // 损坏
	"v1/work/" + strings.Repeat("b", 26),
	"v1/work/" + strings.Repeat("c", 26),
}

func TestLocalCorruptPendingMarkerDoesNotBlockSweep(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t0 := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	for _, p := range corruptScenarioPaths[1:] {
		if _, err := st.Put(ctx, storePrivate, p, writeFixture(t, "data"), "video/mp4"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreatePending(ctx, storePrivate, p, t0); err != nil {
			t.Fatal(err)
		}
	}
	bad, err := st.pendingFile(storePrivate, corruptScenarioPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not-a-timestamp"), 0o644); err != nil {
		t.Fatal(err)
	}

	marks, _, err := st.ListPending(ctx, storePrivate, "", 10)
	if err != nil {
		t.Fatalf("损坏标记不应让列举失败: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("应只跳过损坏的一条，得到 %d 条", len(marks))
	}

	srv := &Server{objects: st, id: testIdentity(t), now: func() time.Time { return t0.Add(time.Minute) }}
	rep, err := srv.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep 报错: %v", err)
	}
	if rep.Deleted != 2 {
		t.Fatalf("其它两条标记应被清理，Deleted=%d", rep.Deleted)
	}
	for _, p := range corruptScenarioPaths[1:] {
		if _, err := st.Head(ctx, storePrivate, p); err == nil {
			t.Fatalf("%s 应已物理删除", p)
		}
	}
}

func TestOSSCorruptPendingMarkerDoesNotBlockSweep(t *testing.T) {
	st, fake := newTestOSS(t)
	ctx := context.Background()
	t0 := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	for _, p := range corruptScenarioPaths[1:] {
		if _, err := st.Put(ctx, storePrivate, p, writeFixture(t, "data"), "video/mp4"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.CreatePending(ctx, storePrivate, p, t0); err != nil {
			t.Fatal(err)
		}
	}
	// 直接把损坏标记写进假 OSS：对象存在，但到期时间元数据不是 RFC3339
	badKey := "priv/" + pendingPrefix + corruptScenarioPaths[0]
	fake.mu.Lock()
	fake.objs[badKey] = []byte{}
	fake.meta[badKey] = "not-a-timestamp"
	fake.mu.Unlock()

	marks, _, err := st.ListPending(ctx, storePrivate, "", 10)
	if err != nil {
		t.Fatalf("损坏标记不应让列举失败: %v", err)
	}
	if len(marks) != 2 {
		t.Fatalf("应只跳过损坏的一条，得到 %d 条", len(marks))
	}

	srv := &Server{objects: st, id: testIdentity(t), now: func() time.Time { return t0.Add(time.Minute) }}
	rep, err := srv.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweep 报错: %v", err)
	}
	if rep.Deleted != 2 {
		t.Fatalf("其它两条标记应被清理，Deleted=%d", rep.Deleted)
	}
	for _, p := range corruptScenarioPaths[1:] {
		if _, err := st.Head(ctx, storePrivate, p); err == nil {
			t.Fatalf("%s 应已物理删除", p)
		}
	}
}
