package javatool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJavaTaskIDOf(t *testing.T) {
	cases := map[string]string{
		"cv_AiCreation_LF_17e4abacc7d22b7ae2fa993e270e832d_run_1361": "AiCreation_LF_17e4abacc7d22b7ae2fa993e270e832d",
		"cv_AiCreation_LF_abc_run_zz":                                "", // 后缀不是 4 位十六进制
		"cv_nounderscore":                                            "",
		"":                                                           "",
	}
	for in, want := range cases {
		if got := JavaTaskIDOf(in); got != want {
			t.Errorf("JavaTaskIDOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLinkGatewayCallsPostsCallIDs(t *testing.T) {
	var gotPath, gotAuth, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotCT = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":null}`)
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	err := c.LinkGatewayCalls(context.Background(), "tok123", "AiCreation_LF_abc", []string{"call_00Bbx3009cATneSY77OTry"})
	if err != nil {
		t.Fatalf("LinkGatewayCalls: %v", err)
	}
	if gotPath != "/api/v1/creation/tasks/AiCreation_LF_abc/gateway-calls" {
		t.Errorf("path=%q", gotPath)
	}
	if gotAuth != "Bearer tok123" || gotCT != "application/json" {
		t.Errorf("auth=%q content-type=%q", gotAuth, gotCT)
	}
	ids, _ := gotBody["callIds"].([]any)
	if len(ids) != 1 || ids[0] != "call_00Bbx3009cATneSY77OTry" {
		t.Errorf("body=%v", gotBody)
	}
}

func TestLinkGatewayCallsGuards(t *testing.T) {
	c := New("http://unused", time.Second)
	if err := c.LinkGatewayCalls(context.Background(), "", "T", []string{"c"}); err != ErrNoBearer {
		t.Errorf("no bearer: %v", err)
	}
	if err := c.LinkGatewayCalls(context.Background(), "tok", "T", nil); err != nil {
		t.Errorf("empty ids should be a no-op, got %v", err)
	}
	if err := New("", time.Second).LinkGatewayCalls(context.Background(), "tok", "T", []string{"c"}); err != ErrNotConfigured {
		t.Errorf("not configured: %v", err)
	}
}
