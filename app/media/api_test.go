package media

import (
	"encoding/json"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- 测试辅助 ----

// apiFixture 一个源站目录 + 一个开了私网访问的服务实例（httptest 源站在 127.0.0.1）。
type apiFixture struct {
	dir     string
	origin  *httptest.Server
	ts      *httptest.Server
	objects string
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	dir := t.TempDir()
	origin := fileServer(t, dir)
	objects := filepath.Join(t.TempDir(), "objects")
	ts := newTestServer(t, Config{AllowPrivateHosts: true, ObjectDir: objects})
	return &apiFixture{dir: dir, origin: origin, ts: ts, objects: objects}
}

func (f *apiFixture) url(name string) map[string]string {
	return map[string]string{"url": f.origin.URL + "/" + name}
}

// decodeMediaResult 取 MediaResult。
func decodeMediaResult(t *testing.T, resp *http.Response) MediaResult {
	t.Helper()
	var res MediaResult
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	return res
}

func objectCount(t *testing.T, objects string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(objects, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	sum, _, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// ---- ingest / probe ----

func TestIngestStoresValidatedClipLocally(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("a.mp4"), "purpose": "user_input"}, testToken, "k-ingest")
	res := decodeMediaResult(t, resp)
	o := res.Object
	if o.Store != storePrivate || o.Width != 320 || !o.HasAudio || o.Mime != "video/mp4" || !near(o.DurationS, 4, 0.2) {
		t.Fatalf("object = %+v", o)
	}
	if res.Url.Kind != "none" {
		t.Fatalf("本地实现 url.kind = %s", res.Url.Kind)
	}
	if got := fileSHA(t, filepath.Join(f.objects, o.Store, filepath.FromSlash(pathOfRID(t, o.RID)))); got != o.Sha256 {
		t.Fatal("落桶内容与 sha256 不一致")
	}
	// 以不同用途再次引用：类别不同，身份（rid）也不同，不会与用户输入混在一起
	res2 := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"rid": o.RID}, "purpose": "intermediate"}, testToken, "k-ingest-2"))
	if res2.Object.RID == o.RID || res2.Object.Class == o.Class {
		t.Fatalf("不同用途必须得到不同身份: %+v vs %+v", res2.Object, o)
	}
}

func TestIngestNeedsIdempotencyKeyAndValidArgs(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("a.mp4"), "purpose": "user_input"}, testToken, "")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")

	resp = postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("a.mp4"), "purpose": "bogus"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")

	resp = postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"url": "http://a", "key": "b"}, "purpose": "user_input"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

func TestIngestRejectsGarbageAsSourceRejected(t *testing.T) {
	f := newAPIFixture(t)
	if err := os.WriteFile(filepath.Join(f.dir, "junk.mp4"), []byte("not a video at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("junk.mp4"), "purpose": "user_input"}, testToken, "k")
	body := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
	if body.Error.Retryable || body.Error.Class != "source" {
		t.Fatalf("素材被拒应永久失败且 class=source: %+v", body.Error)
	}
	if objectCount(t, f.objects) != 0 {
		t.Fatal("被拒的素材不应落桶")
	}
}

func TestProbeReturnsMetaWithoutStoring(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	resp := post(t, f.ts.URL, "/v1/mediasvc/probe", map[string]any{"source": f.url("a.mp4")}, testToken)
	var meta MediaMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || meta.Width != 320 || meta.Height != 240 || !meta.HasAudio || !near(meta.Fps, 10, 0.01) {
		t.Fatalf("probe status=%d meta=%+v", resp.StatusCode, meta)
	}
	if objectCount(t, f.objects) != 0 {
		t.Fatal("probe 不应落桶")
	}
}

// ---- 来源错误分类（重试契约）----

func TestSourceErrorsFollowRetryContract(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)

	// 404：源站永久失败，422，不可重试
	resp := post(t, f.ts.URL, "/v1/mediasvc/probe", map[string]any{"source": map[string]string{"url": f.origin.URL + "/missing.mp4"}}, testToken)
	body := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_unavailable")
	if body.Error.Check != "source.fetch_status=404" || body.Error.Retryable {
		t.Fatalf("404 判据/重试不对: %+v", body.Error)
	}

	// 源站 500：暂时失败，503，可重试，带 Retry-After 之外不泄露源站信息
	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(boom.Close)
	resp = post(t, f.ts.URL, "/v1/mediasvc/probe", map[string]any{"source": map[string]string{"url": boom.URL + "/x.mp4"}}, testToken)
	body = assertAPIError(t, resp, http.StatusServiceUnavailable, "transient")
	if !body.Error.Retryable || strings.Contains(body.Error.Message, boom.URL) {
		t.Fatalf("5xx 应可重试且不回显地址: %+v", body.Error)
	}

	// 回环地址默认被拦（防 SSRF）。这里用不开私网的服务实例
	strict := newTestServer(t, Config{})
	resp = post(t, strict.URL, "/v1/mediasvc/probe", map[string]any{"source": f.url("a.mp4")}, testToken)
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")

	// 非 http 协议
	resp = post(t, strict.URL, "/v1/mediasvc/probe", map[string]any{"source": map[string]string{"url": "file:///etc/passwd"}}, testToken)
	assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
}

// ---- trim / conform ----

func TestTrimCutsAndStores(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 2, "purpose": "intermediate"}, testToken, "k-trim"))
	if !near(res.Object.DurationS, 2, 0.15) || res.Object.Mime != "video/mp4" {
		t.Fatalf("trim = %+v", res.Object)
	}

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 601, "purpose": "intermediate"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	resp = postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 0, "purpose": "intermediate"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

func TestConformShorterThanSecondsIsRejectedNotExtended(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("a.mp4"), "rule": map[string]any{}, "seconds": 10, "purpose": "intermediate"}, testToken, "k")
	body := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_too_short")
	if body.Error.Check != "reference_video.shorter_than_seconds" {
		t.Fatalf("check = %s", body.Error.Check)
	}
	if objectCount(t, f.objects) != 0 {
		t.Fatal("过短拒收时不应落桶")
	}
}

func TestConformStripsAudioSetsFpsAndTrims(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("a.mp4"), "seconds": 2, "purpose": "intermediate",
		"rule": map[string]any{"strip_audio": true, "target_fps": 25}}, testToken, "k-conform"))
	o := res.Object
	if o.HasAudio || !near(o.Fps, 25, 0.1) || !near(o.DurationS, 2, 0.15) {
		t.Fatalf("conform = %+v", o)
	}
}

func TestConformWithoutWorkKeepsSourceBytes(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("a.mp4"), "rule": map[string]any{}, "purpose": "intermediate"}, testToken, "k"))
	if res.Object.Sha256 != fileSHA(t, filepath.Join(f.dir, "a.mp4")) {
		t.Fatal("无需处理时应原样落桶")
	}
}

func TestConformSizeRuleStaysInBounds(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("a.mp4"), "purpose": "intermediate",
		"rule": map[string]any{"min_side": 400, "max_side": 800}}, testToken, "k"))
	o := res.Object
	if o.Width < 400 || o.Width > 800 || o.Height < 400 || o.Width%2 != 0 {
		t.Fatalf("尺寸 %dx%d 不在 min_side=400 max_side=800 内", o.Width, o.Height)
	}
}

func TestConformRatioOutOfRangeRejected(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4")) // 4:3

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/conform", map[string]any{
		"source": f.url("a.mp4"), "purpose": "intermediate",
		"rule": map[string]any{"min_side": 100, "max_ratio": 1.0, "min_ratio": 0.9}}, testToken, "k")
	body := assertAPIError(t, resp, http.StatusUnprocessableEntity, "source_rejected")
	if body.Error.Check != "video.ratio" {
		t.Fatalf("check = %s", body.Error.Check)
	}
}

// ---- frames / cover / watermark / compose / audio / adjust ----

func TestFramesCountRangeAndObjects(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	for _, n := range []int{0, 33} {
		resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/frames", map[string]any{"source": f.url("a.mp4"), "count": n}, testToken, "k")
		assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	}
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/frames", map[string]any{"source": f.url("a.mp4"), "count": 3}, testToken, "k-frames")
	var out FramesResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || len(out.Frames) != 3 || out.Frames[0].Object.Mime != "image/jpeg" {
		t.Fatalf("frames status=%d %+v", resp.StatusCode, out)
	}
}

func TestCoverStoresJPEG(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/cover", map[string]any{
		"source": f.url("a.mp4"), "purpose": "intermediate"}, testToken, "k-cover"))
	if res.Object.Mime != "image/jpeg" {
		t.Fatalf("cover mime = %s", res.Object.Mime)
	}
}

func TestWatermarkNeedsLogoAsInternalConfigError(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/watermark", map[string]any{
		"source": f.url("a.mp4"), "purpose": "intermediate"}, testToken, "k")
	body := assertAPIError(t, resp, http.StatusInternalServerError, "internal")
	if body.Error.Check != "config.watermark_logo_missing" || body.Error.Retryable {
		t.Fatalf("logo 缺失应为不可重试的配置错误: %+v", body.Error)
	}
}

func TestComposeByURLsAndCountLimit(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeClip(t, filepath.Join(f.dir, "a.mp4"), "2", true)
	makeClip(t, filepath.Join(f.dir, "b.mp4"), "3", true)

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/compose", map[string]any{
		"sources": []map[string]string{f.url("a.mp4"), f.url("b.mp4")}, "purpose": "intermediate"}, testToken, "k-compose"))
	if !near(res.Object.DurationS, 5, 0.3) {
		t.Fatalf("合成时长 = %v，期望约 5", res.Object.DurationS)
	}

	limited := newTestServer(t, Config{AllowPrivateHosts: true, MaxComposeInputs: 2})
	resp := postWithKey(t, limited.URL, "/v1/mediasvc/compose", map[string]any{
		"sources": []map[string]string{{"url": "http://a/1.mp4"}, {"url": "http://a/2.mp4"}, {"url": "http://a/3.mp4"}},
		"purpose": "intermediate"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

func TestExtractAudioAndAdjustDuration(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/audio/extract", map[string]any{
		"source": f.url("a.mp4"), "purpose": "intermediate"}, testToken, "k-audio"))
	if res.Object.Mime != "audio/mpeg" || !near(res.Object.DurationS, 4, 0.2) {
		t.Fatalf("extract = %+v", res.Object)
	}

	// 变速 2 倍：时长翻倍，去音轨
	res = decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/adjust-duration", map[string]any{
		"source": f.url("a.mp4"), "factor": 2, "purpose": "intermediate"}, testToken, "k-adj"))
	if !near(res.Object.DurationS, 8, 0.3) || res.Object.HasAudio {
		t.Fatalf("adjust = %+v", res.Object)
	}
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/adjust-duration", map[string]any{
		"source": f.url("a.mp4"), "factor": 0, "purpose": "intermediate"}, testToken, "k")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// ---- delete ----

func TestDeleteProtectsUserInputForever(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	user := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("a.mp4"), "purpose": "user_input"}, testToken, "k-user"))
	// 同内容后来又以 intermediate 落桶：仍然不可删（用户输入标记是粘性的）
	decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"rid": user.Object.RID}, "purpose": "intermediate"}, testToken, "k-user-2"))

	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", map[string]any{"rid": user.Object.RID}, testToken, "k-del")
	assertAPIError(t, resp, http.StatusForbidden, "forbidden_purpose")

	// 中间产物可删：软删除，进入待删除（幂等，二次删除仍是待删除）
	mid := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/trim", map[string]any{
		"source": f.url("a.mp4"), "seconds": 1, "purpose": "intermediate"}, testToken, "k-mid"))
	for i := 0; i < 2; i++ {
		resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", map[string]any{"rid": mid.Object.RID}, testToken, "k-del-mid")
		var out deleteResp
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != http.StatusOK || !out.Deleted || out.State != deleteStatePending {
			t.Fatalf("第 %d 次删除 status=%d deleted=%v state=%s，want 待删除", i+1, resp.StatusCode, out.Deleted, out.State)
		}
	}
}

func TestDeleteRejectsBadObjectRefs(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", map[string]any{"object": map[string]any{"store": "local", "key": "../../etc/passwd"}}, testToken, "k1")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	// 用户输入路径：永不可删，返回 403
	resp = postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", map[string]any{"object": map[string]any{"store": "private", "key": "v1/user/" + strings.Repeat("a", 26)}}, testToken, "k2")
	assertAPIError(t, resp, http.StatusForbidden, "forbidden_purpose")
}

// ---- 公共规则 ----

func TestTargetSizeRules(t *testing.T) {
	if w, h, err := targetSize(1920, 1080, RuleSpec{}); err != nil || w != 1920 || h != 1080 {
		t.Fatalf("identity = %d %d %v", w, h, err)
	}
	// 与网关 VideoConformance.target_size 逐例一致（Python 参考实现核对过）
	w, h, err := targetSize(3840, 2160, RuleSpec{MaxPixels: 2086876})
	if err != nil || w != 1924 || h != 1082 {
		t.Fatalf("max_pixels: %d %d %v", w, h, err)
	}
	w, h, err = targetSize(3840, 2160, RuleSpec{MaxSide: 1000})
	if err != nil || math.Abs(float64(w)/float64(h)-16.0/9.0) > 0.01 {
		t.Fatalf("max_side keeps ratio: %d %d %v", w, h, err)
	}
	if _, _, err := targetSize(3000, 1000, RuleSpec{MinSide: 320, MaxRatio: 2.5}); err == nil {
		t.Fatal("比例 3:1 应越界")
	}
	w, h, err = targetSize(320, 240, RuleSpec{MinSide: 640})
	if err != nil || min(w, h) < 640 {
		t.Fatalf("min_side: %d %d %v", w, h, err)
	}
}

// ---- 本地对象存储 ----

func TestLocalStorePathsAndDeleteProtection(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(src, []byte("same content"), 0o600); err != nil {
		t.Fatal(err)
	}
	const user = "v1/user/abcdefghijklmnopqrstuvwxyz"
	const work = "v1/work/abcdefghijklmnopqrstuvwxyz"
	obj, err := st.Put(t.Context(), storePrivate, work, src, "video/mp4")
	if err != nil || obj.Key != work || obj.Size != int64(len("same content")) {
		t.Fatalf("Put = %+v %v", obj, err)
	}
	if _, err := st.Fetch(t.Context(), storePrivate, work, ""); err != nil {
		t.Fatalf("Fetch 写入的对象: %v", err)
	}
	// 路径校验：拒绝路径穿越、非法类别与非法键
	for _, p := range []string{"../etc/passwd", "v1/video/abcdefghijklmnopqrstuvwxyz", "/abs", "v1/work/XYZ"} {
		if _, err := st.Fetch(t.Context(), storePrivate, p, ""); err == nil {
			t.Fatalf("路径 %q 应被拒", p)
		}
	}
	if _, err := st.Fetch(t.Context(), storePrivate, "v1/work/"+strings.Repeat("a", 26), ""); err != errObjectNotFound {
		t.Fatalf("缺失对象 err = %v", err)
	}
	// 删除：work 可删；user 与历史路径不可删
	if ok, err := st.Delete(t.Context(), storePrivate, work); err != nil || !ok {
		t.Fatalf("删除 work = %v %v", ok, err)
	}
	if _, err := st.Put(t.Context(), storePrivate, user, src, "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(t.Context(), storePrivate, user); err != errUserInputProtected {
		t.Fatalf("删除 user = %v，want errUserInputProtected", err)
	}
	if _, err := st.Delete(t.Context(), storePrivate, "sha256/"+strings.Repeat("0", 64)+".mp4"); err != errUserInputProtected {
		t.Fatalf("删除历史路径 = %v，want errUserInputProtected", err)
	}
}

// 水印：配了 logo 时出结果；logo 只在服务端配置，调用方不传。
func TestWatermarkOverlaysLogoWhenConfigured(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	makeAVClip(t, filepath.Join(dir, "a.mp4"))
	logo := filepath.Join(dir, "logo.png")
	if out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=red:s=32x32",
		"-frames:v", "1", logo).CombinedOutput(); err != nil {
		t.Fatalf("生成 logo 失败: %v: %s", err, out)
	}
	origin := fileServer(t, dir)
	ts := newTestServer(t, Config{AllowPrivateHosts: true, LogoPath: logo, ObjectDir: filepath.Join(t.TempDir(), "o")})

	res := decodeMediaResult(t, postWithKey(t, ts.URL, "/v1/mediasvc/watermark", map[string]any{
		"source": map[string]string{"url": origin.URL + "/a.mp4"}, "purpose": "intermediate"}, testToken, "k-wm"))
	if res.Object.Mime != "video/mp4" || !near(res.Object.DurationS, 4, 0.2) {
		t.Fatalf("watermark = %+v", res.Object)
	}
}

// 从系统内 rid 复制到第三方桶：类别变为 deliver，rid 与原对象不同，原用户输入保持不变。
func TestIngestFromRidCopiesToThirdPartyStore(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))

	user := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": f.url("a.mp4"), "purpose": "user_input"}, testToken, "k-user-src"))
	if user.Object.Class != classUser {
		t.Fatalf("用户输入 class = %s, want user", user.Object.Class)
	}

	out := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/ingest", map[string]any{
		"source": map[string]string{"rid": user.Object.RID}, "purpose": "third_party"}, testToken, "k-copy"))
	if out.Object.Class != classDeliver || out.Object.Store != storeThirdParty {
		t.Fatalf("第三方副本 class=%s store=%s, want deliver/third_party", out.Object.Class, out.Object.Store)
	}
	if out.Object.RID == user.Object.RID {
		t.Fatal("副本必须有独立的 rid")
	}
	if out.Object.Sha256 != user.Object.Sha256 {
		t.Fatalf("副本内容 sha256 与原件不一致: %s vs %s", out.Object.Sha256, user.Object.Sha256)
	}
	// 原用户输入不受影响，仍然不可删
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/delete", map[string]any{"rid": user.Object.RID}, testToken, "k-del-user")
	assertAPIError(t, resp, http.StatusForbidden, "forbidden_purpose")
}

// pathOfRID 由 rid 推导桶内路径（业务响应不返回对象键，测试按同样的规则算出来）。
func pathOfRID(t *testing.T, rid string) string {
	t.Helper()
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	_, class, err := ParseRID(rid)
	if err != nil {
		t.Fatal(err)
	}
	return storePath(class, id.ObjectKey(rid))
}
