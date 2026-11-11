package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copyServerFixture 服务实例的落桶换成假 OSS（支持服务端复制），直接调 storeMeta / storeFile 准备源。
type copyServerFixture struct {
	srv  *Server
	ts   *httptest.Server
	st   *OSSStore
	fake *copyOSS
}

func newCopyServerFixture(t *testing.T, mut func(*OSSConfig)) *copyServerFixture {
	t.Helper()
	srv, err := NewServer(Config{
		Token: testToken, InstanceID: "tst001", IdentitySecret: testSecret,
		ObjectDir: filepath.Join(t.TempDir(), "objects"), TempDir: t.TempDir(),
		MaxConcurrent: 2, MaxInputBytes: 50 << 20, FetchTimeoutSeconds: 60, OpTimeoutSeconds: 120,
		MaxComposeInputs: 5, MaxSourcePixels: 100_000_000,
		Idempotency: IdempotencyConfig{Store: IdemStoreMemory},
	})
	if err != nil {
		t.Fatal(err)
	}
	st, fake := newCopyTestOSS(t, mut)
	srv.objects = st
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &copyServerFixture{srv: srv, ts: ts, st: st, fake: fake}
}

// copyResp resources/copy 的响应：MediaResult 加本次执行的阶段耗时。
type copyResp struct {
	MediaResult
	Timings []StageTiming `json:"timings"`
}

func (f *copyServerFixture) copy(t *testing.T, rid, purpose, key string) (copyResp, *http.Response) {
	t.Helper()
	resp := postCopy(t, f.ts.URL, rid, purpose, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("copy status = %d", resp.StatusCode)
	}
	var out copyResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out, resp
}

func stages(ts []StageTiming) map[string]bool {
	m := map[string]bool{}
	for _, s := range ts {
		m[s.Stage] = true
	}
	return m
}

// 有事实的源（probe 过的落桶）：直接给 meta，不需要 ffmpeg。
func (f *copyServerFixture) sourceWithFacts(t *testing.T, content string, meta MediaMeta) MediaResult {
	t.Helper()
	local := writeFixture(t, content)
	meta.Size = int64(len(content))
	res, err := f.srv.storeMeta(context.Background(), "intermediate", local, meta, true)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func writePNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 8))
	for x := 0; x < 16; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{uint8(x * 16), uint8(y * 32), 90, 255})
		}
	}
	p := filepath.Join(t.TempDir(), "src.png")
	out, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(out, img); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *copyServerFixture) objectsIn(bucket string) int {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	n := 0
	for k := range f.fake.objs {
		if strings.HasPrefix(k, bucket+"/v1/") {
			n++
		}
	}
	return n
}

// 同地域 OSS + 源带事实：服务端复制。结果的 sha256/大小/元数据与源一致，parents 指向源；
// 没有下载与上传，timings 是 copy_server 而不是 copy_read / put；复制出来的对象再复制仍走服务端。
func TestCopyServerSideWhenEligible(t *testing.T) {
	f := newCopyServerFixture(t, nil)
	logs := captureLog(t)
	src := f.sourceWithFacts(t, "video-bytes", testFactsMeta)
	putsBefore := f.fake.puts

	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-ss-1")
	o := res.Object
	if o.RID == src.Object.RID || o.Class != classAsset || o.Store != storeAsset {
		t.Fatalf("object = %+v", o)
	}
	if o.Sha256 != src.Object.Sha256 || o.Size != src.Object.Size || o.Mime != "video/mp4" ||
		o.DurationS != testFactsMeta.DurationS || o.Width != 720 || o.Height != 1280 || o.Fps != 30 || !o.HasAudio {
		t.Fatalf("元数据必须与源一致: %+v vs %+v", o, src.Object)
	}
	if len(res.Parents) != 1 || res.Parents[0] != src.Object.RID {
		t.Fatalf("parents = %v", res.Parents)
	}
	if res.Url.Kind != "public" || !strings.HasPrefix(res.Url.Value, "https://cdn.example.com/v1/asset/") {
		t.Fatalf("url = %+v", res.Url)
	}
	st := stages(res.Timings)
	if !st["copy_server"] || !st["copy_src_facts"] || st["copy_read"] || st["put"] || st["probe"] || st["hash"] {
		t.Fatalf("timings = %+v", res.Timings)
	}
	if f.fake.gets != 0 || f.fake.puts != putsBefore || f.fake.copies != 1 {
		t.Fatalf("不应读写字节: gets=%d puts=%d copies=%d", f.fake.gets, f.fake.puts-putsBefore, f.fake.copies)
	}
	if !strings.Contains(logs.String(), "check=media.copy_server_side") || !strings.Contains(logs.String(), "mode=single") {
		t.Fatalf("日志缺少 copy_server_side: %s", logs.String())
	}

	// 复制的复制（素材库 → 第三方），目标对象也带事实
	res2, _ := f.copy(t, o.RID, "third_party", "k-ss-2")
	if res2.Object.Sha256 != src.Object.Sha256 || res2.Object.Store != storeThirdParty || f.fake.copies != 2 || f.fake.gets != 0 {
		t.Fatalf("复制的复制 = %+v copies=%d", res2.Object, f.fake.copies)
	}
}

// 大对象走分片复制（阈值调小模拟 > 1 GiB）。
func TestCopyServerSideMultipart(t *testing.T) {
	f := newCopyServerFixture(t, nil)
	f.st.copier.threshold, f.st.copier.partSize = 8, 5
	logs := captureLog(t)
	src := f.sourceWithFacts(t, "a-larger-video-payload-0123456789", testFactsMeta)
	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-ss-mp")
	if res.Object.Sha256 != src.Object.Sha256 || res.Object.Size != src.Object.Size {
		t.Fatalf("object = %+v", res.Object)
	}
	if !strings.Contains(logs.String(), "mode=multipart") || len(f.fake.partReqs) != 7 || f.fake.gets != 0 {
		t.Fatalf("parts=%d logs=%s", len(f.fake.partReqs), logs.String())
	}
}

// 幂等：同键重放得到同一个 rid（回放）；同键在幂等记录过期后重新执行、且这次服务端复制被拒走回退路径，
// rid 与路径仍然不变（两条路径的 rid 派生一致）。
func TestCopyServerSideIdempotencyAndPermissionFallback(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyServerFixture(t, nil)
	f.srv.idem = newMemIdemStore(time.Hour)
	src, err := f.srv.storeFile(context.Background(), "intermediate", writePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := f.copy(t, src.Object.RID, "asset_library", "k-idem")
	if f.fake.copies != 1 {
		t.Fatalf("首次应服务端复制，copies=%d", f.fake.copies)
	}
	again, resp := f.copy(t, src.Object.RID, "asset_library", "k-idem")
	if again.Object.RID != first.Object.RID || resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("重放 rid = %s vs %s", again.Object.RID, first.Object.RID)
	}

	// 幂等记录不在了（过期 / 换实例）+ 目标密钥读不了源桶：回退下载 + 上传
	f.srv.idem = newMemIdemStore(time.Hour)
	f.fake.denyCopy = true
	assetsBefore := f.objectsIn("asset")
	logs := captureLog(t)
	fb, _ := f.copy(t, src.Object.RID, "asset_library", "k-idem")
	if fb.Object.RID != first.Object.RID {
		t.Fatalf("回退路径的 rid 必须相同: %s vs %s", fb.Object.RID, first.Object.RID)
	}
	if f.objectsIn("asset") != assetsBefore {
		t.Fatal("同一 rid 必须写回同一路径")
	}
	if fb.Object.Sha256 != src.Object.Sha256 || fb.Object.Width != 16 || fb.Object.Height != 8 || fb.Object.Mime != "image/png" {
		t.Fatalf("回退结果 = %+v", fb.Object)
	}
	if fb.Object != first.Object {
		t.Fatalf("两条路径的结果必须一致:\n server=%+v\n fallback=%+v", first.Object, fb.Object)
	}
	st := stages(fb.Timings)
	if !st["copy_server"] || !st["copy_read"] || !st["put"] {
		t.Fatalf("timings = %+v", fb.Timings)
	}
	if !strings.Contains(logs.String(), "check=media.copy_fallback reason=permission") {
		t.Fatalf("日志缺少回退原因: %s", logs.String())
	}
	// 被拒后这对桶暂停尝试：下一次直接回退，不再发复制请求
	f.srv.idem = newMemIdemStore(time.Hour)
	copyReqs := len(f.fake.copyAKs)
	f.copy(t, src.Object.RID, "asset_library", "k-idem-2")
	if len(f.fake.copyAKs) != copyReqs || !strings.Contains(logs.String(), "reason=denied_recently") {
		t.Fatalf("暂停期内不应再发复制请求: %d → %d", copyReqs, len(f.fake.copyAKs))
	}
}

// 源没有事实（功能上线前写入 / 直传）：回退；回退写出的目标带事实，它再被复制时走服务端。
func TestCopyFallbackWhenSourceFactsMissing(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyServerFixture(t, nil)
	logs := captureLog(t)
	ctx := context.Background()
	rid, err := f.srv.id.RID(f.srv.id.instanceID, classWork, "legacy-src")
	if err != nil {
		t.Fatal(err)
	}
	path, err := f.srv.placeObject(ctx, rid, classWork, storePrivate, f.srv.id.ObjectKey(rid), "image/png", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Put(ctx, storePrivate, path, writePNG(t), "image/png"); err != nil {
		t.Fatal(err)
	}
	res, _ := f.copy(t, rid, "asset_library", "k-legacy")
	if !strings.Contains(logs.String(), "check=media.copy_fallback reason=source_facts_missing") || f.fake.gets != 1 || f.fake.copies != 0 {
		t.Fatalf("应回退下载: gets=%d logs=%s", f.fake.gets, logs.String())
	}
	if !stages(res.Timings)["copy_read"] {
		t.Fatalf("timings = %+v", res.Timings)
	}
	res2, _ := f.copy(t, res.Object.RID, "third_party", "k-legacy-2")
	if f.fake.copies != 1 || f.fake.gets != 1 || res2.Object.Sha256 != res.Object.Sha256 || res2.Object.Width != 16 {
		t.Fatalf("回退写出的对象应带事实: copies=%d gets=%d %+v", f.fake.copies, f.fake.gets, res2.Object)
	}
}

// 开关关闭、本地存储：回退并打出原因。
func TestCopyFallbackDisabledAndLocalStore(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyServerFixture(t, func(c *OSSConfig) { c.DisableServerSideCopy = true })
	logs := captureLog(t)
	src, err := f.srv.storeFile(context.Background(), "intermediate", writePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-disabled")
	if f.fake.copies != 0 || res.Object.Sha256 != src.Object.Sha256 || !strings.Contains(logs.String(), "reason=disabled") {
		t.Fatalf("关闭时应回退: copies=%d logs=%s", f.fake.copies, logs.String())
	}

	lf := newSoftDeleteFixture(t)
	lsrc, err := lf.srv.storeFile(context.Background(), "intermediate", writePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	resp := postCopy(t, lf.ts.URL, lsrc.Object.RID, "asset_library", "k-local")
	if got := decodeMediaResult(t, resp); got.Object.Sha256 != lsrc.Object.Sha256 {
		t.Fatalf("本地存储复制 = %+v", got.Object)
	}
	if !strings.Contains(logs.String(), "reason=store_unsupported") {
		t.Fatalf("本地存储应回退: %s", logs.String())
	}
}

// 源对象的事实被别的写入方伪造（没有本服务的签名）：不当真，回退读字节，结果是真实字节的 sha256 与元数据。
func TestCopyForgedFactsFallBackToRealBytes(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyServerFixture(t, nil)
	logs := captureLog(t)
	src, err := f.srv.storeFile(context.Background(), "intermediate", writePNG(t))
	if err != nil {
		t.Fatal(err)
	}
	_, srcPath, err := f.srv.objectPath(context.Background(), src.Object.RID)
	if err != nil {
		t.Fatal(err)
	}
	forged := storedFacts{V: 1, Sha256: strings.Repeat("0", 64), Size: src.Object.Size,
		Meta: MediaMeta{Mime: "image/png", Size: src.Object.Size, Width: 4096, Height: 4096}}
	raw, _ := json.Marshal(forged)
	f.fake.mu.Lock()
	f.fake.objs["priv/"+srcPath].meta = map[string]string{ossFactsHeader: base64.RawURLEncoding.EncodeToString(raw) + ".AAAA"}
	f.fake.mu.Unlock()

	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-forged")
	if res.Object.Sha256 != src.Object.Sha256 || res.Object.Width != 16 || res.Object.Height != 8 {
		t.Fatalf("不能按伪造的事实给结果: %+v", res.Object)
	}
	if f.fake.copies != 0 || f.fake.gets != 1 || !strings.Contains(logs.String(), "reason=source_facts_untrusted") {
		t.Fatalf("应回退读字节: copies=%d gets=%d logs=%s", f.fake.copies, f.fake.gets, logs.String())
	}
}

// 落桶（storeMeta）写出的事实带 CRC64，与 OSS 对这份字节算的一致；CRC64 相符时走服务端复制。
func TestCopyServerSideFactsBoundByCRC64(t *testing.T) {
	f := newCopyServerFixture(t, nil)
	src := f.sourceWithFacts(t, "video-bytes", testFactsMeta)
	_, srcPath, err := f.srv.objectPath(context.Background(), src.Object.RID)
	if err != nil {
		t.Fatal(err)
	}
	facts := openFacts(f.srv.id, f.fake.obj("priv/" + srcPath).meta[ossFactsHeader])
	if facts == nil || facts.CRC64 != crcOf([]byte("video-bytes")) {
		t.Fatalf("落桶事实应带 CRC64: %+v", facts)
	}
	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-crc-ok")
	if f.fake.copies != 1 || f.fake.gets != 0 || res.Object.Sha256 != src.Object.Sha256 {
		t.Fatalf("CRC64 相符应服务端复制: copies=%d gets=%d", f.fake.copies, f.fake.gets)
	}
}

// 源字节被换成等长、同类型的另一份而事实原样留着（真签名、字节数与类型都对）：CRC64 不符，
// 绝不服务端复制，回退原路径读真实字节。
func TestCopyCRC64MismatchFallsBackToSlowPath(t *testing.T) {
	f := newCopyServerFixture(t, nil)
	logs := captureLog(t)
	src := f.sourceWithFacts(t, "video-bytes", testFactsMeta)
	_, srcPath, err := f.srv.objectPath(context.Background(), src.Object.RID)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.mu.Lock()
	f.fake.objs["priv/"+srcPath].data = []byte("VIDEO-BYTES")
	f.fake.mu.Unlock()
	resp := postCopy(t, f.ts.URL, src.Object.RID, "asset_library", "k-crc-bad")
	resp.Body.Close()
	if f.fake.copies != 0 || !strings.Contains(logs.String(), "check=media.copy_fallback reason=source_crc64_mismatch") {
		t.Fatalf("CRC64 不符不能服务端复制: copies=%d logs=%s", f.fake.copies, logs.String())
	}
	// 回退路径真的去读了字节（版本变了，本机缓存不命中，从存储下载）
	if f.fake.gets != 1 {
		t.Fatalf("应回退下载真实字节: gets=%d", f.fake.gets)
	}
	// 目标桶里不能出现带旧事实的对象
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	for k, o := range f.fake.objs {
		if strings.HasPrefix(k, "asset/v1/") && o.meta[ossFactsHeader] == f.fake.objs["priv/"+srcPath].meta[ossFactsHeader] {
			t.Fatalf("目标对象带了源的旧事实: %s", k)
		}
	}
}

// 事实没有 CRC64（绑定上线前写入）或存储 HEAD 不给 CRC64：回退原路径，不按没绑定的事实复制。
func TestCopyFallbackWithoutCRC64(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mut    func(f *copyServerFixture, srcPath string, facts *storedFacts)
		reason string
	}{
		{name: "旧事实无 CRC64", reason: "source_facts_no_crc64", mut: func(f *copyServerFixture, srcPath string, facts *storedFacts) {
			old := *facts
			old.CRC64 = ""
			f.fake.mu.Lock()
			f.fake.objs["priv/"+srcPath].meta = map[string]string{ossFactsHeader: sealFacts(f.srv.id, old)}
			f.fake.mu.Unlock()
		}},
		{name: "HEAD 无 CRC64", reason: "source_crc64_missing", mut: func(f *copyServerFixture, _ string, _ *storedFacts) {
			f.fake.mu.Lock()
			f.fake.noCRC = true
			f.fake.mu.Unlock()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireFFmpeg(t)
			f := newCopyServerFixture(t, nil)
			logs := captureLog(t)
			src, err := f.srv.storeFile(context.Background(), "intermediate", writePNG(t))
			if err != nil {
				t.Fatal(err)
			}
			_, srcPath, err := f.srv.objectPath(context.Background(), src.Object.RID)
			if err != nil {
				t.Fatal(err)
			}
			facts := openFacts(f.srv.id, f.fake.obj("priv/" + srcPath).meta[ossFactsHeader])
			if facts == nil {
				t.Fatal("源应带事实")
			}
			tc.mut(f, srcPath, facts)
			putsBefore := f.fake.puts
			// 回退路径：读字节（本机缓存或下载）、上传，结果与源一致
			res, _ := f.copy(t, src.Object.RID, "asset_library", "k-nocrc")
			if f.fake.copies != 0 || !strings.Contains(logs.String(), "reason="+tc.reason) {
				t.Fatalf("应回退: copies=%d logs=%s", f.fake.copies, logs.String())
			}
			if f.fake.puts != putsBefore+1 || res.Object.Sha256 != src.Object.Sha256 || !stages(res.Timings)["put"] {
				t.Fatalf("回退应上传字节: puts=%d %+v", f.fake.puts-putsBefore, res.Object)
			}
		})
	}
}

// 复制后目标 CRC64 与源不符：回退原路径覆盖目标，结果仍正确。
func TestCopyDestCRC64MismatchFallsBack(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyServerFixture(t, nil)
	logs := captureLog(t)
	png := writePNG(t)
	pngBytes, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}
	src, err := f.srv.storeFile(context.Background(), "intermediate", png)
	if err != nil {
		t.Fatal(err)
	}
	f.fake.badCopyCRC = true
	putsBefore := f.fake.puts
	res, _ := f.copy(t, src.Object.RID, "asset_library", "k-dst-bad")
	if f.fake.copies != 1 || f.fake.puts != putsBefore+1 || !strings.Contains(logs.String(), "reason=dest_crc64_mismatch") {
		t.Fatalf("应回退覆盖目标: copies=%d puts=%d logs=%s", f.fake.copies, f.fake.puts-putsBefore, logs.String())
	}
	if res.Object.Sha256 != src.Object.Sha256 || res.Object.Width != 16 {
		t.Fatalf("结果 = %+v", res.Object)
	}
	dst := f.fake.obj("asset/" + strings.TrimPrefix(res.Url.Value, "https://cdn.example.com/"))
	if dst == nil || string(dst.data) != string(pngBytes) {
		t.Fatalf("目标对象 = %+v", dst)
	}
}
