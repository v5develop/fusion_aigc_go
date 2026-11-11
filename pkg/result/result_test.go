package result

import (
	"context"
	"encoding/json"
	"github.com/zeromicro/go-zero/rest/httpx"
	"google.golang.org/grpc/status"
	"net/http"
	"net/http/httptest"
	"testing"

	"aigc-local-life/pkg/xerr"
)

func TestSuccessBodyHasNoReason(t *testing.T) {
	b, _ := json.Marshal(ok(map[string]int{"n": 1}))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["code"].(float64) != 0 {
		t.Fatalf("成功码应为 0，实际 %v", m["code"])
	}
	if _, has := m["reason"]; has {
		t.Fatalf("成功响应不应带 reason：%s", b)
	}
}

func TestFailureBodyCarriesJavaReason(t *testing.T) {
	cases := []struct {
		code   int
		reason string
	}{
		{xerr.Unauthorized, "UNAUTHORIZED"},
		{xerr.ParamError, "PARAM_INVALID"},
		{xerr.ParamMissing, "PARAM_MISSING"},
		{xerr.NotFound, "RESOURCE_NOT_FOUND"},
		{xerr.Conflict, "RESOURCE_CONFLICT"},
		{xerr.Forbidden, "ACCESS_DENIED"},
		{xerr.InsufficientPoints, "INSUFFICIENT_POINTS"},
		{xerr.ServerError, "SYSTEM_ERROR"},
	}
	for _, c := range cases {
		if got := fail(c.code, "x").Reason; got != c.reason {
			t.Errorf("码 %d：reason 应为 %s，实际 %q", c.code, c.reason, got)
		}
	}
}

// 每个 Go 码的 reason 都必须是 Java ErrorCode 的常量名（同码同名）。
func TestEveryCodeHasJavaReason(t *testing.T) {
	cases := map[int]string{
		xerr.NotReady:     "FEATURE_NOT_READY",
		xerr.AgreementReq: "AGREEMENT_REQUIRED",
	}
	for code, reason := range cases {
		if got := fail(code, "x").Reason; got != reason {
			t.Errorf("码 %d：reason 应为 %s，实际 %q", code, reason, got)
		}
	}
}

func TestUnauthorizedIsHTTP401(t *testing.T) {
	if httpStatus(xerr.Unauthorized) != http.StatusUnauthorized {
		t.Fatalf("未登录应为 HTTP 401")
	}
	if httpStatus(xerr.ParamError) != http.StatusOK {
		t.Fatalf("业务失败保持 HTTP 200")
	}
}

func TestErrorWritesEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	Error(rec, req, xerr.Unauthorized, "登录已过期")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("HTTP 应为 401，实际 %d", rec.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["code"].(float64) != 2001 || m["reason"] != "UNAUTHORIZED" {
		t.Fatalf("信封不对：%s", rec.Body.String())
	}
}

// 错误处理器优先带错误自己的细分 reason（经 gRPC status 透传），码不变；无细分仍按码派生。
func TestErrorHandlerCarriesDetailedReason(t *testing.T) {
	Setup()
	for _, c := range []struct {
		err    error
		reason string
	}{
		{status.Convert(xerr.NewReason(xerr.NotFound, xerr.ReasonMaskSourceNotFound, "执行记录不存在")).Err(), "MASK_SOURCE_NOT_FOUND"},
		{xerr.New(xerr.NotFound, "没识别到对应区域"), "RESOURCE_NOT_FOUND"},
		{xerr.NewReason(xerr.Conflict, xerr.ReasonMaskSourceNotReady, "还没成功"), "MASK_SOURCE_NOT_READY"},
		{xerr.New(xerr.Conflict, "识别太频繁"), "RESOURCE_CONFLICT"},
	} {
		w := httptest.NewRecorder()
		httpx.ErrorCtx(context.Background(), w, c.err)
		var b Body
		if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if b.Reason != c.reason {
			t.Fatalf("reason=%q want %q (%s)", b.Reason, c.reason, w.Body.String())
		}
	}
}
