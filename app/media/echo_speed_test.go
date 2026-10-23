package media

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 回显提速（入库/复制链路）的回归测试：待删除标记、摘要复用、复制的 HEAD 次数与免 probe、响应里的阶段耗时。

// A：软删除后用同一幂等键重新入库（沿用原索引与路径）：写入即撤销待删除标记，读到的是新字节。
func TestReingestAfterSoftDeleteClearsMarker(t *testing.T) {
	srv, _ := newCachedServer(t, filepath.Join(t.TempDir(), "objects"), 1<<20)
	ctx := context.Background()
	rid := putObject(t, srv, "k-reingest", []byte("first-bytes"))
	store, path, err := srv.objectPath(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.softDelete(ctx, store, path); err != nil {
		t.Fatal(err)
	}
	var ae *apiError
	if _, err := readRID(t, srv, rid); !errors.As(err, &ae) || ae.Code != codeResourceDeleted {
		t.Fatalf("软删除后应 410, got %v", err)
	}

	again := putObject(t, srv, "k-reingest", []byte("second-bytes"))
	if again != rid {
		t.Fatalf("同一幂等键应得到同一 rid: %s vs %s", again, rid)
	}
	if _, p2, _ := srv.objectPath(ctx, rid); p2 != path {
		t.Fatalf("重新入库应沿用原路径: %s vs %s", p2, path)
	}
	if _, pending, err := srv.objects.PendingDeadline(ctx, store, path); err != nil || pending {
		t.Fatalf("重新入库后待删除标记必须已撤销: pending=%v err=%v", pending, err)
	}
	mustRead(t, srv, rid, []byte("second-bytes"))
}

// A：新写的索引也必须撤销标记。模拟清理任务删了对象与索引、清标记前中断：标记留在一个已无索引的路径上，
// 同一 rid 同月重新入库正好写到这个路径。不撤销的话新对象 410，且下一轮清理会把它物理删除。
func TestReingestOnFreshIndexClearsStaleMarker(t *testing.T) {
	srv, _ := newCachedServer(t, filepath.Join(t.TempDir(), "objects"), 1<<20)
	ctx := context.Background()
	rid := putObject(t, srv, "k-stale-marker", []byte("old-bytes"))
	store, path, err := srv.objectPath(ctx, rid)
	if err != nil {
		t.Fatal(err)
	}
	// 清理任务的前两步（删对象、删索引）完成，标记仍在且已过期
	if _, _, err := srv.objects.CreatePending(ctx, store, path, time.Now().Add(-time.Hour), pendingReasonManual); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.objects.Delete(ctx, store, path); err != nil {
		t.Fatal(err)
	}
	key, _ := objectKeyOfPath(path)
	if err := srv.objects.DeleteIndex(ctx, key); err != nil {
		t.Fatal(err)
	}
	srv.paths = newPathCache()
	srv.objCache.forget(rid)

	if again := putObject(t, srv, "k-stale-marker", []byte("new-bytes")); again != rid {
		t.Fatalf("rid 应一致: %s vs %s", again, rid)
	}
	_, p2, err := srv.objectPath(ctx, rid)
	if err != nil || p2 != path {
		t.Fatalf("前提：新索引应派生出同一路径: %s vs %s (%v)", p2, path, err)
	}
	if _, pending, err := srv.objects.PendingDeadline(ctx, store, path); err != nil || pending {
		t.Fatalf("新索引路径上的残留标记必须撤销: pending=%v err=%v", pending, err)
	}
	mustRead(t, srv, rid, []byte("new-bytes"))
	if rep, err := srv.sweepOnce(ctx); err != nil || rep.Deleted != 0 {
		t.Fatalf("清理不应删除新写入的对象: %+v %v", rep, err)
	}
	mustRead(t, srv, rid, []byte("new-bytes"))
}

// B：登记的摘要被落桶直接使用（不重算）；路径不同或文件大小与登记不符时照常重算。
func TestStoreUsesKnownDigest(t *testing.T) {
	srv, _ := newCachedServer(t, filepath.Join(t.TempDir(), "objects"), 1<<20)
	data := []byte("digest-reuse-bytes")
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	real := sha256Hex(data)
	const marker = "0000000000000000000000000000000000000000000000000000000000000abc"
	meta := MediaMeta{Mime: "font/ttf", Size: int64(len(data))}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "登记的摘要被使用", ctx: withKnownDigest(context.Background(), p, marker, int64(len(data))), want: marker},
		{name: "路径不同不使用", ctx: withKnownDigest(context.Background(), p+".other", marker, int64(len(data))), want: real},
		{name: "大小不符不使用", ctx: withKnownDigest(context.Background(), p, marker, int64(len(data))+1), want: real},
		{name: "没有登记照常计算", ctx: context.Background(), want: real},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := srv.storeWithMeta(withIdemKey(tc.ctx, "k-digest-"+string(rune('a'+i))), "intermediate", p, meta)
			if err != nil {
				t.Fatal(err)
			}
			if res.Object.Sha256 != tc.want {
				t.Fatalf("sha256 = %s, want %s", res.Object.Sha256, tc.want)
			}
		})
	}
}

// B + E：字节上传复用读请求体时算出的摘要，存下的 sha256 等于字节的真实摘要；响应带 timings，重放不带。
func TestUploadBytesTimingsAndDigest(t *testing.T) {
	requireFFmpeg(t)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: t.TempDir(), IdempotencyMinutes: 10})
	data := clipBytes(t)

	resp := uploadBytes(t, ts.URL, "intermediate", "k-timings-upload", data)
	body := readBody(t, resp)
	var out MediaResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object.Sha256 != sha256Hex(data) {
		t.Fatalf("sha256 必须是字节的真实摘要: %s", out.Object.Sha256)
	}
	assertStages(t, body, []string{"read_body", "idem_begin", "validate", "probe", "hash", "place_object", "ingest_local", "idem_finish"}, nil)

	replay := uploadBytes(t, ts.URL, "intermediate", "k-timings-upload", data)
	if replay.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("应为重放")
	}
	assertNoTimings(t, readBody(t, replay))
}

// E：JSON 入库（ingest）同样带 timings，重放不带；没装收集器的路由不带。
func TestIngestTimingsAndAbsentElsewhere(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t) // 开了幂等缓存
	makeAVClip(t, filepath.Join(f.dir, "src.mp4"))
	ingest := func() *http.Response {
		return postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
			"source": f.url("src.mp4"), "purpose": "intermediate"}, testToken, "k-timings-ingest")
	}
	body := readBody(t, ingest())
	assertStages(t, body, []string{"idem_begin", "validate", "probe", "hash", "place_object", "idem_finish"}, nil)
	assertNoTimings(t, readBody(t, ingest()))

	// probe 路由没有收集器：响应不变
	assertNoTimings(t, readBody(t, post(t, f.ts.URL, "/v1/mediasvc/probe", map[string]any{"source": f.url("src.mp4")}, testToken)))

	// 没有收集器时 logStage 只打日志
	logStage(context.Background(), "probe", time.Now())
	if got := appendTimings([]byte(`{"a":1}`+"\n"), nil); string(got) != `{"a":1}`+"\n" {
		t.Fatalf("没有耗时时响应体必须原样: %q", got)
	}
}

// C + D：复制对每个候选桶最多 HEAD 一次；同机命中写穿透条目时不 probe，结果的 sha256 与媒体属性与源一致。
func TestCopyHeadsOncePerCandidateAndSkipsProbeOnHit(t *testing.T) {
	requireFFmpeg(t)
	clip := filepath.Join(t.TempDir(), "src.mp4")
	makeAVClip(t, clip)
	for _, tc := range []struct {
		name      string
		purpose   string // 源的用途
		sameHost  bool
		wantHeads map[string]int
		wantProbe bool
	}{
		{name: "work 同机命中", purpose: "intermediate", sameHost: true, wantHeads: map[string]int{storePrivate: 1}, wantProbe: false},
		{name: "work 别的实例", purpose: "intermediate", sameHost: false, wantHeads: map[string]int{storePrivate: 1}, wantProbe: true},
		{name: "deliver 在第三方桶", purpose: "third_party", sameHost: true, wantHeads: map[string]int{storePrivate: 1, storeThirdParty: 1}, wantProbe: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := filepath.Join(t.TempDir(), "objects")
			producer, _ := newCachedServer(t, objects, 1<<30)
			copier := producer
			if !tc.sameHost {
				copier, _ = newCachedServer(t, objects, 1<<30)
			}
			counter := copier.objects.(*countingStore)
			src, err := producer.storeFile(withIdemKey(context.Background(), "k-heads-src-"+tc.name), tc.purpose, clip)
			if err != nil {
				t.Fatal(err)
			}
			counter.resetHeads()
			ts := httptest.NewServer(copier)
			t.Cleanup(ts.Close)
			body := readBody(t, postCopy(t, ts.URL, src.Object.RID, "asset_library", "k-heads-"+tc.name))
			var res MediaResult
			if err := json.Unmarshal(body, &res); err != nil {
				t.Fatal(err)
			}
			if got := counter.headCounts(); !sameCounts(got, tc.wantHeads) {
				t.Fatalf("HEAD 次数 = %v, want %v", got, tc.wantHeads)
			}
			o, s := res.Object, src.Object
			if o.Sha256 != s.Sha256 || o.Size != s.Size || o.Mime != s.Mime || o.Width != s.Width || o.Height != s.Height ||
				o.DurationS != s.DurationS || o.Fps != s.Fps || o.HasAudio != s.HasAudio {
				t.Fatalf("复制结果与源不一致:\n got %+v\nwant %+v", o, s)
			}
			var forbid []string
			if !tc.wantProbe {
				forbid = []string{"probe"}
			}
			want := []string{"idem_begin", "hash", "place_object", "idem_finish"}
			if tc.wantProbe {
				want = append(want, "probe")
			}
			assertStages(t, body, want, forbid)
		})
	}
}

// C：源不存在（两个候选桶都没有）仍是 404，待删除仍是 410（与原来一致）。
func TestCopyNotFoundAndPendingSemanticsUnchanged(t *testing.T) {
	requireFFmpeg(t)
	clip := filepath.Join(t.TempDir(), "src.mp4")
	makeAVClip(t, clip)
	objects := filepath.Join(t.TempDir(), "objects")
	srv, _ := newCachedServer(t, objects, 1<<30)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	src, err := srv.storeFile(withIdemKey(context.Background(), "k-sem-src"), "third_party", clip)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, path, err := srv.objectPath(ctx, src.Object.RID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.softDelete(ctx, storeThirdParty, path); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, postCopy(t, ts.URL, src.Object.RID, "asset_library", "k-sem-410"), http.StatusGone, codeResourceDeleted)

	if err := os.Remove(filepath.Join(objects, storeThirdParty, filepath.FromSlash(path))); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, postCopy(t, ts.URL, src.Object.RID, "asset_library", "k-sem-404"), http.StatusNotFound, codeNotFound)
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, b)
	}
	return b
}

// assertStages 响应体里有 timings，包含 want 的每个阶段、不含 forbid 的阶段，dur_ms 非负。
func assertStages(t *testing.T, body []byte, want, forbid []string) {
	t.Helper()
	var v struct {
		Timings []map[string]any `json:"timings"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Timings) == 0 {
		t.Fatalf("响应应带 timings: %s", body)
	}
	seen := map[string]bool{}
	for _, it := range v.Timings {
		if len(it) != 2 {
			t.Fatalf("timings 条目只应有 stage 与 dur_ms: %v", it)
		}
		stage, ok := it["stage"].(string)
		dur, okd := it["dur_ms"].(float64)
		if !ok || !okd || dur < 0 || dur != float64(int64(dur)) {
			t.Fatalf("timings 条目形状不对: %v", it)
		}
		seen[stage] = true
	}
	for _, s := range want {
		if !seen[s] {
			t.Fatalf("timings 缺阶段 %q: %v", s, v.Timings)
		}
	}
	for _, s := range forbid {
		if seen[s] {
			t.Fatalf("timings 不应有阶段 %q: %v", s, v.Timings)
		}
	}
}

func assertNoTimings(t *testing.T, body []byte) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["timings"]; ok {
		t.Fatalf("不应带 timings: %s", body)
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
