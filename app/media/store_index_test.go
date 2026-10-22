package media

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testIndexKeyA = "aaaaaaaaaaaaaaaaaaaaaaaaaa" // 26 位合法对象键
	testIndexKeyB = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// newOSSWithHandler 用自定义 handler 构造 OSSStore（私有桶 priv），所有请求转到 handler，保留桶名 Host。
func newOSSWithHandler(t *testing.T, h http.Handler) *OSSStore {
	t.Helper()
	srv := httptest.NewServer(h)
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
	return st
}

func TestOSSListIndexesParsesEntriesAndSkipsNonJSON(t *testing.T) {
	var gotQuery url.Values
	st := newOSSWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Host, "priv.") {
			t.Errorf("list must target private bucket, host=%s", r.Host)
		}
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>
<IsTruncated>false</IsTruncated>
<Contents><Key>idx/%s.json</Key><LastModified>2026-10-21T10:00:00.000Z</LastModified></Contents>
<Contents><Key>idx/notes.txt</Key><LastModified>2026-10-21T11:00:00.000Z</LastModified></Contents>
<Contents><Key>idx/%s.json</Key><LastModified>2026-10-22T08:30:15.123Z</LastModified></Contents>
</ListBucketResult>`, testIndexKeyA, testIndexKeyB)
	}))

	entries, next, err := st.ListIndexes(context.Background(), "", 100)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if next != "" {
		t.Fatalf("next = %q, want empty for a complete page", next)
	}
	if got := gotQuery.Get("prefix"); got != "idx/" {
		t.Fatalf("prefix = %q, want idx/", got)
	}
	if got := gotQuery.Get("max-keys"); got != "100" {
		t.Fatalf("max-keys = %q, want 100", got)
	}
	if _, ok := gotQuery["marker"]; ok {
		t.Fatalf("first page must not send marker, query=%v", gotQuery)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (notes.txt skipped): %+v", len(entries), entries)
	}
	if entries[0].ObjectKey != testIndexKeyA {
		t.Fatalf("entries[0].ObjectKey = %q", entries[0].ObjectKey)
	}
	want0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	if !entries[0].Modified.Equal(want0) {
		t.Fatalf("entries[0].Modified = %v, want %v", entries[0].Modified, want0)
	}
	if entries[1].ObjectKey != testIndexKeyB {
		t.Fatalf("entries[1].ObjectKey = %q", entries[1].ObjectKey)
	}
	want1 := time.Date(2026, 10, 22, 8, 30, 15, 123_000_000, time.UTC)
	if !entries[1].Modified.Equal(want1) {
		t.Fatalf("entries[1].Modified = %v, want %v", entries[1].Modified, want1)
	}
}

func TestOSSListIndexesTruncatedPageReturnsLastKeyAsNext(t *testing.T) {
	var gotMarker string
	st := newOSSWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMarker = r.URL.Query().Get("marker")
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>
<IsTruncated>true</IsTruncated>
<Contents><Key>idx/%s.json</Key><LastModified>2026-10-21T10:00:00.000Z</LastModified></Contents>
<Contents><Key>idx/%s.json</Key><LastModified>2026-10-21T10:05:00.000Z</LastModified></Contents>
</ListBucketResult>`, testIndexKeyA, testIndexKeyB)
	}))

	entries, next, err := st.ListIndexes(context.Background(), "idx/"+testIndexKeyA+".json", 2)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if gotMarker != "idx/"+testIndexKeyA+".json" {
		t.Fatalf("marker not forwarded, got %q", gotMarker)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if next != "idx/"+testIndexKeyB+".json" {
		t.Fatalf("next = %q, want last listed key", next)
	}
}

func TestOSSListIndexesErrorStatusFails(t *testing.T) {
	st := newOSSWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	if _, _, err := st.ListIndexes(context.Background(), "", 10); err == nil {
		t.Fatal("expected error on 403 listing")
	}
}

func TestOSSDeleteIndexTargetsPrivateIndexKey(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 no content", http.StatusNoContent, false},
		{"200 ok", http.StatusOK, false},
		{"404 already gone", http.StatusNotFound, false},
		{"500 server error", http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var method, host, path, auth string
			st := newOSSWithHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, host, path = r.Method, r.Host, r.URL.Path
				auth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))

			err := st.DeleteIndex(context.Background(), testIndexKeyA)
			if (err != nil) != tc.wantErr {
				t.Fatalf("DeleteIndex err = %v, wantErr %v", err, tc.wantErr)
			}
			if method != http.MethodDelete {
				t.Fatalf("method = %s, want DELETE", method)
			}
			if !strings.HasPrefix(host, "priv.") {
				t.Fatalf("host = %s, want private bucket", host)
			}
			if want := "/idx/" + testIndexKeyA + ".json"; path != want {
				t.Fatalf("path = %s, want %s", path, want)
			}
			if !strings.HasPrefix(auth, "OSS AK:") {
				t.Fatalf("DELETE must be signed, Authorization=%q", auth)
			}
		})
	}
}

func TestLocalStoreListIndexesReturnsModifiedTimes(t *testing.T) {
	dir := t.TempDir()
	st, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	idxDir := filepath.Join(dir, storePrivate, "idx")
	if err := os.MkdirAll(idxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]time.Time{
		testIndexKeyA + ".json": time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		testIndexKeyB + ".json": time.Date(2026, 10, 22, 9, 15, 30, 0, time.UTC),
	}
	for name, mt := range files {
		p := filepath.Join(idxDir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// 非 .json 文件必须被忽略
	if err := os.WriteFile(filepath.Join(idxDir, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, next, err := st.ListIndexes(context.Background(), "", 1)
	if err != nil {
		t.Fatalf("ListIndexes: %v", err)
	}
	if next != "" {
		t.Fatalf("local store is not paged, next = %q", next)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	got := map[string]time.Time{}
	for _, e := range entries {
		got[e.ObjectKey] = e.Modified
	}
	for name, mt := range files {
		key := strings.TrimSuffix(name, ".json")
		m, ok := got[key]
		if !ok {
			t.Fatalf("missing entry for %s", key)
		}
		if !m.Equal(mt) {
			t.Fatalf("Modified[%s] = %v, want %v", key, m, mt)
		}
	}
}

func TestLocalStoreListIndexesMissingDirIsEmpty(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, next, err := st.ListIndexes(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListIndexes on missing dir: %v", err)
	}
	if len(entries) != 0 || next != "" {
		t.Fatalf("want empty result, got %d entries next=%q", len(entries), next)
	}
}

func TestLocalStoreDeleteIndex(t *testing.T) {
	dir := t.TempDir()
	st, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	idxDir := filepath.Join(dir, storePrivate, "idx")
	if err := os.MkdirAll(idxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(idxDir, testIndexKeyA+".json")
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteIndex(context.Background(), testIndexKeyA); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("index file still present after DeleteIndex: %v", err)
	}
	// 文件已不存在时视为成功
	if err := st.DeleteIndex(context.Background(), testIndexKeyA); err != nil {
		t.Fatalf("DeleteIndex on missing file: %v", err)
	}
	// 非法对象键被拒绝
	if err := st.DeleteIndex(context.Background(), "../escape"); err == nil {
		t.Fatal("DeleteIndex must reject invalid object key")
	}
}

func TestObjectKeyOfIndexName(t *testing.T) {
	cases := []struct {
		name   string
		wantK  string
		wantOK bool
	}{
		{testIndexKeyA + ".json", testIndexKeyA, true},
		{"x.txt", "", false},
		{"0.json", "", false},
		{testIndexKeyA[:25] + ".json", "", false},
		{testIndexKeyA + "a.json", "", false},
	}
	for _, tc := range cases {
		key, ok := objectKeyOfIndexName(tc.name)
		if ok != tc.wantOK {
			t.Errorf("objectKeyOfIndexName(%q) ok = %v, want %v", tc.name, ok, tc.wantOK)
		}
		// 合法名才要求键值正确；非法名调用方只看 ok。
		if tc.wantOK && key != tc.wantK {
			t.Errorf("objectKeyOfIndexName(%q) key = %q, want %q", tc.name, key, tc.wantK)
		}
	}
}
