package middleware

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestPermFor(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"GET", "/api/v1/local-life/admin/notes", "local_life:view"},
		{"POST", "/api/v1/local-life/admin/tasks/retry", "local_life:update"},
		{"GET", "/api/v1/asset-library/admin/tags", "asset_library:view"},
		{"POST", "/api/v1/asset-library/admin/tags", "asset_library:update"},
		{"GET", "/api/v1/studio/admin/batches", "studio:view"},
		{"GET", "/api/v1/studio/admin/cost-summary", "studio:view"},
		{"POST", "/api/v1/studio/admin/tasks/1/retry", "studio:update"},
		{"POST", "/api/v1/studio/admin/tasks/1/force-fail", "studio:update"},
		{"GET", "/api/v1/canvas/admin/runs", "canvas:view"},
		{"POST", "/api/v1/canvas/admin/runs/1/force-fail", "canvas:update"},
		{"PUT", "/api/v1/canvas/admin/templates/1", "canvas:update"},
		{"GET", "/api/v1/prompt-library/admin/prompts", "canvas:view"},
		{"POST", "/api/v1/prompt-library/admin/prompts", "canvas:update"},
		// 不规范路径：按 path.Clean 后的路径取权限（与 go-zero 路由同口径），不能借 local_life 权限绕进 studio / 资产库
		{"POST", "/api/v1//studio/admin/tasks/1/force-fail", "studio:update"},
		{"GET", "/api/v1//studio/admin/batches", "studio:view"},
		{"POST", "/api/v1/local-life/../studio/admin/tasks/1/retry", "studio:update"},
		{"POST", "/api/v1/local-life/admin/../../asset-library/admin/tags", "asset_library:update"},
		{"POST", "/api/v1/studio/admin/./tasks/1/retry", "studio:update"},
		{"GET", "/api/v1/studio/admin", "studio:view"},
		// 整段匹配：前缀后面不是 / 的不算
		{"GET", "/api/v1/studio/adminx/batches", "local_life:view"},
		{"POST", "/api/v1/canvas/administrators", "local_life:update"},
	}
	for _, c := range cases {
		if got := permFor(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s: got %s want %s", c.method, c.path, got, c.want)
		}
	}
}

// 独立模式（ctx 里没有判断函数）放行；共享模式按放入的判断函数。
func TestAllowedFromContext(t *testing.T) {
	ctx := context.Background()
	if ok, err := Allowed(ctx, "studio:update"); !ok || err != nil {
		t.Fatalf("独立模式应放行：%v %v", ok, err)
	}
	fn := func(_ context.Context, code string) (bool, error) { return code == "local_life:update", nil }
	ctx = context.WithValue(ctx, permCheckerKey{}, fn)
	if err := Require(ctx, "studio:update"); err == nil {
		t.Fatal("没有 studio:update 应拒绝")
	}
	if err := Require(ctx, "local_life:update"); err != nil {
		t.Fatal(err)
	}
}
