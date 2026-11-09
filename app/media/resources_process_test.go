package media

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// SignedProcessURL 的假实现：把样式放进地址，便于断言处理器是否把 process 传到了存储层。
func (f *fakeResourceStore) SignedProcessURL(_ context.Context, store, path, style string, ttl time.Duration) (StoredURL, error) {
	return StoredURL{Kind: "signed", Value: "https://signed.example/" + store + "/" + path + "?x-oss-process=" + style + "&ttl=" + ttl.String(),
		ExpiresAt: "2030-01-01T00:00:00Z"}, nil
}

type resourceErrBody struct {
	Error struct {
		Code  string `json:"code"`
		Check string `json:"check"`
	} `json:"error"`
}

// 处理样式：signed 时地址带 x-oss-process=<样式>；其余字段与原来一致。
func TestResourceURLProcessSignedCarriesStyle(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "deliver", "job-p1")
	fake.putIndexed(storePrivate, classDeliver, s.id.ObjectKey(rid), 42)

	style := "work-thumb_1.0/a-b"
	resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "deliver", "store": "private",
		"mode": "signed", "ttl_seconds": 600, "process": style})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out resourceURLResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Mode != "signed" || out.Size != 42 || out.ExpiresAt == "" {
		t.Fatalf("resp = %+v", out)
	}
	if !strings.Contains(out.URL, "x-oss-process=") {
		t.Fatalf("地址缺少 x-oss-process: %s", out.URL)
	}
	u, err := url.Parse(out.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("x-oss-process"); got != style {
		t.Fatalf("x-oss-process = %q, want %q", got, style)
	}
}

// 处理样式只允许 signed：public 与其它模式返回 400 request.process_mode。
func TestResourceURLProcessRequiresSigned(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "deliver", "job-p2")
	fake.putIndexed(storeThirdParty, classDeliver, s.id.ObjectKey(rid), 42)

	for _, mode := range []string{"public", "bogus", ""} {
		resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "deliver", "store": "third_party",
			"mode": mode, "process": "thumb"})
		var body resourceErrBody
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusBadRequest || body.Error.Check != "request.process_mode" {
			t.Fatalf("mode %q: status = %d check = %q, want 400 request.process_mode", mode, resp.StatusCode, body.Error.Check)
		}
	}
}

// 样式字符越界：返回 400 request.process_style。允许的字符集见 validProcessStyle。
func TestResourceURLProcessRejectsBadCharacters(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "work", "job-p3")
	fake.putIndexed(storePrivate, classWork, s.id.ObjectKey(rid), 42)

	bad := []string{"a b", "a?x=1", "a&b=c", "a%2Fb", "a=b", "a;b", `a"b`, "中文", "a+b", "a#b"}
	for _, style := range bad {
		resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "work", "store": "private",
			"mode": "signed", "process": style})
		var body resourceErrBody
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusBadRequest || body.Error.Check != "request.process_style" {
			t.Fatalf("style %q: status = %d check = %q, want 400 request.process_style", style, resp.StatusCode, body.Error.Check)
		}
	}
}

// 回归：不传 process（或传空串）时，地址与原来的 SignedURL 完全一致，且不带 x-oss-process。
func TestResourceURLProcessAbsentIsUnchanged(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "deliver", "job-p4")
	key := s.id.ObjectKey(rid)
	fake.putIndexed(storePrivate, classDeliver, key, 7)
	_, path, err := s.objectPath(context.Background(), rid)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fake.SignedURL(context.Background(), storePrivate, path, 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{
		{"rid": rid, "class": "deliver", "store": "private", "mode": "signed", "ttl_seconds": 600},
		{"rid": rid, "class": "deliver", "store": "private", "mode": "signed", "ttl_seconds": 600, "process": ""},
	} {
		resp := postResourceURL(t, ts.URL, body)
		var out resourceURLResp
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.URL != want.Value {
			t.Fatalf("url = %s, want %s", out.URL, want.Value)
		}
		if strings.Contains(out.URL, "x-oss-process") {
			t.Fatalf("无 process 的地址不应带 x-oss-process: %s", out.URL)
		}
	}
}

// OSS 签名：样式进入签名，改样式改签名；地址带 x-oss-process；签名可以按 canonical resource 重算。
func TestOSSSignedProcessURLSignsTheStyle(t *testing.T) {
	st, _ := newTestOSS(t)
	const nowUnix = int64(1800000000)
	st.now = func() time.Time { return time.Unix(nowUnix, 0) }
	ctx := context.Background()
	ttl := 10 * time.Minute
	exp := strconv.FormatInt(nowUnix+int64(ttl/time.Second), 10)

	plain, err := st.SignedURL(ctx, storePrivate, ossWorkPath, ttl)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.SignedProcessURL(ctx, storePrivate, ossWorkPath, "thumb-small", ttl)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.SignedProcessURL(ctx, storePrivate, ossWorkPath, "thumb-large", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != "signed" || !strings.HasSuffix(a.Value, "&x-oss-process=thumb-small") {
		t.Fatalf("url = %s", a.Value)
	}
	qa, _ := url.Parse(a.Value)
	qb, _ := url.Parse(b.Value)
	sigA, sigB, sigPlain := qa.Query().Get("Signature"), qb.Query().Get("Signature"), mustSig(t, plain.Value)
	if sigA == sigB || sigA == sigPlain {
		t.Fatalf("签名没有随样式变化: plain=%s a=%s b=%s", sigPlain, sigA, sigB)
	}
	if qa.Query().Get("x-oss-process") != "thumb-small" || qa.Query().Get("Expires") != exp {
		t.Fatalf("query = %v", qa.Query())
	}
	// 按 canonical resource "/<bucket>/<key>?x-oss-process=<style>" 重算 HMAC-SHA1
	want := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath+"?x-oss-process=thumb-small")
	if sigA != want {
		t.Fatalf("签名 = %s, want %s", sigA, want)
	}
	// 没有样式的签名 canonical resource 里不能出现子资源
	wantPlain := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath)
	if sigPlain != wantPlain {
		t.Fatalf("无样式签名 = %s, want %s", sigPlain, wantPlain)
	}
}

// 免费档参数水印：内联的 image/watermark 处理串（逗号分隔、文字为 URL 安全 Base64）被接受，原样进入签名；
// 去掉 x-oss-process 后服务端会校验的「无子资源」签名与之不同（即去参数的地址 403）。
func TestOSSSignedProcessURLSignsInlineWatermark(t *testing.T) {
	st, _ := newTestOSS(t)
	const nowUnix = int64(1800000000)
	st.now = func() time.Time { return time.Unix(nowUnix, 0) }
	ttl := 10 * time.Minute
	exp := strconv.FormatInt(nowUnix+int64(ttl/time.Second), 10)
	const wm = "image/watermark,image_d2F0ZXJtYXJrL2xvZ28ucG5nP3gtb3NzLXByb2Nlc3M9aW1hZ2UvcmVzaXplLFBfMTU,text_5bCP5p-SQUk,size_56,shadow_50,order_1,align_1,interval_10,t_85,g_se,x_40,y_40"
	if !validProcessStyle(wm) {
		t.Fatal("内联水印处理串应被接受")
	}
	got, err := st.SignedProcessURL(context.Background(), storePrivate, ossWorkPath, wm, ttl)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.Parse(got.Value)
	if q.Query().Get("x-oss-process") != wm {
		t.Fatalf("x-oss-process = %q", q.Query().Get("x-oss-process"))
	}
	sig := q.Query().Get("Signature")
	if want := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath+"?x-oss-process="+wm); sig != want {
		t.Fatalf("签名 = %s, want %s", sig, want)
	}
	if stripped := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath); sig == stripped {
		t.Fatal("去掉处理参数后签名不应仍然成立")
	}
}

// 回归：空样式的 SignedProcessURL 与 SignedURL 逐字节一致；非法样式在存储层也被拒绝。
func TestOSSSignedURLWithoutStyleIsUnchanged(t *testing.T) {
	st, _ := newTestOSS(t)
	const nowUnix = int64(1800000000)
	st.now = func() time.Time { return time.Unix(nowUnix, 0) }
	ctx := context.Background()
	plain, err := st.SignedURL(ctx, storePrivate, ossWorkPath, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := st.SignedProcessURL(ctx, storePrivate, ossWorkPath, "", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if plain != empty {
		t.Fatalf("空样式改变了地址:\n%+v\n%+v", plain, empty)
	}
	exp := strconv.FormatInt(nowUnix+600, 10)
	q := url.Values{}
	q.Set("OSSAccessKeyId", "AK")
	q.Set("Expires", exp)
	q.Set("Signature", sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath))
	if want := "https://" + host(st.private) + "/" + ossWorkPath + "?" + q.Encode(); plain.Value != want {
		t.Fatalf("url = %s\nwant %s", plain.Value, want)
	}
	if _, err := st.SignedProcessURL(ctx, storePrivate, ossWorkPath, "a b", time.Minute); err == nil {
		t.Fatal("非法样式应被拒绝")
	}
}

func mustSig(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("Signature")
}

// 本地存储做不了处理参数：要求样式时必须报错，不能退化成干净地址。
func TestLocalSignedProcessURLRefusesStyle(t *testing.T) {
	l := &LocalStore{dir: t.TempDir()}
	if _, err := l.SignedProcessURL(context.Background(), storePrivate, "v1/work/a.png", "style/wm", time.Minute); err == nil {
		t.Fatal("带样式应返回错误")
	}
	if u, err := l.SignedProcessURL(context.Background(), storePrivate, "v1/work/a.png", "", time.Minute); err != nil || u.Kind != "none" {
		t.Fatalf("空样式等同 SignedURL: %+v %v", u, err)
	}
}
