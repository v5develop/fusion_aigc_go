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
		// 广场后台：前缀映射 + 路由级覆盖
		{"GET", "/api/v1/plaza/admin/industries", "creaft_catalog:view"},
		{"POST", "/api/v1/plaza/admin/industries", "creaft_catalog:edit"},
		{"PUT", "/api/v1/plaza/admin/templates/9", "creaft_catalog:edit"},
		{"POST", "/api/v1/plaza/admin/templates/9/duplicate", "creaft_catalog:edit"},
		{"POST", "/api/v1/plaza/admin/templates/9/publish", "plaza:publish"},
		{"POST", "/api/v1/plaza/admin/templates/9/offline", "plaza:publish"},
		{"POST", "/api/v1/plaza/admin/templates/batch/publish", "plaza:publish"},
		{"POST", "/api/v1/plaza/admin/templates/batch/offline", "plaza:publish"},
		{"POST", "/api/v1/plaza/admin/templates/batch/assign", "creaft_catalog:edit"},
		// 改行业关系：中间件层要编辑权限；已发布 / 已有行业的模板还要 plaza:publish（handler 补查、core 事务内按数据判定）
		{"POST", "/api/v1/plaza/admin/templates/batch/unassign", "creaft_catalog:edit"},
		{"POST", "/api/v1/plaza/admin/templates/batch/replaceIndustries", "creaft_catalog:edit"},
		{"POST", "/api/v1/plaza/admin/templates/batch/setOrder", "creaft_catalog:edit"},
		{"PUT", "/api/v1/plaza/admin/templates/9/industries", "creaft_catalog:edit"},
		{"PUT", "/api/v1/plaza/admin//templates/9/industries", "creaft_catalog:edit"},
		{"POST", "/api/v1/plaza/admin/templates/batch/delete", "plaza:delete"},
		{"POST", "/api/v1/plaza/admin/templates/batch/restore", "plaza:delete"},
		{"DELETE", "/api/v1/plaza/admin/templates/9", "plaza:delete"},
		{"POST", "/api/v1/plaza/admin/templates/9/restore", "plaza:delete"},
		{"DELETE", "/api/v1/plaza/admin/templates/9/purge", "plaza:delete"},
		{"GET", "/api/v1/plaza/admin/templates/9/audit", "plaza:audit"},
		{"GET", "/api/v1/plaza/admin/audit", "plaza:audit"},
		{"GET", "/api/v1/plaza/admin/templates/9", "creaft_catalog:view"},
		{"GET", "/api/v1/plaza/admin/templates/trash", "creaft_catalog:view"},
		{"DELETE", "/api/v1/plaza/admin/industries/3", "creaft_catalog:edit"},
		// path.Clean 绕过：不能借普通写权限绕过发布 / 删除 / 审计
		{"POST", "/api/v1/plaza/admin//templates/9/publish", "plaza:publish"},
		{"POST", "/api/v1/plaza/admin/industries/../templates/9/publish", "plaza:publish"},
		{"DELETE", "/api/v1/plaza/admin/./templates/9", "plaza:delete"},
		{"GET", "/api/v1/plaza/admin/x/../audit", "plaza:audit"},
		{"GET", "/api/v1/plaza/adminx/audit", "local_life:view"},
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

func TestIndustryRelationRoutes(t *testing.T) {
	for _, a := range []string{"assign", "unassign", "replaceIndustries", "setOrder"} {
		if !IndustryRelationAction(a) {
			t.Errorf("%s 会改行业关系，handler 必须补查 plaza:publish", a)
		}
		if got := routePerm("POST", "/api/v1/plaza/admin/templates/batch/"+a); got != permIndustryRelation {
			t.Errorf("batch/%s 应在 routePerm 显式登记：%q", a, got)
		}
	}
	for _, a := range []string{"publish", "offline", "delete", "restore", "x"} {
		if IndustryRelationAction(a) {
			t.Errorf("%s 不是行业关系动作", a)
		}
	}
	if got := routePerm("PUT", "/api/v1/plaza/admin/templates/9/industries"); got != permIndustryRelation {
		t.Errorf("PUT /templates/{id}/industries 应在 routePerm 显式登记：%q", got)
	}
}
