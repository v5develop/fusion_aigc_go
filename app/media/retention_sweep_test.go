package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 保留期到期的用户输入：标记原因为 retention 时，宽限期过后由清理任务物理删除；其它原因的 user 标记一律不删。

// retentionMark 经保留期删除接口给 user 对象打标记，返回 rid 与桶内路径。
func retentionMark(t *testing.T, f *softDeleteFixture, content string) (string, string) {
	t.Helper()
	rid, path := f.putObject(t, classUser, storePrivate, content)
	out := decodeDelete(t, f.retentionDelete(t, map[string]any{"rid": rid}, "k-ret-"+content))
	if out.State != deleteStatePending {
		t.Fatalf("retention delete = %+v, want pending", out)
	}
	return rid, path
}

// assertIndexPresent 索引是否仍在（私有桶 idx/<对象键>.json）。
func assertIndexPresent(t *testing.T, st ObjectStore, path string, want bool) {
	t.Helper()
	key, ok := objectKeyOfPath(path)
	if !ok {
		t.Fatalf("路径没有对象键: %s", path)
	}
	_, err := st.Fetch(context.Background(), storePrivate, indexKeyOf(key), t.TempDir())
	switch {
	case want && err != nil:
		t.Fatalf("索引应仍在: %v", err)
	case !want && !errors.Is(err, errObjectNotFound):
		t.Fatalf("索引应已删除，err=%v", err)
	}
}

// 保留期标记、宽限期已过：对象、索引、标记全部删除。
func TestSweepDeletesRetentionExpiredUserInput(t *testing.T) {
	f, _ := newRetentionFixture(t)
	_, path := retentionMark(t, f, "expired")
	ctx := context.Background()
	m, ok, err := f.srv.objects.PendingMarker(ctx, storePrivate, path)
	if err != nil || !ok || m.Reason != pendingReasonRetention {
		t.Fatalf("标记 = %+v %v %v，want 原因 retention", m, ok, err)
	}
	assertIndexPresent(t, f.srv.objects, path, true)

	f.clk.t = f.clk.t.Add(61 * time.Minute)
	rep, err := f.srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 1 || rep.Failed != 0 {
		t.Fatalf("清理 = %+v, %v，want 删除 1 失败 0", rep, err)
	}
	if _, err := f.srv.objects.Head(ctx, storePrivate, path); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("保留期到期的用户输入应已物理删除: %v", err)
	}
	assertIndexPresent(t, f.srv.objects, path, false)
	if _, ok, _ := f.srv.objects.PendingMarker(ctx, storePrivate, path); ok {
		t.Fatal("清理后标记应已清除")
	}
}

// 手工（无原因）的 user 标记、宽限期已过：不删，对象、索引、标记都在。
func TestSweepKeepsManualUserMarkerPastDeadline(t *testing.T) {
	f := newSoftDeleteFixture(t)
	_, path := f.putObject(t, classUser, storePrivate, "manual")
	ctx := context.Background()
	if _, _, err := f.srv.objects.CreatePending(ctx, storePrivate, path, f.clk.t.Add(-time.Hour), pendingReasonManual); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	rep, err := f.srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 0 || rep.Failed != 1 {
		t.Fatalf("清理 = %+v, %v，want 删除 0 失败 1", rep, err)
	}
	if _, err := f.srv.objects.Head(ctx, storePrivate, path); err != nil {
		t.Fatalf("手工标记的用户输入必须保留: %v", err)
	}
	assertIndexPresent(t, f.srv.objects, path, true)
	if _, ok, _ := f.srv.objects.PendingMarker(ctx, storePrivate, path); !ok {
		t.Fatal("手工标记应保留（交人工处理）")
	}
	if !strings.Contains(logs.String(), "check=sweep.user_marker_not_retention count=1") {
		t.Fatalf("拒删日志缺判据: %s", logs.String())
	}
}

// 保留期标记、宽限期内：不删。
func TestSweepKeepsRetentionMarkerBeforeDeadline(t *testing.T) {
	f, _ := newRetentionFixture(t)
	_, path := retentionMark(t, f, "in-grace")
	f.clk.t = f.clk.t.Add(30 * time.Minute)
	rep, err := f.srv.sweepOnce(context.Background())
	if err != nil || rep.Deleted != 0 || rep.Pending != 1 {
		t.Fatalf("清理 = %+v, %v，want 删除 0 待定 1", rep, err)
	}
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("宽限期内不得删除: %v", err)
	}
}

// 保留期标记在列举后、删除前被清除（restore 或重新入库）：复核发现标记已不在，跳过，不删。
func TestSweepKeepsRetentionMarkerClearedBeforeDelete(t *testing.T) {
	f, _ := newRetentionFixture(t)
	_, path := retentionMark(t, f, "restored")
	f.clk.t = f.clk.t.Add(61 * time.Minute)
	f.srv.objects = &clearAfterListStore{ObjectStore: f.srv.objects}
	rep, err := f.srv.sweepOnce(context.Background())
	if err != nil || rep.Deleted != 0 || rep.Failed != 0 {
		t.Fatalf("清理 = %+v, %v，want 删除 0 失败 0", rep, err)
	}
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("标记已被清除，用户输入必须保留: %v", err)
	}
	assertIndexPresent(t, f.srv.objects, path, true)
}

// reasonSwapAfterListStore 列举后把标记换成同到期时间、无原因（manual）的标记，模拟别的路径改写了标记。
type reasonSwapAfterListStore struct {
	ObjectStore
}

func (c *reasonSwapAfterListStore) ListPending(ctx context.Context, store, class, marker string, limit int) ([]PendingMark, string, error) {
	marks, next, err := c.ObjectStore.ListPending(ctx, store, class, marker, limit)
	for _, m := range marks {
		if err := c.ObjectStore.ClearPending(ctx, m.Store, m.Path); err != nil {
			return nil, "", err
		}
		if _, _, err := c.ObjectStore.CreatePending(ctx, m.Store, m.Path, m.DeleteAfter, pendingReasonManual); err != nil {
			return nil, "", err
		}
	}
	return marks, next, err
}

// 列举时原因是 retention、复核时已变成 manual：按复读到的原因判断，不删。
func TestSweepRecheckRefusesUserMarkerWhoseReasonChanged(t *testing.T) {
	f, _ := newRetentionFixture(t)
	_, path := retentionMark(t, f, "swapped")
	f.clk.t = f.clk.t.Add(61 * time.Minute)
	f.srv.objects = &reasonSwapAfterListStore{ObjectStore: f.srv.objects}
	rep, err := f.srv.sweepOnce(context.Background())
	if err != nil || rep.Deleted != 0 {
		t.Fatalf("清理 = %+v, %v，want 删除 0", rep, err)
	}
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("复核时原因不是 retention，用户输入必须保留: %v", err)
	}
}

// 存储层：Delete 仍拒绝 user；DeleteRetentionExpired 只收 user，拒绝 work/deliver/asset 与历史路径。
func TestStoreRetentionDeleteOnlyUserClass(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ossSt, _ := newTestOSS(t)
	ctx := context.Background()
	user := "v1/user/2026/10/" + strings.Repeat("a", 26)
	legacy := "sha256/" + strings.Repeat("0", 64) + ".mp4"
	for name, s := range map[string]ObjectStore{"local": st, "oss": ossSt} {
		if _, err := s.Delete(ctx, storePrivate, user); !errors.Is(err, errUserInputProtected) {
			t.Fatalf("%s Delete(user) err=%v，want errUserInputProtected", name, err)
		}
		for _, p := range []string{ossWorkPath, "v1/deliver/2026/10/" + strings.Repeat("b", 26), "v1/asset/2026/10/" + strings.Repeat("c", 26), legacy} {
			if _, err := s.DeleteRetentionExpired(ctx, storePrivate, p); !errors.Is(err, errUserInputProtected) {
				t.Fatalf("%s DeleteRetentionExpired(%s) err=%v，want errUserInputProtected", name, p, err)
			}
		}
	}
}

// OSS：原因写在标记对象的 x-oss-meta-delete-reason 元数据上；retention 的 user 标记被删，manual 的不删。
func TestOSSRetentionMarkerReasonAndSweep(t *testing.T) {
	st, fake := newTestOSS(t)
	ctx := context.Background()
	retained := "v1/user/2026/10/" + strings.Repeat("d", 26)
	manual := "v1/user/2026/10/" + strings.Repeat("e", 26)
	t0 := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{retained, manual} {
		if _, err := st.Put(ctx, storePrivate, p, writeFixture(t, p), "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.CreatePending(ctx, storePrivate, retained, t0, pendingReasonRetention); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreatePending(ctx, storePrivate, manual, t0, pendingReasonManual); err != nil {
		t.Fatal(err)
	}
	if got := fake.reason["priv/"+pendingPrefix+retained]; got != pendingReasonRetention {
		t.Fatalf("retention 标记的 x-oss-meta-delete-reason = %q", got)
	}
	if got, ok := fake.reason["priv/"+pendingPrefix+manual]; ok {
		t.Fatalf("manual 标记不应带原因元数据，got %q", got)
	}
	marks, _, err := st.ListPending(ctx, storePrivate, classUser, "", 10)
	if err != nil || len(marks) != 2 {
		t.Fatalf("ListPending(user) = %+v, %v", marks, err)
	}
	for _, m := range marks {
		want := pendingReasonManual
		if m.Path == retained {
			want = pendingReasonRetention
		}
		if m.Reason != want {
			t.Fatalf("列举到的原因 %s = %q，want %q", m.Path, m.Reason, want)
		}
	}

	srv := &Server{objects: st, id: testIdentity(t), now: func() time.Time { return t0.Add(time.Minute) }}
	rep, err := srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 1 || rep.Failed != 1 {
		t.Fatalf("OSS 清理 = %+v, %v，want 删除 1 失败 1", rep, err)
	}
	if _, err := st.Head(ctx, storePrivate, retained); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("retention 标记的用户输入应已删除: %v", err)
	}
	if _, err := st.Head(ctx, storePrivate, manual); err != nil {
		t.Fatalf("manual 标记的用户输入必须保留: %v", err)
	}
}

// 单轮预算按类别分开：6000 条不可删的用户输入标记积压，不妨碍同一轮删掉到期的工作产物。
func TestSweepUserBacklogDoesNotStarveWork(t *testing.T) {
	f := newSoftDeleteFixture(t)
	ctx := context.Background()
	past := f.clk.t.Add(-time.Hour)
	const backlog = 6000
	for i := 0; i < backlog; i++ {
		p := fmt.Sprintf("v1/user/2026/10/%s", objectKeyForIndex(i))
		if _, _, err := f.srv.objects.CreatePending(ctx, storePrivate, p, past, pendingReasonManual); err != nil {
			t.Fatal(err)
		}
	}
	rid, workPath := f.putObject(t, classWork, storePrivate, "after-backlog")
	decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-work-backlog"))
	f.clk.t = f.clk.t.Add(61 * time.Minute)

	rep, err := f.srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("清理 = %+v, %v，want 工作产物删除 1", rep, err)
	}
	if _, err := f.srv.objects.Head(ctx, storePrivate, workPath); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("工作产物应已删除: %v", err)
	}
	// 用户批次自己封顶：一轮最多看 sweepMaxUserPerRun 条，其余留给下一轮
	if rep.Failed != sweepMaxUserPerRun {
		t.Fatalf("用户批次处理 %d 条，want 封顶 %d", rep.Failed, sweepMaxUserPerRun)
	}
}

// objectKeyForIndex 生成第 i 个合法对象键（26 位 base32 小写）。
func objectKeyForIndex(i int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := []byte(strings.Repeat("a", 26))
	for pos := 25; i > 0; pos-- {
		b[pos] = alphabet[i%32]
		i /= 32
	}
	return string(b)
}
