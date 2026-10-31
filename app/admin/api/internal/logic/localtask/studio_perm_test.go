package localtask

import (
	"context"
	"testing"

	"aigc-local-life/app/admin/api/internal/middleware"
	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/pkg/xerr"
)

// 共享模式下只有 local_life:update 的管理员，不能经本地生活任务入口重做 / 强制失败 studio 任务（在调 RPC 之前拦下）。
func TestStudioTaskNeedsStudioUpdate(t *testing.T) {
	ctx := ctxWithPerms(t, "local_life:update")
	for _, ref := range []string{"tyo:1", "pse:2", "simg:3"} {
		_, err := NewRetryTaskLogic(ctx, &svc.ServiceContext{}).RetryTask(&types.RetryTaskReq{OriginRef: ref})
		if c, _, _ := xerr.Parse(err); c != xerr.Forbidden {
			t.Fatalf("retry %s: %v", ref, err)
		}
		_, err = NewForceFailTaskLogic(ctx, &svc.ServiceContext{}).ForceFailTask(&types.ForceFailTaskReq{OriginRef: ref})
		if c, _, _ := xerr.Parse(err); c != xerr.Forbidden {
			t.Fatalf("force-fail %s: %v", ref, err)
		}
	}
	if _, err := NewRetryTaskLogic(ctx, &svc.ServiceContext{}).RetryTask(&types.RetryTaskReq{OriginRef: "xyz:1"}); err == nil {
		t.Fatal("未知来源应报错")
	}
}

// ctxWithPerms 模拟中间件在共享模式下放入的权限判断。
func ctxWithPerms(t *testing.T, codes ...string) context.Context {
	t.Helper()
	return middleware.WithPermChecker(context.Background(), func(_ context.Context, code string) (bool, error) {
		for _, c := range codes {
			if c == code {
				return true, nil
			}
		}
		return false, nil
	})
}
