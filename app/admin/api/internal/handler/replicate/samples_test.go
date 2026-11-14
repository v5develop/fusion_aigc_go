package replicate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"aigc-local-life/app/admin/api/internal/middleware"
	"aigc-local-life/pkg/xerr"
)

// 写示例要 replicate:sample：没有权限直接 Forbidden，不进业务。
func TestNeedSamplePermission(t *testing.T) {
	called := false
	fn := needSample(func(*http.Request) (any, error) { called = true; return "ok", nil })
	deny := func(_ context.Context, code string) (bool, error) { return code != permSample, nil }
	r := httptest.NewRequest("POST", prefix+"/samples", nil)
	r = r.WithContext(middleware.WithPermChecker(r.Context(), deny))
	if _, err := fn(r); err == nil || called {
		t.Fatalf("没有 replicate:sample 应拒绝：%v called=%v", err, called)
	} else if code, _, _ := xerr.Parse(err); code != xerr.Forbidden {
		t.Fatalf("应 Forbidden：%v", err)
	}
	allow := func(context.Context, string) (bool, error) { return true, nil }
	r = r.WithContext(middleware.WithPermChecker(context.Background(), allow))
	if out, err := fn(r); err != nil || out != "ok" || !called {
		t.Fatalf("有权限放行：%v %v", out, err)
	}
}
