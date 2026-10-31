package canvas

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/rest/pathvar"
)

func TestParseSaveKeepsRawSnapshot(t *testing.T) {
	body := `{"if_match_revision":"1-a","force":false,"title":"t","snapshot":{"nodes":[{"id":"n","data":{"seed":12345678901234567890,"t":"<a&b>"}}],"edges":[]}}`
	r := pathvar.WithVars(httptest.NewRequest(http.MethodPut, "/api/v1/canvas/canvases/cv_1", strings.NewReader(body)), map[string]string{"id": "cv_1"})
	var req types.CanvasSaveReq
	if err := parseSave(r, &req); err != nil {
		t.Fatal(err)
	}
	raw, ok := req.Snapshot.(json.RawMessage)
	if !ok {
		t.Fatalf("snapshot 应为 json.RawMessage: %T", req.Snapshot)
	}
	if !strings.Contains(string(raw), "12345678901234567890") || !strings.Contains(string(raw), "<a&b>") {
		t.Fatalf("大整数不丢精度、不做 HTML 转义: %s", raw)
	}
	if req.Id != "cv_1" || req.IfMatchRevision != "1-a" || req.Title == nil || *req.Title != "t" {
		t.Fatalf("其余字段照常解析: %+v", req)
	}
	r = pathvar.WithVars(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"force":true}`)), map[string]string{"id": "cv_1"})
	req = types.CanvasSaveReq{}
	if err := parseSave(r, &req); err != nil || req.Snapshot != nil || !req.Force {
		t.Fatalf("缺 snapshot 留空交给 logic 报 1002: %+v %v", req, err)
	}
}

func TestParseRawPathHeaderNotOverriddenByBody(t *testing.T) {
	body := `{"id":"cv_EVIL","nodeid":"n_EVIL","node_id":"n_EVIL","key":"k_EVIL","Key":"k_EVIL","kind":"image"}`
	r := pathvar.WithVars(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), map[string]string{"id": "cv_A", "node_id": "n_A"})
	r.Header.Set("Idempotency-Key", "k_A")
	var req types.CanvasRunReq
	if err := parseRaw(r, &req); err != nil {
		t.Fatal(err)
	}
	if req.Id != "cv_A" || req.NodeId != "n_A" || req.Key != "k_A" || req.Kind != "image" {
		t.Fatalf("path/header 应胜出: %+v", req)
	}
	// 缺 Idempotency-Key 头时，请求体的 key 不能顶替
	r = pathvar.WithVars(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body)), map[string]string{"id": "cv_A", "node_id": "n_A"})
	req = types.CanvasRunReq{}
	if err := parseRaw(r, &req); err == nil && req.Key != "" {
		t.Fatalf("请求体 key 绕过头部: %+v", req)
	}
}

func TestParseSavePathNotOverriddenByBody(t *testing.T) {
	r := pathvar.WithVars(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"id":"cv_EVIL","snapshot":{}}`)), map[string]string{"id": "cv_A"})
	var req types.CanvasSaveReq
	if err := parseSave(r, &req); err != nil || req.Id != "cv_A" {
		t.Fatalf("%+v %v", req, err)
	}
}
