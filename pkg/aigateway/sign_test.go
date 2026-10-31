package aigateway

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// contract/hmac.md 的测试向量。
func TestSignReferenceVector(t *testing.T) {
	const (
		wantCanonical = "v1\nfusion-java\ncalls\nPOST\n/v1/calls\nretry_failed=true&wait=25\n1790000000\nnonce-0123456789abcdef\n" +
			"6d62550888a7c7d42cb854d4321536b176ffe666fded010fdf87655718f61fbb"
		wantSig = "c85d585c8ce54c561674ca404ba99cff9570cfd7b32e50191edba2a0a6991d89"
	)
	body := []byte(`{"operation":"image.generate"}`)

	if got := CanonicalQuery("wait=25&retry_failed=true"); got != "retry_failed=true&wait=25" {
		t.Fatalf("canonical query = %q", got)
	}
	canonical := CanonicalString("fusion-java", "calls", "post", "/v1/calls", CanonicalQuery("wait=25&retry_failed=true"),
		1790000000, "nonce-0123456789abcdef", body)
	if canonical != wantCanonical {
		t.Fatalf("canonical string mismatch:\n got %q\nwant %q", canonical, wantCanonical)
	}
	if got := Signature("test-secret-please-rotate", canonical); got != wantSig {
		t.Fatalf("signature = %s, want %s", got, wantSig)
	}

	// 走 Sign 全流程（含路径规范化）
	req, _ := http.NewRequest(http.MethodPost, "http://ai-gateway:8080//v1/calls/?wait=25&retry_failed=true", nil)
	s := Signer{CallerID: "fusion-java", KeyID: "fj-2026-09", Secret: "test-secret-please-rotate"}
	if err := s.Sign(req, PurposeCalls, body, time.Unix(1790000000, 0), "nonce-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		HeaderVersion:   "v1",
		HeaderCaller:    "fusion-java",
		HeaderKeyID:     "fj-2026-09",
		HeaderPurpose:   "calls",
		HeaderTimestamp: "1790000000",
		HeaderNonce:     "nonce-0123456789abcdef",
		HeaderSignature: wantSig,
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestSignEmptyQueryAndBody(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://gw/v1/catalog/model-groups", nil)
	s := Signer{CallerID: "fusion-go", KeyID: "fusion-go-v1", Secret: "k"}
	if err := s.Sign(req, PurposeCatalog, nil, time.Unix(1790000000, 0), "nonce-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	canonical := "v1\nfusion-go\ncatalog\nGET\n/v1/catalog/model-groups\n\n1790000000\nnonce-0123456789abcdef\n" + emptyBodySHA256
	if got := req.Header.Get(HeaderSignature); got != Signature("k", canonical) {
		t.Fatalf("signature mismatch for empty query/body")
	}
	if BodySHA256(nil) != BodySHA256([]byte{}) || BodySHA256(nil) != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("empty body sha256")
	}
}

func TestCanonicalQueryEncoding(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", ""},
		{"&&", ""},
		{"q=a b", "q=a%20b"},
		{"q=a+b", "q=a%20b"},   // + 按空格解码（与 parse_qsl 一致）
		{"q=a%20b", "q=a%20b"}, // 已编码的保持一致
		{"q=a%2Bb", "q=a%2Bb"}, // 字面 +
		{"name=%E4%B8%AD%E6%96%87", "name=%E4%B8%AD%E6%96%87"},
		{"name=中文", "name=%E4%B8%AD%E6%96%87"},
		{"k=~-._*", "k=~-._%2A"},
		{"b=2&a=2&a=1&c", "a=1&a=2&b=2&c="},
		{"key=x%2fy", "key=x%2Fy"}, // 十六进制统一大写
		{"id=job-1:render/0#x", "id=job-1%3Arender%2F0%23x"},
	}
	for _, c := range cases {
		if got := CanonicalQuery(c.raw); got != c.want {
			t.Errorf("CanonicalQuery(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	// 解码后的参数与原始串结果一致
	if got := CanonicalValues(map[string][]string{"name": {"中文 x"}, "a": {"1"}}); got != "a=1&name=%E4%B8%AD%E6%96%87%20x" {
		t.Errorf("CanonicalValues = %q", got)
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/":                        "/",
		"//":                       "/",
		"/v1//calls/":              "/v1/calls",
		"/v1/calls:batchGet":       "/v1/calls:batchGet",
		"/v1/calls/call_x/cancel/": "/v1/calls/call_x/cancel",
	}
	for in, want := range cases {
		got, err := NormalizePath(in)
		if err != nil || got != want {
			t.Errorf("NormalizePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "v1/calls", "/v1?x", "/v1#f"} {
		if _, err := NormalizePath(bad); err == nil {
			t.Errorf("NormalizePath(%q) want error", bad)
		}
	}
}

func TestSignRejectsBadInput(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://gw/v1/calls", nil)
	now := time.Unix(1790000000, 0)
	if err := (Signer{CallerID: "fusion-go", KeyID: "k1"}).Sign(req, PurposeCalls, nil, now, "nonce-0123456789abcdef"); err == nil {
		t.Error("empty secret should fail")
	}
	if err := (Signer{CallerID: "fusion-go", KeyID: "k1", Secret: "s"}).Sign(req, PurposeCalls, nil, now, "short"); err == nil {
		t.Error("short nonce should fail")
	}
	if n := NewNonce(); len(n) != 32 || strings.Trim(n, "0123456789abcdef") != "" {
		t.Errorf("NewNonce = %q", n)
	}
}

// 固定 lenientUnescape 对非法转义的当前行为（与 Python unquote 不同，见函数注释）：
// 整段不做百分号解码，只把 + 换成空格；合法输入则正常解码。
func TestLenientUnescapeInvalidEscape(t *testing.T) {
	cases := map[string]string{
		"a%41+b":    "aA b",      // 合法：正常解码
		"a%zz%41+b": "a%zz%41 b", // 含非法转义：不解码，仅 + -> 空格
		"100%":      "100%",      // 结尾孤立的 %
		"%e4%bd%a0": "你",         // 合法多字节
		"%e4%bd%zz": "%e4%bd%zz", // 非法 -> 原样
	}
	for in, want := range cases {
		if got := lenientUnescape(in); got != want {
			t.Errorf("lenientUnescape(%q) = %q, want %q", in, got, want)
		}
	}
}
