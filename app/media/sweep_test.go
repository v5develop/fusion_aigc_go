package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 软删除（设计文档 §8.5）的测试。时钟可注入：Server.now 换成 fakeClock，宽限期与清理都按它走。

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// softDeleteFixture 一个带假时钟、宽限期 60 分钟的服务（本地存储）。
type softDeleteFixture struct {
	srv *Server
	ts  *httptest.Server
	clk *fakeClock
}

func newSoftDeleteFixture(t *testing.T) *softDeleteFixture {
	t.Helper()
	srv, err := NewServer(Config{
		Token: testToken, InstanceID: "tst001", IdentitySecret: testSecret,
		ObjectDir: filepath.Join(t.TempDir(), "objects"), TempDir: t.TempDir(),
		MaxConcurrent: 2, MaxInputBytes: 50 << 20, FetchTimeoutSeconds: 60, OpTimeoutSeconds: 120,
		MaxComposeInputs: 5, DeleteGraceMinutes: 60, SweepIntervalMinutes: 60,
		Idempotency: IdempotencyConfig{Store: IdemStoreMemory},
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	srv.now = clk.Now
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &softDeleteFixture{srv: srv, ts: ts, clk: clk}
}

// putObject 直接把一段内容落到对象存储（不走 ffmpeg），返回 rid 与桶内路径。
func (f *softDeleteFixture) putObject(t *testing.T, class, store, content string) (string, string) {
	t.Helper()
	rid, err := f.srv.id.RID(f.srv.id.instanceID, class, "test-"+class+"-"+content)
	if err != nil {
		t.Fatal(err)
	}
	path := storePath(class, f.srv.id.ObjectKey(rid))
	if _, err := f.srv.objects.Put(context.Background(), store, path, writeFixture(t, content), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	return rid, path
}

func (f *softDeleteFixture) delete(t *testing.T, body map[string]any, key string) *http.Response {
	t.Helper()
	return postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", body, testToken, key)
}

func (f *softDeleteFixture) restore(t *testing.T, rid string) *http.Response {
	t.Helper()
	return post(t, f.ts.URL, "/v1/mediasvc/resources/restore", map[string]any{"rid": rid}, testToken)
}

func decodeDelete(t *testing.T, resp *http.Response) deleteResp {
	t.Helper()
	var out deleteResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func decodeRestore(t *testing.T, resp *http.Response) restoreResp {
	t.Helper()
	var out restoreResp
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 删除只打标记，宽限期内 restore 可撤销；撤销后资源恢复可读。
func TestSoftDeleteMarksAndRestoresWithinGrace(t *testing.T) {
	f := newSoftDeleteFixture(t)
	rid, path := f.putObject(t, classWork, storePrivate, "work-clip")

	out := decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-del-1"))
	if !out.Deleted || out.State != deleteStatePending || out.DeleteAfter == "" {
		t.Fatalf("delete = %+v, want 待删除且带到期时间", out)
	}
	// 字节仍在：只是打了标记
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("待删除对象应仍在存储里: %v", err)
	}
	// 同一幂等键重放与另一把键重复删除：沿用原到期时间，不延长
	again := decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-del-2"))
	if again.State != deleteStatePending || again.DeleteAfter != out.DeleteAfter {
		t.Fatalf("重复删除 = %+v，want 同一到期时间 %s", again, out.DeleteAfter)
	}

	// 宽限期内被标记的对象：读取拒绝（410），资源地址也拒绝签发
	assertAPIError(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"rid": rid}, "purpose": "intermediate"}, testToken, "k-read-rid"), http.StatusGone, "resource_deleted")
	assertAPIError(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"key": path, "store": storePrivate}, "purpose": "intermediate"}, testToken, "k-read-key"), http.StatusGone, "resource_deleted")
	assertAPIError(t, post(t, f.ts.URL, "/v1/mediasvc/resources/urls", map[string]any{
		"rid": rid, "store": storePrivate, "mode": "signed"}, testToken), http.StatusGone, "resource_deleted")

	// 宽限期内撤销
	f.clk.t = f.clk.t.Add(30 * time.Minute)
	if got := decodeRestore(t, f.restore(t, rid)); !got.Restored {
		t.Fatalf("restore = %+v, want restored", got)
	}
	// 撤销后可读：资源地址签发不再被标记拦截（本地存储没有公有/签名地址，返回 409 而不是 410）
	assertAPIError(t, post(t, f.ts.URL, "/v1/mediasvc/resources/urls", map[string]any{
		"rid": rid, "store": storePrivate, "mode": "signed"}, testToken), http.StatusConflict, "no_public_location")
	// 不在待删除状态再撤销：幂等，restored=false
	if got := decodeRestore(t, f.restore(t, rid)); got.Restored {
		t.Fatalf("二次 restore = %+v, want restored=false", got)
	}
}

// 宽限期过后：撤销被拒绝（409），清理任务物理删除，之后对象不存在。宽限期内的清理不删。
func TestSweepDeletesOnlyAfterGrace(t *testing.T) {
	f := newSoftDeleteFixture(t)
	rid, path := f.putObject(t, classWork, storePrivate, "to-sweep")
	decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-sweep"))

	// 宽限期内清理：什么也不删
	rep, err := f.srv.sweepOnce(context.Background())
	if err != nil || rep.Deleted != 0 || rep.Pending != 1 {
		t.Fatalf("宽限期内清理 = %+v, %v，want 删除 0 待定 1", rep, err)
	}
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("宽限期内对象不应被删: %v", err)
	}

	// 宽限期过后：撤销被拒绝
	f.clk.t = f.clk.t.Add(61 * time.Minute)
	assertAPIError(t, f.restore(t, rid), http.StatusConflict, "grace_expired")

	rep, err = f.srv.sweepOnce(context.Background())
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("宽限期后清理 = %+v, %v，want 删除 1", rep, err)
	}
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err == nil {
		t.Fatal("宽限期后对象应已物理删除")
	}
	if _, ok, _ := f.srv.objects.PendingDeadline(context.Background(), storePrivate, path); ok {
		t.Fatal("清理后待删除标记应已清除")
	}
	// 再删一次：对象已不在，返回 absent
	out := decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-sweep-2"))
	if out.Deleted || out.State != deleteStateAbsent {
		t.Fatalf("清理后再删 = %+v, want absent", out)
	}
}

// 用户输入与历史路径永不删：delete 直接 403；即使标记被手工写入，清理也不删。
func TestSoftDeleteProtectsUserInputAndLegacyPaths(t *testing.T) {
	f := newSoftDeleteFixture(t)
	userRid, userPath := f.putObject(t, classUser, storePrivate, "user-input")

	assertAPIError(t, f.delete(t, map[string]any{"rid": userRid}, "k-user"), http.StatusForbidden, "forbidden_purpose")
	legacy := "sha256/" + strings.Repeat("0", 64) + ".mp4"
	assertAPIError(t, f.delete(t, map[string]any{"object": map[string]any{"store": storePrivate, "key": legacy}}, "k-legacy"),
		http.StatusForbidden, "forbidden_purpose")

	// 手工登记标记（绕过 delete）：清理必须拦住，不删用户输入
	ctx := context.Background()
	if _, _, err := f.srv.objects.CreatePending(ctx, storePrivate, userPath, f.clk.t.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rep, err := f.srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 0 || rep.Failed != 1 {
		t.Fatalf("清理用户输入 = %+v, %v，want 失败 1 且不删", rep, err)
	}
	if _, err := f.srv.objects.Head(ctx, storePrivate, userPath); err != nil {
		t.Fatalf("用户输入必须保留: %v", err)
	}
}

// 没有标记的对象读取照常（这里只验证门禁不误伤：没有标记时 restore 对存在的对象是幂等 no-op）。
func TestRestoreUnmarkedObjectIsNoop(t *testing.T) {
	f := newSoftDeleteFixture(t)
	rid, _ := f.putObject(t, classDeliver, storePrivate, "deliver-clip")
	if got := decodeRestore(t, f.restore(t, rid)); got.Restored {
		t.Fatalf("未标记的对象 restore = %+v, want restored=false", got)
	}
	assertAPIError(t, f.restore(t, "tst001-work-"+strings.Repeat("a", 26)), http.StatusNotFound, "not_found")
}

// 对不存在的对象删除：absent，不登记标记。
func TestSoftDeleteAbsentObjectDoesNotMark(t *testing.T) {
	f := newSoftDeleteFixture(t)
	rid, err := f.srv.id.RID(f.srv.id.instanceID, classWork, "never-written")
	if err != nil {
		t.Fatal(err)
	}
	out := decodeDelete(t, f.delete(t, map[string]any{"rid": rid}, "k-absent"))
	if out.Deleted || out.State != deleteStateAbsent {
		t.Fatalf("不存在的对象 delete = %+v, want absent", out)
	}
	path := storePath(classWork, f.srv.id.ObjectKey(rid))
	if _, ok, _ := f.srv.objects.PendingDeadline(context.Background(), storePrivate, path); ok {
		t.Fatal("不存在的对象不应登记标记")
	}
}

// OSS 实现：标记是同桶的状态对象，禁止覆盖；列举、清理链路走通。
func TestOSSPendingStateAndSweep(t *testing.T) {
	st, _ := newTestOSS(t)
	ctx := context.Background()
	if _, err := st.Put(ctx, storePrivate, ossWorkPath, writeFixture(t, "oss-work"), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2030, 1, 8, 0, 0, 0, 0, time.UTC)
	at, created, err := st.CreatePending(ctx, storePrivate, ossWorkPath, t0)
	if err != nil || !created || !at.Equal(t0) {
		t.Fatalf("CreatePending = %v, %v, %v", at, created, err)
	}
	// 再登记一次：不覆盖，返回已有的到期时间
	at, created, err = st.CreatePending(ctx, storePrivate, ossWorkPath, t0.Add(time.Hour))
	if err != nil || created || !at.Equal(t0) {
		t.Fatalf("重复 CreatePending = %v, %v, %v，want 原到期时间且 created=false", at, created, err)
	}
	marks, next, err := st.ListPending(ctx, storePrivate, "", 10)
	if err != nil || next != "" || len(marks) != 1 || marks[0].Path != ossWorkPath || !marks[0].DeleteAfter.Equal(t0) {
		t.Fatalf("ListPending = %+v next=%q err=%v", marks, next, err)
	}

	srv := &Server{objects: st, id: testIdentity(t), now: func() time.Time { return t0.Add(-time.Minute) }}
	rep, err := srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 0 || rep.Pending != 1 {
		t.Fatalf("OSS 宽限期内清理 = %+v, %v", rep, err)
	}
	srv.now = func() time.Time { return t0.Add(time.Minute) }
	rep, err = srv.sweepOnce(ctx)
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("OSS 宽限期后清理 = %+v, %v", rep, err)
	}
	if _, err := st.Head(ctx, storePrivate, ossWorkPath); err == nil {
		t.Fatal("OSS 对象应已物理删除")
	}
	if _, ok, _ := st.PendingDeadline(ctx, storePrivate, ossWorkPath); ok {
		t.Fatal("OSS 清理后标记应已删除")
	}
}

// 本地存储的标记也要能列举与分页（marker 语义与 OSS 一致）。
func TestLocalPendingListPaginates(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	paths := []string{
		"v1/work/" + strings.Repeat("a", 26),
		"v1/work/" + strings.Repeat("b", 26),
		"v1/deliver/" + strings.Repeat("c", 26),
	}
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range paths {
		if _, _, err := st.CreatePending(ctx, storePrivate, p, at); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	marker := ""
	for {
		marks, next, err := st.ListPending(ctx, storePrivate, marker, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range marks {
			got = append(got, m.Path)
		}
		if next == "" {
			break
		}
		marker = next
	}
	if len(got) != len(paths) {
		t.Fatalf("分页列举 = %v, want %d 条", got, len(paths))
	}
	// 清掉标记后目录为空，列举返回空而不是报错
	for _, p := range paths {
		if err := st.ClearPending(ctx, storePrivate, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(st.dir, storePrivate, "pending-delete")); err != nil {
		t.Fatalf("pending-delete 目录应仍在（只是空）: %v", err)
	}
	if marks, _, err := st.ListPending(ctx, storePrivate, "", 10); err != nil || len(marks) != 0 {
		t.Fatalf("清空后列举 = %v, %v", marks, err)
	}
}
