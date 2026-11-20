package canvas

import (
	"reflect"
	"strings"
	"testing"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/gateway/api/internal/types"
)

// SSE run 事件里的 run（canvasevent.RunView）必须与 GET /runs/{id} 的 CanvasRunResult 同形：
// 每个接口字段都有对应的 camelCase 键、类型一致；事件只多出本地外推进度用的两项与事件版本 version。字段只增不改，新增要两边一起加。
func TestRunEventViewMatchesRunResult(t *testing.T) {
	extra := map[string]bool{"elapsedMs": true, "expectedMs": true, "version": true}
	sameShape(t, reflect.TypeOf(types.CanvasRunResult{}), reflect.TypeOf(canvasevent.RunView{}), extra)
	sameShape(t, reflect.TypeOf(types.CanvasCandidate{}), reflect.TypeOf(canvasevent.RunCandidate{}), nil)
}

func sameShape(t *testing.T, api, ev reflect.Type, extra map[string]bool) {
	t.Helper()
	apiKeys, evKeys := jsonKeys(api), jsonKeys(ev)
	for k, f := range apiKeys {
		g, ok := evKeys[camel(k)]
		if !ok {
			t.Errorf("%s.%s（json %q）在事件 %s 里缺少 camelCase 键 %q", api.Name(), f.Name, k, ev.Name(), camel(k))
			continue
		}
		if f.Type.Kind() != g.Type.Kind() {
			t.Errorf("%s 类型不一致：%s vs %s", k, f.Type, g.Type)
		}
	}
	for k := range evKeys {
		found := extra[k]
		for a := range apiKeys {
			found = found || camel(a) == k
		}
		if !found {
			t.Errorf("事件 %s 多出接口里没有的键 %q（只增：先加到 gateway.api 的 %s）", ev.Name(), k, api.Name())
		}
	}
}

func jsonKeys(t reflect.Type) map[string]reflect.StructField {
	out := map[string]reflect.StructField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = f
		}
	}
	return out
}

func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}
