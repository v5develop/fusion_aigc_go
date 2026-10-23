package media

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 用户输入保留期删除（POST /resources/retention/delete）的测试：只收 user 类别，软删除语义与 delete 相同。

const retentionDeletePath = "/v1/mediasvc/resources/retention/delete"

// pendingCountingStore 在 countingStore 之上统计待删除标记的写入（登记与撤销），用来证明被拒的请求零写入、回放不重复删除。
type pendingCountingStore struct {
	*countingStore
	creates atomic.Int32
	clears  atomic.Int32
}

func (c *pendingCountingStore) CreatePending(ctx context.Context, store, path string, at time.Time) (time.Time, bool, error) {
	c.creates.Add(1)
	return c.countingStore.CreatePending(ctx, store, path, at)
}

func (c *pendingCountingStore) ClearPending(ctx context.Context, store, path string) error {
	c.clears.Add(1)
	return c.countingStore.ClearPending(ctx, store, path)
}

// writes 数据对象写入 + 标记写入的总次数（索引写入不计）。
func (c *pendingCountingStore) writes() int32 {
	return c.puts.Load() + c.creates.Load() + c.clears.Load()
}

// newRetentionFixture 软删除夹具，落桶换成计数存储。返回前先落好对象，计数从零开始。
func newRetentionFixture(t *testing.T) (*softDeleteFixture, func() *pendingCountingStore) {
	t.Helper()
	f := newSoftDeleteFixture(t)
	// 夹具没配幂等保留期（TTL 0 不回放）；这里要验证回放，给 1 小时
	f.srv.idem = newMemIdemStore(time.Hour)
	return f, func() *pendingCountingStore {
		local, ok := f.srv.objects.(*LocalStore)
		if !ok {
			t.Fatalf("夹具落桶应为 LocalStore，实际 %T", f.srv.objects)
		}
		cs := &pendingCountingStore{countingStore: &countingStore{LocalStore: local}}
		f.srv.objects = cs
		return cs
	}
}

func (f *softDeleteFixture) retentionDelete(t *testing.T, body map[string]any, key string) *http.Response {
	t.Helper()
	return postWithKey(t, f.ts.URL, retentionDeletePath, body, testToken, key)
}

// user 类别：只登记一次待删除标记；同一幂等键回放返回同一结果，不再登记；审计日志带 check、rid、trace_id，不带地址。
func TestRetentionDeleteUserSoftDeletesOnceAndReplays(t *testing.T) {
	f, wrap := newRetentionFixture(t)
	rid, path := f.putObject(t, classUser, storePrivate, "user-upload")
	cs := wrap()
	logs := captureLog(t)

	first := f.retentionDelete(t, map[string]any{"rid": rid}, "k-ret-1")
	firstBody, _ := io.ReadAll(first.Body)
	if first.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", first.StatusCode, firstBody)
	}
	replay := f.retentionDelete(t, map[string]any{"rid": rid}, "k-ret-1")
	replayBody, _ := io.ReadAll(replay.Body)
	if replay.StatusCode != http.StatusOK || replay.Header.Get("Idempotent-Replayed") != "true" || !bytes.Equal(firstBody, replayBody) {
		t.Fatalf("回放 = %d %s，want 与首次相同 %s", replay.StatusCode, replayBody, firstBody)
	}
	if got := cs.creates.Load(); got != 1 {
		t.Fatalf("登记待删除 %d 次，want 恰好 1 次（回放不得重复删除）", got)
	}
	out := decodeDelete(t, f.retentionDelete(t, map[string]any{"rid": rid}, "k-ret-2"))
	if !out.Deleted || out.State != deleteStatePending || out.DeleteAfter == "" {
		t.Fatalf("另一把键再删 = %+v，want 仍是待删除", out)
	}
	at, ok, err := f.srv.objects.PendingDeadline(context.Background(), storePrivate, path)
	if err != nil || !ok || at.Format(time.RFC3339) != out.DeleteAfter || !at.Equal(f.clk.t.Add(time.Hour)) {
		t.Fatalf("标记到期 = %v %v %v，want 现在+宽限期且两次一致 %s", at, ok, err, out.DeleteAfter)
	}
	// 字节仍在：只是打了标记
	if _, err := f.srv.objects.Head(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("待删除的用户输入字节应仍在: %v", err)
	}

	line := logs.String()
	if !strings.Contains(line, "check=retention.delete") || !strings.Contains(line, "rid="+rid) ||
		!strings.Contains(line, "trace_id=") || !strings.Contains(line, "state="+deleteStatePending) {
		t.Fatalf("审计日志缺字段: %s", line)
	}
	if strings.Contains(line, "http://") || strings.Contains(line, "https://") || strings.Contains(line, path) {
		t.Fatalf("审计日志不得带地址或桶内路径: %s", line)
	}
}

// work、asset、deliver 类别一律 403 purpose.not_user_class，零写入，对象照常可读。
func TestRetentionDeleteRejectsNonUserClasses(t *testing.T) {
	cases := []struct {
		class, store string
	}{
		{classWork, storePrivate},
		{classAsset, storeAsset},
		{classDeliver, storeThirdParty},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			f, wrap := newRetentionFixture(t)
			rid, path := f.putObject(t, tc.class, tc.store, tc.class+"-clip")
			cs := wrap()
			body := assertAPIError(t, f.retentionDelete(t, map[string]any{"rid": rid}, "k-"+tc.class),
				http.StatusForbidden, "forbidden_purpose")
			if body.Error.Check != "purpose.not_user_class" {
				t.Fatalf("check = %q, want purpose.not_user_class", body.Error.Check)
			}
			if got := cs.writes(); got != 0 {
				t.Fatalf("被拒请求写入 %d 次，want 0", got)
			}
			if _, ok, _ := f.srv.objects.PendingDeadline(context.Background(), tc.store, path); ok {
				t.Fatal("被拒请求不得留下待删除标记")
			}
		})
	}
}

// rid 不合法（含空、历史路径、类别拼错）一律 400 invalid_request，零写入；缺幂等键同样 400。
func TestRetentionDeleteRejectsMalformedRid(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		key  string
	}{
		{"empty", map[string]any{"rid": ""}, "k-empty"},
		{"missing", map[string]any{}, "k-missing"},
		{"garbage", map[string]any{"rid": "not-a-rid"}, "k-garbage"},
		{"legacy_path", map[string]any{"rid": "sha256/" + strings.Repeat("0", 64) + ".mp4"}, "k-legacy"},
		{"unknown_class", map[string]any{"rid": "tst001-index-" + strings.Repeat("A", 26)}, "k-class"},
		{"no_idempotency_key", map[string]any{"rid": "tst001-user-" + strings.Repeat("A", 26)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, wrap := newRetentionFixture(t)
			cs := wrap()
			assertAPIError(t, f.retentionDelete(t, tc.body, tc.key), http.StatusBadRequest, "invalid_request")
			if got := cs.writes(); got != 0 {
				t.Fatalf("被拒请求写入 %d 次，want 0", got)
			}
		})
	}
}

// 软删除后读取返回 410；宽限期内 restore 成功，之后恢复可读。
func TestRetentionDeleteGoneThenRestoreWithinGrace(t *testing.T) {
	f, wrap := newRetentionFixture(t)
	rid, path := f.putObject(t, classUser, storePrivate, "user-restore")
	wrap()

	out := decodeDelete(t, f.retentionDelete(t, map[string]any{"rid": rid}, "k-ret-restore"))
	if out.State != deleteStatePending {
		t.Fatalf("delete = %+v", out)
	}
	urlsReq := map[string]any{"rid": rid, "store": storePrivate, "mode": "signed"}
	assertAPIError(t, post(t, f.ts.URL, "/v1/mediasvc/resources/urls", urlsReq, testToken), http.StatusGone, "resource_deleted")
	assertAPIError(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"rid": rid}, "purpose": "intermediate"}, testToken, "k-read-user"), http.StatusGone, "resource_deleted")

	f.clk.t = f.clk.t.Add(30 * time.Minute) // 仍在 60 分钟宽限期内
	if got := decodeRestore(t, f.restore(t, rid)); !got.Restored {
		t.Fatalf("宽限期内 restore = %+v, want restored", got)
	}
	// 恢复后不再是 410（本地存储没有签名地址，签发返回 409 no_public_location，与未删除的对象一致）
	if resp := post(t, f.ts.URL, "/v1/mediasvc/resources/urls", urlsReq, testToken); resp.StatusCode == http.StatusGone {
		t.Fatal("restore 后读取仍返回 410")
	}
	if err := f.srv.checkReadable(context.Background(), storePrivate, path); err != nil {
		t.Fatalf("restore 后应可读: %v", err)
	}
}

// 不存在的 user 类别 rid：与 delete 相同返回 absent，不写标记。
func TestRetentionDeleteAbsentUserRid(t *testing.T) {
	f, wrap := newRetentionFixture(t)
	cs := wrap()
	rid, err := f.srv.id.RID(f.srv.id.instanceID, classUser, "never-stored")
	if err != nil {
		t.Fatal(err)
	}
	out := decodeDelete(t, f.retentionDelete(t, map[string]any{"rid": rid}, "k-absent"))
	if out.Deleted || out.State != deleteStateAbsent || out.DeleteAfter != "" {
		t.Fatalf("不存在的对象 = %+v, want absent", out)
	}
	if got := cs.writes(); got != 0 {
		t.Fatalf("写入 %d 次，want 0", got)
	}
}

// 回归：通用 delete 对 user 类别仍是 403 forbidden_purpose（purpose.user_input_protected），零写入。
func TestDeleteStillRefusesUserClass(t *testing.T) {
	f, wrap := newRetentionFixture(t)
	rid, path := f.putObject(t, classUser, storePrivate, "user-keep")
	cs := wrap()
	body := assertAPIError(t, f.delete(t, map[string]any{"rid": rid}, "k-del-user"), http.StatusForbidden, "forbidden_purpose")
	if body.Error.Check != "purpose.user_input_protected" {
		t.Fatalf("check = %q, want purpose.user_input_protected", body.Error.Check)
	}
	if got := cs.writes(); got != 0 {
		t.Fatalf("写入 %d 次，want 0", got)
	}
	if _, ok, _ := f.srv.objects.PendingDeadline(context.Background(), storePrivate, path); ok {
		t.Fatal("通用 delete 不得给用户输入打标记")
	}
}
