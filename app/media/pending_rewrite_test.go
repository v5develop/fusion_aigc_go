package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 待删除的对象被同一幂等键重新写入后，标记被清除：宽限期结束也不会被删掉。
func TestRewriteClearsPendingDeleteMark(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := "v1/work/2026/10/abcdefghijklmnopqrstuvwxyz"
	if _, err := st.Put(ctx, storePrivate, path, src, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreatePending(ctx, storePrivate, path, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearPending(ctx, storePrivate, path); err != nil {
		t.Fatal(err)
	}
	_, pending, err := st.PendingDeadline(ctx, storePrivate, path)
	if err != nil || pending {
		t.Fatalf("清除后不应仍是待删除: pending=%v err=%v", pending, err)
	}
}
