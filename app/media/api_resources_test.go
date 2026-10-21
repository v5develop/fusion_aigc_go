package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeResourceStore 假的存储：对象集合在内存里，签名地址是固定格式，公有地址只对非私有存储给出。
type fakeResourceStore struct {
	objects map[string]int64 // store|path -> size
	pending map[string]time.Time
}

func (f *fakeResourceStore) CreatePending(_ context.Context, store, path string, at time.Time) (time.Time, bool, error) {
	if f.pending == nil {
		f.pending = map[string]time.Time{}
	}
	if old, ok := f.pending[store+"|"+path]; ok {
		return old, false, nil
	}
	f.pending[store+"|"+path] = at
	return at, true, nil
}
func (f *fakeResourceStore) PendingDeadline(_ context.Context, store, path string) (time.Time, bool, error) {
	at, ok := f.pending[store+"|"+path]
	return at, ok, nil
}
func (f *fakeResourceStore) ClearPending(_ context.Context, store, path string) error {
	delete(f.pending, store+"|"+path)
	return nil
}
func (f *fakeResourceStore) ListPending(context.Context, string, string, int) ([]PendingMark, string, error) {
	return nil, "", nil
}

func (f *fakeResourceStore) Put(context.Context, string, string, string, string) (StoredObject, error) {
	return StoredObject{}, nil
}
func (f *fakeResourceStore) Fetch(context.Context, string, string, string) (string, error) {
	return "", errObjectNotFound
}
func (f *fakeResourceStore) URL(context.Context, StoredObject) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}
func (f *fakeResourceStore) Delete(context.Context, string, string) (bool, error) { return false, nil }
func (f *fakeResourceStore) SignedPutURL(context.Context, string, string, time.Duration) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}
func (f *fakeResourceStore) Head(_ context.Context, store, path string) (int64, error) {
	if n, ok := f.objects[store+"|"+path]; ok {
		return n, nil
	}
	return 0, errObjectNotFound
}
func (f *fakeResourceStore) SignedURL(_ context.Context, store, path string, ttl time.Duration) (StoredURL, error) {
	return StoredURL{Kind: "signed", Value: "https://signed.example/" + store + "/" + path + "?ttl=" + ttl.String(),
		ExpiresAt: "2030-01-01T00:00:00Z"}, nil
}
func (f *fakeResourceStore) PublicURL(_ context.Context, store, path string) (StoredURL, error) {
	if store == storePrivate {
		return StoredURL{Kind: "none"}, nil
	}
	return StoredURL{Kind: "public", Value: "https://cdn.example/" + path}, nil
}

// newResourceFixture 启动一个服务，并把对象存储换成假存储；objects 里预置的对象会出现在 Head 里。
func newResourceFixture(t *testing.T, s *Server) (*httptest.Server, *fakeResourceStore) {
	t.Helper()
	fake := &fakeResourceStore{objects: map[string]int64{}}
	s.objects = fake
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return ts, fake
}

func newResourceServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(Config{Token: testToken, InstanceID: "tst001", IdentitySecret: testSecret,
		ObjectDir: t.TempDir(), MaxConcurrent: 2, MaxInputBytes: 50 << 20, OpTimeoutSeconds: 60,
		Idempotency: IdempotencyConfig{Store: IdemStoreMemory}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func postResourceURL(t *testing.T, base string, body map[string]any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/mediasvc/resources/urls", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	return send(t, req, testToken)
}

// 签名地址：返回对象键（由 rid 派生），地址里不出现 rid 或实例 ID。
func TestResourceURLSignedHidesIdentity(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "deliver", "job-1")
	key := s.id.ObjectKey(rid)
	fake.objects[storePrivate+"|"+storePath(classDeliver, key)] = 1234

	resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "deliver", "store": "private", "mode": "signed", "ttl_seconds": 600})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out resourceURLResp
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Size != 1234 || out.Mode != "signed" {
		t.Fatalf("resp = %+v", out)
	}
	if strings.Contains(out.URL, rid) || strings.Contains(out.URL, "tst001") {
		t.Fatalf("地址泄露了身份: %s", out.URL)
	}
	if !strings.Contains(out.URL, "ttl=10m0s") {
		t.Fatalf("有效期没有传给签名: %s", out.URL)
	}
}

// 公有地址：只对非私有存储可用；交付到私有桶的资源要求公有地址时返回 409，不自动复制。
func TestResourceURLPublicNeedsPublicLocation(t *testing.T) {
	s := newResourceServer(t)
	ts, fake := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "deliver", "job-2")
	key := s.id.ObjectKey(rid)
	fake.objects[storePrivate+"|"+storePath(classDeliver, key)] = 10

	resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "deliver", "store": "private", "mode": "public"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("私有资源要公有地址 status = %d, want 409", resp.StatusCode)
	}
	fake.objects[storeThirdParty+"|"+storePath(classDeliver, key)] = 10
	resp = postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "deliver", "store": "third_party", "mode": "public"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("公有交付桶的资源 status = %d", resp.StatusCode)
	}
}

// 对象不存在：404。
func TestResourceURLMissingObjectIs404(t *testing.T) {
	s := newResourceServer(t)
	ts, _ := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "work", "job-3")
	resp := postResourceURL(t, ts.URL, map[string]any{"rid": rid, "class": "work", "store": "private", "mode": "signed"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// 参数校验：rid 格式、类别与存储配对、模式、有效期上限。
func TestResourceURLValidation(t *testing.T) {
	s := newResourceServer(t)
	ts, _ := newResourceFixture(t, s)
	rid, _ := s.id.RID("", "work", "job-4")
	cases := []map[string]any{
		{"rid": "not-a-rid", "class": "work", "store": "private", "mode": "signed"},
		{"rid": rid, "class": "video", "store": "private", "mode": "signed"},
		{"rid": rid, "class": "user", "store": "third_party", "mode": "signed"},
		{"rid": rid, "class": "work", "store": "private", "mode": "bogus"},
		{"rid": rid, "class": "work", "store": "private", "mode": "signed", "ttl_seconds": 86401},
	}
	for i, c := range cases {
		if got := postResourceURL(t, ts.URL, c).StatusCode; got != http.StatusBadRequest {
			t.Fatalf("case %d status = %d, want 400", i, got)
		}
	}
}

// 没有鉴权：401。
func TestResourceURLRequiresToken(t *testing.T) {
	s := newResourceServer(t)
	ts, _ := newResourceFixture(t, s)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/mediasvc/resources/urls", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp := send(t, req, "wrong")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
}

// 启动时校验身份配置：缺失则拒绝启动。
func TestNewServerRejectsMissingIdentity(t *testing.T) {
	if _, err := NewServer(Config{Token: testToken, ObjectDir: filepath.Join(t.TempDir(), "o"), InstanceID: "bad", IdentitySecret: testSecret}); err == nil {
		t.Fatal("非法实例 ID 必须拒绝启动")
	}
	if _, err := NewServer(Config{Token: testToken, ObjectDir: t.TempDir(), InstanceID: "tst001"}); err == nil {
		t.Fatal("缺少身份密钥必须拒绝启动")
	}
	_ = os.Getenv
}
