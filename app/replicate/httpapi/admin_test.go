package httpapi

import (
	"context"
	"errors"
	"testing"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/xerr"
)

// 新4：一次都没读到运行参数时拒绝强制收尾（不拿内核默认值去动钱）；读到过之后，再失败沿用上一次。
func TestAdminForcerRequiresLoadedParams(t *testing.T) {
	fail := true
	load := func(context.Context) (map[string]string, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return map[string]string{KeyEnabled: "true"}, nil
	}
	builds := 0
	f := &AdminForcer{rt: newRuntime(core.DefaultConfig(), false, load, func(cfg core.Config, _ bool) *core.Service {
		builds++
		return core.New(cfg, nil, nil)
	})}
	_, err := f.ForceFinish(context.Background(), 1)
	if code, _, _ := xerr.Parse(err); code != xerr.NotReady {
		t.Fatalf("没读到运行参数应拒绝：%v", err)
	}
	if f.rt.Loaded() {
		t.Fatal("不应标记已读")
	}
	fail = false
	f.rt.at = f.rt.at.Add(-2 * runtimeTTL)
	f.rt.Service(context.Background())
	if !f.rt.Loaded() {
		t.Fatal("读到后应标记")
	}
	fail = true
	f.rt.at = f.rt.at.Add(-2 * runtimeTTL)
	f.rt.Service(context.Background())
	if !f.rt.Loaded() {
		t.Fatal("读到过之后再失败仍可用（沿用上一次）")
	}
}
