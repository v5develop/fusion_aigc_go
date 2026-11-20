package media

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOSS 假的 OSS：按 Host 的第一段区分桶（<bucket>.<endpoint>），只记录对象和是否带了签名头。
type fakeOSS struct {
	mu     sync.Mutex
	objs   map[string][]byte
	signed map[string]bool
	meta   map[string]string // 对象元数据 x-oss-meta-delete-after
	reason map[string]string // 对象元数据 x-oss-meta-delete-reason
}

func (f *fakeOSS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := strings.SplitN(r.Host, ".", 2)[0]
	k := bucket + "/" + strings.TrimPrefix(r.URL.Path, "/")
	// 桶根的 GET 是列举（ListObjects v1）
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		f.list(w, bucket, r.URL.Query())
		return
	}
	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("x-oss-forbid-overwrite") == "true" {
			if _, ok := f.objs[k]; ok {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		data, _ := io.ReadAll(r.Body)
		f.objs[k] = data
		f.signed[k] = r.Header.Get("Authorization") != ""
		if m := r.Header.Get("x-oss-meta-delete-after"); m != "" {
			f.meta[k] = m
		}
		if m := r.Header.Get("x-oss-meta-delete-reason"); m != "" {
			f.reason[k] = m
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.signed[k] = r.Header.Get("Authorization") != ""
		data, ok := f.objs[k]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodHead:
		if _, ok := f.objs[k]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if m, ok := f.meta[k]; ok {
			w.Header().Set("X-Oss-Meta-Delete-After", m)
		}
		if m, ok := f.reason[k]; ok {
			w.Header().Set("X-Oss-Meta-Delete-Reason", m)
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if _, ok := f.objs[k]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.objs, k)
		delete(f.meta, k)
		delete(f.reason, k)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// list 按 prefix / marker / max-keys 列举桶内对象（调用方已持锁）。
func (f *fakeOSS) list(w http.ResponseWriter, bucket string, q url.Values) {
	var keys []string
	for k := range f.objs {
		if !strings.HasPrefix(k, bucket+"/") {
			continue
		}
		key := strings.TrimPrefix(k, bucket+"/")
		if strings.HasPrefix(key, q.Get("prefix")) && key > q.Get("marker") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	limit, _ := strconv.Atoi(q.Get("max-keys"))
	truncated := false
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
		truncated = true
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	b.WriteString("<IsTruncated>" + strconv.FormatBool(truncated) + "</IsTruncated>")
	for _, key := range keys {
		b.WriteString("<Contents><Key>" + key + "</Key></Contents>")
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(b.String()))
}

// rewriteTransport 把所有请求转到假服务，保留原 Host（桶名）。
type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Host = r.URL.Host
	r2.URL.Scheme = "http"
	r2.URL.Host = rt.target
	return http.DefaultTransport.RoundTrip(r2)
}

func newTestOSS(t *testing.T) (*OSSStore, *fakeOSS) {
	t.Helper()
	fake := &fakeOSS{objs: map[string][]byte{}, signed: map[string]bool{}, meta: map[string]string{}, reason: map[string]string{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	st, err := NewOSSStore(OSSConfig{
		Enabled:        true,
		PresignMinutes: 30,
		Private:        OSSBucket{Endpoint: "https://oss.example.com", Bucket: "priv", AccessKey: "AK", SecretKey: "SK"},
		AssetLibrary:   OSSBucket{Endpoint: "https://oss.example.com", Bucket: "asset", AccessKey: "AK", SecretKey: "SK", CustomDomain: "cdn.example.com"},
		ThirdParty:     OSSBucket{Endpoint: "https://oss.example.com", Bucket: "third", AccessKey: "AK", SecretKey: "SK"},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.hc = &http.Client{Transport: rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://")}}
	return st, fake
}

func writeFixture(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const ossWorkPath = "v1/work/2026/10/abcdefghijklmnopqrstuvwxyz"
const ossUserPath = "v1/user/2026/10/abcdefghijklmnopqrstuvwxyz"
const ossAssetPath = "v1/asset/2026/10/abcdefghijklmnopqrstuvwxyz"

// 中间产物：写私有桶，签名读取，删除后不可再读。
func TestOSSPutFetchDeleteRoundTrip(t *testing.T) {
	st, fake := newTestOSS(t)
	ctx := context.Background()
	obj, err := st.Put(ctx, storePrivate, ossWorkPath, writeFixture(t, "clip-bytes"), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Store != storePrivate || obj.Key != ossWorkPath {
		t.Fatalf("object = %+v", obj)
	}
	path, err := st.Fetch(ctx, storePrivate, obj.Key, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "clip-bytes" {
		t.Fatalf("fetched = %q", got)
	}
	if !fake.signed["priv/"+obj.Key] {
		t.Fatal("私有桶的写入和读取必须带签名")
	}
	deleted, err := st.Delete(ctx, storePrivate, obj.Key)
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	if _, err := st.Fetch(ctx, storePrivate, obj.Key, t.TempDir()); err != errObjectNotFound {
		t.Fatalf("Fetch after delete err = %v", err)
	}
}

// 用户输入（user/ 前缀）：永不可删。
func TestOSSUserInputIsProtectedFromDelete(t *testing.T) {
	st, _ := newTestOSS(t)
	ctx := context.Background()
	if _, err := st.Put(ctx, storePrivate, ossUserPath, writeFixture(t, "user-clip"), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(ctx, storePrivate, ossUserPath); err != errUserInputProtected {
		t.Fatalf("Delete user input err = %v, want errUserInputProtected", err)
	}
}

// 素材库产物写素材库桶，返回公有地址（优先自定义域名），不签名。
func TestOSSAssetLibraryGoesToItsOwnBucketWithCustomDomain(t *testing.T) {
	st, fake := newTestOSS(t)
	ctx := context.Background()
	obj, err := st.Put(ctx, storeAsset, ossAssetPath, writeFixture(t, "cover-bytes"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Store != storeAsset {
		t.Fatalf("store = %s, want asset_library", obj.Store)
	}
	// 写入任何桶都必须签名；公有桶的读取不签名
	if !fake.signed["asset/"+obj.Key] {
		t.Fatal("写入公有桶也必须签名")
	}
	if _, err := st.Fetch(ctx, storeAsset, obj.Key, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if fake.signed["asset/"+obj.Key] {
		t.Fatal("公有桶的读取不应该带签名")
	}
	u, err := st.PublicURL(ctx, storeAsset, obj.Key)
	if err != nil {
		t.Fatal(err)
	}
	if u.Kind != "public" || u.Value != "https://cdn.example.com/"+obj.Key {
		t.Fatalf("public url = %+v", u)
	}
}

// 私有桶的签名地址：带 AccessKey、过期时间和签名，过期时间在将来；私有桶没有公有地址。
func TestOSSPrivateURLIsPresignedWithExpiry(t *testing.T) {
	st, _ := newTestOSS(t)
	ctx := context.Background()
	u, err := st.SignedURL(ctx, storePrivate, ossWorkPath, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(u.Value)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if u.Kind != "signed" || q.Get("OSSAccessKeyId") != "AK" || q.Get("Signature") == "" || q.Get("Expires") == "" {
		t.Fatalf("url = %+v query=%v", u, q)
	}
	exp, err := time.Parse(time.RFC3339, u.ExpiresAt)
	if err != nil || !exp.After(time.Now()) {
		t.Fatalf("expires_at = %q (%v)", u.ExpiresAt, err)
	}
	if pu, _ := st.PublicURL(ctx, storePrivate, ossWorkPath); pu.Kind != "none" {
		t.Fatalf("私有桶不应有公有地址: %+v", pu)
	}
}

// 直传签名：PUT 地址带签名与过期时间；字节可以直接 PUT 上去。
func TestOSSSignedPutURL(t *testing.T) {
	st, _ := newTestOSS(t)
	u, err := st.SignedPutURL(context.Background(), storePrivate, ossUserPath, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(u.Value)
	if u.Kind != "signed" || parsed.Query().Get("Signature") == "" || parsed.Query().Get("OSSAccessKeyId") != "AK" {
		t.Fatalf("put url = %+v", u)
	}
}

// 配置不完整或有效期越界，启动时就拒绝。
func TestNewOSSStoreRejectsBadConfig(t *testing.T) {
	full := OSSBucket{Endpoint: "https://oss.example.com", Bucket: "b", AccessKey: "AK", SecretKey: "SK"}
	if _, err := NewOSSStore(OSSConfig{Private: OSSBucket{Bucket: "b"}, AssetLibrary: full, ThirdParty: full, PresignMinutes: 30}); err == nil {
		t.Fatal("缺 Endpoint/AccessKey 的私有桶必须拒绝")
	}
	if _, err := NewOSSStore(OSSConfig{Private: full, AssetLibrary: full, ThirdParty: full, PresignMinutes: 2000}); err == nil {
		t.Fatal("有效期超过 24 小时必须拒绝")
	}
}

// 画布缩略图处理串（canvas config.DefaultThumbProcess）：样式字符合法，签名的 canonical resource 包含该子资源，
// 地址里的 x-oss-process 与签名用的值逐字相同；去掉处理串后签名不再匹配（前端不能自己拼 / 摘）。
func TestOSSSignedProcessURLThumbStyleInSignature(t *testing.T) {
	st, _ := newTestOSS(t)
	const style = "image/resize,m_lfit,w_480/format,webp/quality,q_80"
	u, err := st.SignedProcessURL(context.Background(), storePrivate, ossWorkPath, style, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(u.Value, "&x-oss-process="+style) {
		t.Fatalf("处理串应原样（不编码）放在最后: %s", u.Value)
	}
	q, err := url.Parse(u.Value)
	if err != nil {
		t.Fatal(err)
	}
	exp := q.Query().Get("Expires")
	want := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath+"?x-oss-process="+style)
	if got := q.Query().Get("Signature"); got != want {
		t.Fatalf("签名未包含处理串: got %s want %s", got, want)
	}
	if bare := sign("SK", "GET\n\n\n"+exp+"\n/priv/"+ossWorkPath); bare == want {
		t.Fatal("带 / 不带处理串的签名不应相同")
	}
}
