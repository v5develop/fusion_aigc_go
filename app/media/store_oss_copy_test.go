package media

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// copyOSS 支持服务端复制的假 OSS：对象带内容类型与 x-oss-meta-*，HEAD 返回 Content-Length / ETag；
// 支持 CopyObject、InitiateMultipartUpload / UploadPartCopy / CompleteMultipartUpload / Abort；
// 每个签名请求都按 OSS v1 规则复算签名（含子资源与 x-oss-* 头），不符记入 sigErrors。
type copyOSS struct {
	mu        sync.Mutex
	secrets   map[string]string // AK → SK
	objs      map[string]*fakeObj
	uploads   map[string]*fakeUpload
	nextID    int
	denyCopy  bool     // 带 x-oss-copy-source 的请求一律 403 AccessDenied（目标密钥读不了源桶）
	copyFail  int      // 非 0 时带 x-oss-copy-source 的请求一律返回该状态码
	ifMatches []string // 复制请求带的 x-oss-copy-source-if-match
	failPart  int      // 第几片 UploadPartCopy 返回 500（0 不失败）
	sigErrors []string
	gets      int // 对象 GET 次数（下载字节）
	puts      int // 带字节的 PUT 次数（上传字节）
	copies    int // CopyObject 次数
	partReqs  []string
	aborted   int
	copyAKs   []string // 复制请求使用的 AccessKey
}

type fakeObj struct {
	data   []byte
	ctype  string
	meta   map[string]string
	sclass string // x-oss-storage-class（空 = 不返回）
}

type fakeUpload struct {
	key   string
	ctype string
	meta  map[string]string
	parts map[int][]byte
}

func newCopyOSS() *copyOSS {
	return &copyOSS{secrets: map[string]string{"AK": "SK", "AK2": "SK2"}, objs: map[string]*fakeObj{}, uploads: map[string]*fakeUpload{}}
}

func etagOf(b []byte) string {
	s := md5.Sum(b)
	return `"` + strings.ToUpper(hex.EncodeToString(s[:])) + `"`
}

// verify 复算 OSS v1 签名。
func (f *copyOSS) verify(r *http.Request, bucket, key string) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		if r.Method != http.MethodGet {
			f.sigErrors = append(f.sigErrors, r.Method+" 未签名")
		}
		return
	}
	ak, sig, _ := strings.Cut(strings.TrimPrefix(auth, "OSS "), ":")
	var names []string
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-oss-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)
	var hs strings.Builder
	for _, n := range names {
		hs.WriteString(n + ":" + r.Header.Get(n) + "\n")
	}
	resource := "/" + bucket + "/" + key
	q := r.URL.Query()
	var subs []string
	for _, n := range []string{"partNumber", "uploadId", "uploads"} {
		if _, ok := q[n]; ok {
			if v := q.Get(n); v != "" {
				subs = append(subs, n+"="+v)
			} else {
				subs = append(subs, n)
			}
		}
	}
	if len(subs) > 0 {
		resource += "?" + strings.Join(subs, "&")
	}
	toSign := r.Method + "\n" + r.Header.Get("Content-MD5") + "\n" + r.Header.Get("Content-Type") + "\n" + r.Header.Get("Date") + "\n" + hs.String() + resource
	if sign(f.secrets[ak], toSign) != sig {
		f.sigErrors = append(f.sigErrors, fmt.Sprintf("%s %s 签名不符", r.Method, resource))
	}
}

func (f *copyOSS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := strings.SplitN(r.Host, ".", 2)[0]
	key := strings.TrimPrefix(r.URL.Path, "/")
	k := bucket + "/" + key
	f.verify(r, bucket, key)
	q := r.URL.Query()
	src := r.Header.Get("x-oss-copy-source")
	if src != "" {
		ak, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "OSS "), ":")
		f.copyAKs = append(f.copyAKs, ak)
		if f.denyCopy {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		if f.copyFail != 0 {
			w.WriteHeader(f.copyFail)
			_, _ = io.WriteString(w, "<Error><Code>Injected</Code></Error>")
			return
		}
		im := r.Header.Get("x-oss-copy-source-if-match")
		f.ifMatches = append(f.ifMatches, im)
		if so := f.objs[strings.TrimPrefix(src, "/")]; so != nil && im != "" && im != etagOf(so.data) {
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			return
		}
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.nextID++
		id := fmt.Sprintf("UP%04d", f.nextID)
		f.uploads[id] = &fakeUpload{key: k, ctype: r.Header.Get("Content-Type"), meta: userMeta(r.Header), parts: map[int][]byte{}}
		_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>"+id+"</UploadId></InitiateMultipartUploadResult>")
	case r.Method == http.MethodPut && q.Has("partNumber"):
		up := f.uploads[q.Get("uploadId")]
		n, _ := strconv.Atoi(q.Get("partNumber"))
		rng := r.Header.Get("x-oss-copy-source-range")
		f.partReqs = append(f.partReqs, rng)
		if up == nil || (f.failPart != 0 && n == f.failPart) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "<Error><Code>InternalError</Code></Error>")
			return
		}
		so := f.objs[strings.TrimPrefix(src, "/")]
		if so == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		var a, b int
		_, _ = fmt.Sscanf(rng, "bytes=%d-%d", &a, &b)
		up.parts[n] = append([]byte(nil), so.data[a:b+1]...)
		_, _ = io.WriteString(w, "<CopyPartResult><ETag>"+etagOf(up.parts[n])+"</ETag></CopyPartResult>")
	case r.Method == http.MethodPost && q.Has("uploadId"):
		up := f.uploads[q.Get("uploadId")]
		var body struct {
			Parts []struct {
				N    int    `xml:"PartNumber"`
				ETag string `xml:"ETag"`
			} `xml:"Part"`
		}
		raw, _ := io.ReadAll(r.Body)
		if up == nil || xml.Unmarshal(raw, &body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var data []byte
		for i, p := range body.Parts {
			if p.N != i+1 || p.ETag != etagOf(up.parts[p.N]) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "<Error><Code>InvalidPart</Code></Error>")
				return
			}
			data = append(data, up.parts[p.N]...)
		}
		f.objs[up.key] = &fakeObj{data: data, ctype: up.ctype, meta: up.meta}
		delete(f.uploads, q.Get("uploadId"))
		_, _ = io.WriteString(w, "<CompleteMultipartUploadResult><ETag>\"MULTI-"+strconv.Itoa(len(body.Parts))+"\"</ETag></CompleteMultipartUploadResult>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && src != "":
		f.copies++
		so := f.objs[strings.TrimPrefix(src, "/")]
		if so == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		f.objs[k] = &fakeObj{data: so.data, ctype: so.ctype, meta: so.meta}
		_, _ = io.WriteString(w, "<CopyObjectResult><ETag>"+etagOf(so.data)+"</ETag></CopyObjectResult>")
	case r.Method == http.MethodPut:
		if r.Header.Get("x-oss-forbid-overwrite") == "true" && f.objs[k] != nil {
			w.WriteHeader(http.StatusConflict)
			return
		}
		data, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(key, "v1/") {
			f.puts++
		}
		f.objs[k] = &fakeObj{data: data, ctype: r.Header.Get("Content-Type"), meta: userMeta(r.Header)}
		w.Header().Set("ETag", etagOf(data))
	case r.Method == http.MethodGet:
		o := f.objs[k]
		if o == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(key, "v1/") {
			f.gets++
		}
		_, _ = w.Write(o.data)
	case r.Method == http.MethodHead:
		o := f.objs[k]
		if o == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for mk, mv := range o.meta {
			w.Header().Set(mk, mv)
		}
		if o.ctype != "" {
			w.Header().Set("Content-Type", o.ctype)
		}
		if o.sclass != "" {
			w.Header().Set("x-oss-storage-class", o.sclass)
		}
		w.Header().Set("ETag", etagOf(o.data))
		w.Header().Set("Content-Length", strconv.Itoa(len(o.data)))
	case r.Method == http.MethodDelete:
		if f.objs[k] == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.objs, k)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func userMeta(h http.Header) map[string]string {
	m := map[string]string{}
	for k, vs := range h {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-oss-meta-") {
			m[lk] = vs[0]
		}
	}
	return m
}

func (f *copyOSS) obj(k string) *fakeObj {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[k]
}

// newCopyTestOSS 三个桶同地域（私有桶走内网 endpoint，仍算同地域）；mut 可改配置。
func newCopyTestOSS(t *testing.T, mut func(*OSSConfig)) (*OSSStore, *copyOSS) {
	t.Helper()
	fake := newCopyOSS()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cfg := OSSConfig{
		Enabled:        true,
		PresignMinutes: 30,
		Private:        OSSBucket{Endpoint: "https://oss-cn-shanghai-internal.aliyuncs.com", Bucket: "priv", AccessKey: "AK", SecretKey: "SK"},
		AssetLibrary:   OSSBucket{Endpoint: "https://oss-cn-shanghai.aliyuncs.com", Bucket: "asset", AccessKey: "AK", SecretKey: "SK", CustomDomain: "cdn.example.com"},
		ThirdParty:     OSSBucket{Endpoint: "https://oss-cn-shanghai.aliyuncs.com", Bucket: "third", AccessKey: "AK", SecretKey: "SK"},
	}
	if mut != nil {
		mut(&cfg)
	}
	st, err := NewOSSStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	st.hc = &http.Client{Transport: rewriteTransport{target: strings.TrimPrefix(srv.URL, "http://")}}
	t.Cleanup(func() {
		if len(fake.sigErrors) > 0 {
			t.Errorf("签名错误: %v", fake.sigErrors)
		}
	})
	return st, fake
}

var testFactsMeta = MediaMeta{Mime: "video/mp4", DurationS: 6.5, Width: 720, Height: 1280, Fps: 30, HasAudio: true,
	PixFmt: "yuv420p", ColorSpace: "bt709"}

// putWithFacts 按 storeMeta 的方式落桶：登记摘要与事实后 Put。
func putWithFacts(t *testing.T, st ObjectStore, store, path, content string, meta MediaMeta) StoredObject {
	t.Helper()
	local := writeFixture(t, content)
	sum, size, err := hashFile(local)
	if err != nil {
		t.Fatal(err)
	}
	meta.Size = size
	sealed := sealFacts(testFactsID, storedFacts{V: storedFactsVersion, Sha256: sum, Size: size, Meta: meta})
	ctx := withSealedFacts(withKnownDigest(context.Background(), local, sum, size), local, sum, size, sealed)
	obj, err := st.Put(ctx, store, path, local, meta.Mime)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// testFactsID 与 copyServerFixture 的服务同一密钥（testSecret），签出的事实两边都认。
var testFactsID = func() *identity {
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		panic(err)
	}
	return id
}()

// trustedHead HEAD 源对象并按服务的规则验签、核对事实（Server.trustedFacts 的同一实现）。
func trustedHead(t *testing.T, st *OSSStore, store, path string) (objectHead, string) {
	t.Helper()
	h, err := st.ObjectFacts(context.Background(), store, path)
	if err != nil {
		t.Fatal(err)
	}
	f, reason := trustFacts(testFactsID, h)
	h.Facts = f
	return h, reason
}

const ossAssetPath2 = "v1/asset/2026/11/zyxwvutsrqponmlkjihgfedcba"

// 落桶写事实：Put 带 x-oss-meta-media-facts（进入签名），HEAD 读回 sha256 与全部元数据。
func TestOSSPutWritesFactsAndHeadReadsThem(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	obj := putWithFacts(t, st, storePrivate, ossWorkPath, "clip-bytes", testFactsMeta)
	if fake.obj("priv/" + ossWorkPath).meta[ossFactsHeader] == "" {
		t.Fatal("probe 过的落桶必须把事实写进对象元数据")
	}
	h, reason := trustedHead(t, st, storePrivate, ossWorkPath)
	if h.Facts == nil || reason != "" || h.Facts.Sha256 != obj.Sha256 || h.Size != int64(len("clip-bytes")) || h.ContentType != "video/mp4" {
		t.Fatalf("head = %+v facts=%+v", h, h.Facts)
	}
	want := testFactsMeta
	want.Size = h.Size
	if h.Facts.Meta != want {
		t.Fatalf("事实元数据 = %+v, want %+v", h.Facts.Meta, want)
	}
	if h.Version != normalizeETag(etagOf([]byte("clip-bytes"))) {
		t.Fatalf("version = %q", h.Version)
	}
}

// 不带事实的写入（索引、字体、直传）不写元数据头；字节数与登记不符的事实当作没有。
func TestOSSFactsAbsentOrStale(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	if _, err := st.Put(context.Background(), storePrivate, ossWorkPath, writeFixture(t, "plain"), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if len(fake.obj("priv/"+ossWorkPath).meta) != 0 {
		t.Fatal("没登记事实的 Put 不应带元数据头")
	}
	// 登记的是另一个文件：不带
	other := writeFixture(t, "other")
	ctx := withSealedFacts(context.Background(), other, strings.Repeat("a", 64), 5,
		sealFacts(testFactsID, storedFacts{V: 1, Sha256: strings.Repeat("a", 64), Size: 5, Meta: testFactsMeta}))
	if _, err := st.Put(ctx, storePrivate, ossWorkPath, writeFixture(t, "plain2"), "video/mp4"); err != nil {
		t.Fatal(err)
	}
	if len(fake.obj("priv/"+ossWorkPath).meta) != 0 {
		t.Fatal("事实只对登记的那个文件生效")
	}
	setMeta := func(v string) {
		fake.mu.Lock()
		fake.objs["priv/"+ossWorkPath].meta = map[string]string{ossFactsHeader: v}
		fake.mu.Unlock()
	}
	good := storedFacts{V: 1, Sha256: strings.Repeat("b", 64), Size: int64(len("plain2")), Meta: MediaMeta{Mime: "video/mp4"}}
	setMeta(sealFacts(testFactsID, good))
	if h, reason := trustedHead(t, st, storePrivate, ossWorkPath); h.Facts == nil || reason != "" {
		t.Fatalf("签名、字节数、内容类型都对的事实应当可用: %s", reason)
	}
	// 元数据里的字节数与对象不符（被覆盖过）：丢弃
	stale := good
	stale.Size = 999
	setMeta(sealFacts(testFactsID, stale))
	if h, reason := trustedHead(t, st, storePrivate, ossWorkPath); h.Facts != nil || reason != "source_facts_stale" {
		t.Fatalf("字节数不符的事实必须丢弃: %+v %s", h.Facts, reason)
	}
	// 内容类型对不上（别的写入方改过）：丢弃
	otherMime := good
	otherMime.Meta.Mime = "image/png"
	setMeta(sealFacts(testFactsID, otherMime))
	if _, reason := trustedHead(t, st, storePrivate, ossWorkPath); reason != "source_facts_stale" {
		t.Fatalf("内容类型不符的事实必须丢弃: %s", reason)
	}
	if openFacts(testFactsID, "!!") != nil || openFacts(testFactsID, sealFacts(testFactsID, storedFacts{V: 2, Sha256: strings.Repeat("c", 64)})) != nil ||
		openFacts(testFactsID, sealFacts(testFactsID, storedFacts{V: 1, Sha256: strings.Repeat("C", 64)})) != nil {
		t.Fatal("损坏、未知版本或 sha256 非法的事实必须丢弃")
	}
}

// 单次 CopyObject：私有桶 → 素材库桶，字节、内容类型、元数据原样；用目标桶密钥签名；不下载不上传。
func TestOSSServerCopySingle(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip-bytes", testFactsMeta)
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "" {
		t.Fatalf("同地域（内网/公网 endpoint）应可服务端复制，原因 %s", r)
	}
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	putsBefore := fake.puts
	obj, mode, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h)
	if err != nil || mode != "single" {
		t.Fatalf("ServerCopy = %v mode=%s", err, mode)
	}
	dst := fake.obj("asset/" + ossAssetPath2)
	if dst == nil || string(dst.data) != "clip-bytes" || dst.ctype != "video/mp4" || dst.meta[ossFactsHeader] != h.UserMeta[ossFactsHeader] {
		t.Fatalf("目标对象 = %+v", dst)
	}
	if obj.Sha256 != h.Facts.Sha256 || obj.Size != h.Size || obj.Mime != "video/mp4" || obj.Version == "" || obj.Store != storeAsset {
		t.Fatalf("obj = %+v", obj)
	}
	if fake.gets != 0 || fake.puts != putsBefore || fake.copies != 1 {
		t.Fatalf("服务端复制不应读写字节: gets=%d puts=%d copies=%d", fake.gets, fake.puts-putsBefore, fake.copies)
	}
}

// 超过阈值走分片复制：按区间 UploadPartCopy、完成后字节一致，内容类型与元数据显式带上。
func TestOSSServerCopyMultipart(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	st.copier.threshold, st.copier.partSize = 8, 4
	content := "0123456789abcdefghij-xyz" // 24 字节 → 6 片
	putWithFacts(t, st, storePrivate, ossWorkPath, content, testFactsMeta)
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	obj, mode, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h)
	if err != nil || mode != "multipart" {
		t.Fatalf("ServerCopy = %v mode=%s", err, mode)
	}
	dst := fake.obj("asset/" + ossAssetPath2)
	if dst == nil || string(dst.data) != content || dst.ctype != "video/mp4" || dst.meta[ossFactsHeader] == "" {
		t.Fatalf("分片复制结果 = %+v", dst)
	}
	if len(fake.partReqs) != 6 || obj.Version != "MULTI-6" || fake.copies != 0 {
		t.Fatalf("parts=%v version=%s copies=%d", fake.partReqs, obj.Version, fake.copies)
	}
	ranges := "," + strings.Join(fake.partReqs, ",") + ","
	if !strings.Contains(ranges, ",bytes=0-3,") || !strings.Contains(ranges, ",bytes=20-23,") {
		t.Fatalf("区间 = %v", fake.partReqs)
	}
	// 复制出来的对象再 HEAD，事实仍然有效（复制的复制也能走服务端）
	h2, reason := trustedHead(t, st, storeAsset, ossAssetPath2)
	if h2.Facts == nil || h2.Facts.Sha256 != h.Facts.Sha256 {
		t.Fatalf("目标对象事实 = %+v %s", h2.Facts, reason)
	}
}

// 分片中途失败：放弃分片上传，不留半截对象；错误是暂时故障（不是「做不了」，不回退）。
func TestOSSServerCopyMultipartAbortsOnPartFailure(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	st.copier.threshold, st.copier.partSize = 8, 4
	putWithFacts(t, st, storePrivate, ossWorkPath, "0123456789abcdef", testFactsMeta)
	fake.failPart = 2
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	_, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h)
	var refused *serverCopyRefused
	if err == nil || errors.As(err, &refused) {
		t.Fatalf("err = %v", err)
	}
	if fake.aborted != 1 || len(fake.uploads) != 0 || fake.obj("asset/"+ossAssetPath2) != nil {
		t.Fatalf("aborted=%d uploads=%d", fake.aborted, len(fake.uploads))
	}
}

// 目标密钥读不了源桶（403）：返回「做不了」，之后一段时间内这对桶不再尝试。
func TestOSSServerCopyDeniedIsRefusedAndRemembered(t *testing.T) {
	st, fake := newCopyTestOSS(t, func(c *OSSConfig) { c.AssetLibrary.AccessKey, c.AssetLibrary.SecretKey = "AK2", "SK2" })
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip", testFactsMeta)
	fake.denyCopy = true
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	_, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h)
	var refused *serverCopyRefused
	if !errors.As(err, &refused) || refused.reason != "permission" {
		t.Fatalf("err = %v", err)
	}
	if len(fake.copyAKs) != 1 || fake.copyAKs[0] != "AK2" {
		t.Fatalf("复制请求必须用目标桶的密钥签名: %v", fake.copyAKs)
	}
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "denied_recently" {
		t.Fatalf("被拒后应暂停尝试，原因 = %q", r)
	}
	if r := st.ServerCopyEligible(storePrivate, storeThirdParty); r != "" {
		t.Fatalf("别的桶对不受影响，原因 = %q", r)
	}
	st.now = func() time.Time { return time.Now().Add(copyDeniedTTL + time.Minute) }
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "" {
		t.Fatalf("暂停期过后应重新尝试，原因 = %q", r)
	}
}

// 关闭开关、跨地域：不做服务端复制。
func TestOSSServerCopyEligibility(t *testing.T) {
	st, _ := newCopyTestOSS(t, func(c *OSSConfig) { c.DisableServerSideCopy = true })
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "disabled" {
		t.Fatalf("关闭时原因 = %q", r)
	}
	st2, _ := newCopyTestOSS(t, func(c *OSSConfig) { c.AssetLibrary.Endpoint = "https://oss-cn-beijing.aliyuncs.com" })
	if r := st2.ServerCopyEligible(storePrivate, storeAsset); r != "cross_region" {
		t.Fatalf("跨地域原因 = %q", r)
	}
	if r := st2.ServerCopyEligible(storePrivate, storeThirdParty); r != "" {
		t.Fatalf("同地域原因 = %q", r)
	}
}

// 源在 HEAD 之后被删：CopyObject 404 NoSuchKey → errObjectNotFound（不是回退）。
func TestOSSServerCopySourceVanished(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip", testFactsMeta)
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	fake.mu.Lock()
	delete(fake.objs, "priv/"+ossWorkPath)
	fake.mu.Unlock()
	if _, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// 伪造的事实（没有签名、别的密钥签的）不当真：回退原路径，不按伪造值给结果。
func TestOSSFactsForgedAreUntrusted(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip-bytes", testFactsMeta)
	f := storedFacts{V: 1, Sha256: strings.Repeat("e", 64), Size: int64(len("clip-bytes")), Meta: testFactsMeta}
	f.Meta.Size = f.Size
	raw, _ := json.Marshal(f)
	unsigned := base64.RawURLEncoding.EncodeToString(raw)
	otherID, err := newIdentity("tst001", strings.Repeat("x", 40))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"无签名":   unsigned,
		"空签名":   unsigned + ".",
		"别的密钥":  sealFacts(otherID, f),
		"签名被截断": sealFacts(testFactsID, f)[:len(sealFacts(testFactsID, f))-2],
		"payload 被改": func() string {
			_, sig, _ := strings.Cut(sealFacts(testFactsID, f), ".")
			g := f
			g.Sha256 = strings.Repeat("d", 64)
			raw2, _ := json.Marshal(g)
			return base64.RawURLEncoding.EncodeToString(raw2) + "." + sig
		}(),
	} {
		fake.mu.Lock()
		fake.objs["priv/"+ossWorkPath].meta = map[string]string{ossFactsHeader: v}
		fake.mu.Unlock()
		if h, reason := trustedHead(t, st, storePrivate, ossWorkPath); h.Facts != nil || reason != "source_facts_untrusted" {
			t.Fatalf("%s: 伪造的事实必须丢弃，reason=%s", name, reason)
		}
	}
	// 没有验过的事实，存储层也拒绝复制
	h, _ := st.ObjectFacts(context.Background(), storePrivate, ossWorkPath)
	if _, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h); err == nil {
		t.Fatal("没有验过的事实不应复制")
	}
}

// 低频 / 归档存储的源不走服务端复制（复制出的存储类型可能与原路径不同）；标准型照常。
func TestOSSFactsNonStandardStorageClass(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip-bytes", testFactsMeta)
	for cls, want := range map[string]string{"IA": "source_storage_class", "Archive": "source_storage_class", "Standard": ""} {
		fake.mu.Lock()
		fake.objs["priv/"+ossWorkPath].sclass = cls
		fake.mu.Unlock()
		if _, reason := trustedHead(t, st, storePrivate, ossWorkPath); reason != want {
			t.Fatalf("%s: reason = %q, want %q", cls, reason, want)
		}
	}
}

// 复制带 if-match：HEAD 之后源被改写（412）→ 回退但不记这对桶；409 同样只回退这一次。
func TestOSSServerCopySourceChangedAfterHead(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	putWithFacts(t, st, storePrivate, ossWorkPath, "clip-bytes", testFactsMeta)
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	fake.mu.Lock()
	fake.objs["priv/"+ossWorkPath].data = []byte("CLIP-BYTES")
	fake.mu.Unlock()
	_, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h)
	var refused *serverCopyRefused
	if !errors.As(err, &refused) || refused.reason != "source_changed" || refused.remember {
		t.Fatalf("err = %v", err)
	}
	if len(fake.ifMatches) != 1 || fake.ifMatches[0] != h.ETag || fake.obj("asset/"+ossAssetPath2) != nil {
		t.Fatalf("if-match = %v", fake.ifMatches)
	}
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "" {
		t.Fatalf("412 不应暂停这对桶: %q", r)
	}
	fake.copyFail = http.StatusConflict
	h2, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	h2.Facts = h.Facts
	_, _, err = st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h2)
	if !errors.As(err, &refused) || refused.remember {
		t.Fatalf("409 err = %v", err)
	}
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "" {
		t.Fatalf("409 不应暂停这对桶: %q", r)
	}
	// 501 / 405 / 400：存储不支持 → 回退并暂停
	for _, code := range []int{http.StatusNotImplemented, http.StatusMethodNotAllowed, http.StatusBadRequest} {
		st.copier.denied = map[string]time.Time{}
		fake.copyFail = code
		_, _, err = st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h2)
		if !errors.As(err, &refused) || !refused.remember || st.ServerCopyEligible(storePrivate, storeAsset) != "denied_recently" {
			t.Fatalf("%d err = %v", code, err)
		}
	}
	// 5xx：暂时故障，不回退不暂停
	st.copier.denied = map[string]time.Time{}
	fake.copyFail = http.StatusServiceUnavailable
	_, _, err = st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h2)
	if err == nil || errors.As(err, &refused) || st.ServerCopyEligible(storePrivate, storeAsset) != "" {
		t.Fatalf("503 err = %v", err)
	}
}

// 分片复制每一片都带 if-match；请求被取消时放弃分片上传。
func TestOSSServerCopyMultipartIfMatchAndCancel(t *testing.T) {
	st, fake := newCopyTestOSS(t, nil)
	st.copier.threshold, st.copier.partSize = 8, 4
	putWithFacts(t, st, storePrivate, ossWorkPath, "0123456789abcdef", testFactsMeta)
	h, _ := trustedHead(t, st, storePrivate, ossWorkPath)
	if _, _, err := st.ServerCopy(context.Background(), storePrivate, ossWorkPath, storeAsset, ossAssetPath2, h); err != nil {
		t.Fatal(err)
	}
	if len(fake.ifMatches) != 4 {
		t.Fatalf("每片都应带 if-match: %v", fake.ifMatches)
	}
	for _, im := range fake.ifMatches {
		if im != h.ETag {
			t.Fatalf("if-match = %q", im)
		}
	}
	// 初始化成功后、第一片复制时请求被取消：放弃分片上传（Abort 用独立上下文，仍能发出）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := st.hc.Transport
	st.hc = &http.Client{Transport: cancelOnPart{base: base, cancel: cancel}}
	_, _, err := st.ServerCopy(ctx, storePrivate, ossWorkPath, storeAsset, "v1/asset/2026/11/cancelledcancelledcancelxx", h)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if fake.aborted != 1 || len(fake.uploads) != 0 {
		t.Fatalf("取消后必须放弃分片上传: aborted=%d uploads=%d", fake.aborted, len(fake.uploads))
	}
}

// cancelOnPart 遇到 UploadPartCopy 时取消请求上下文并返回取消错误，其余请求照常转发。
type cancelOnPart struct {
	base   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelOnPart) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Query().Has("partNumber") {
		c.cancel()
		return nil, context.Canceled
	}
	return c.base.RoundTrip(r)
}

// 地域判断：内网 / 公网 / VPC 同地域；协议头、端口、尾斜杠不影响；加速域名与认不出的 aliyuncs 域名判断不了。
func TestRegionKey(t *testing.T) {
	same := [][2]string{
		{"https://oss-cn-shanghai.aliyuncs.com", "oss-cn-shanghai-internal.aliyuncs.com"},
		{"http://OSS-CN-SHANGHAI.aliyuncs.com:443/", "vpc100-oss-cn-shanghai.aliyuncs.com"},
		{"https://oss-cn-hangzhou-finance-1-internal.aliyuncs.com", "oss-cn-hangzhou-finance-1.aliyuncs.com"},
		{"https://minio.example.com:9000", "minio.example.com"},
	}
	for _, p := range same {
		if a, b := regionKey(p[0]), regionKey(p[1]); a == "" || a != b {
			t.Fatalf("%s vs %s: %q %q", p[0], p[1], a, b)
		}
	}
	diff := [][2]string{
		{"https://oss-cn-shanghai.aliyuncs.com", "https://oss-cn-beijing.aliyuncs.com"},
		{"https://oss-cn-hangzhou.aliyuncs.com", "https://oss-cn-hangzhou-finance-1.aliyuncs.com"},
		{"https://minio-a.example.com", "https://minio-b.example.com"},
	}
	for _, p := range diff {
		if regionKey(p[0]) == regionKey(p[1]) {
			t.Fatalf("%s vs %s 应不同", p[0], p[1])
		}
	}
	for _, e := range []string{"https://oss-accelerate.aliyuncs.com", "oss-accelerate-overseas.aliyuncs.com", "https://cn-shanghai.oss.aliyuncs.com", ""} {
		if k := regionKey(e); k != "" {
			t.Fatalf("%s 应判断不了地域，得到 %q", e, k)
		}
	}
	st, _ := newCopyTestOSS(t, func(c *OSSConfig) {
		c.Private.Endpoint, c.AssetLibrary.Endpoint = "https://oss-accelerate.aliyuncs.com", "https://oss-accelerate.aliyuncs.com"
	})
	if r := st.ServerCopyEligible(storePrivate, storeAsset); r != "region_unknown" {
		t.Fatalf("加速域名原因 = %q", r)
	}
}

// 网络错误不把地址（桶域名 + 对象键）带进错误与日志。
func TestOSSSendErrorRedactsURL(t *testing.T) {
	st, _ := newCopyTestOSS(t, nil)
	st.hc = &http.Client{Transport: failTransport{}}
	_, err := st.ObjectFacts(context.Background(), storePrivate, ossWorkPath)
	if err == nil || strings.Contains(err.Error(), ossWorkPath) || strings.Contains(err.Error(), "aliyuncs.com") {
		t.Fatalf("err = %v", err)
	}
	ue := &url.Error{Op: "Put", URL: "https://priv.oss-cn-shanghai.aliyuncs.com/" + ossWorkPath, Err: errors.New("boom")}
	if got := redactErr(fmt.Errorf("wrap: %w", ue)); strings.Contains(got, ossWorkPath) || !strings.Contains(got, "boom") {
		t.Fatalf("redact = %q", got)
	}
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

// 部署 yaml 不写 DisableServerSideCopy 时默认开启；写 true 关闭。
func TestDisableServerSideCopyDefault(t *testing.T) {
	var c struct {
		OSS OSSConfig `json:",optional"`
	}
	if err := conf.LoadFromYamlBytes([]byte("OSS:\n  Enabled: true\n  PresignMinutes: 30\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.OSS.DisableServerSideCopy {
		t.Fatal("不写该键时应默认开启服务端复制")
	}
	if err := conf.LoadFromYamlBytes([]byte("OSS:\n  DisableServerSideCopy: true\n"), &c); err != nil || !c.OSS.DisableServerSideCopy {
		t.Fatalf("写 true 时应关闭: %v", err)
	}
}
