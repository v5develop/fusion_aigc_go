package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/rest/pathvar"
)

// 确认 / 继续 / 重做都必须带 expectedPoints（页面展示的本次点数）：缺失 / 非正数回 PARAM_MISSING，不进内核、不动钱。
func TestExpectedPointsRequired(t *testing.T) {
	h := &handlers{}
	cases := []struct {
		name string
		fn   handlerFn
		vars map[string]string
		pre  string // 其余必填字段
	}{
		{"confirm", h.confirm, map[string]string{"no": "VRPx"}, `"planHash":"h","stage":"first"`},
		{"continue", h.cont, map[string]string{"id": "42"}, `"planHash":"h"`},
		{"redo", h.redo, map[string]string{"id": "42", "k": "2"}, ``},
	}
	for _, c := range cases {
		for _, pts := range []string{"", `"expectedPoints":0`, `"expectedPoints":-5`, `"expectedPoints":null`} {
			parts := []string{}
			for _, x := range []string{c.pre, pts} {
				if x != "" {
					parts = append(parts, x)
				}
			}
			body := "{" + strings.Join(parts, ",") + "}"
			r := httptest.NewRequest("POST", "/x", strings.NewReader(body))
			r = pathvar.WithVars(r, c.vars)
			// Scope 是空壳（没有内核、没装模块）：校验必须在查重放 / 限频 / 调内核之前完成，否则这里会空指针
			_, err := c.fn(httptest.NewRecorder(), r, &Scope{uid: 7, write: true})
			if code, _, ok := xerr.Parse(err); err == nil || !ok || code != xerr.ParamMissing {
				t.Fatalf("%s body=%s 应回 PARAM_MISSING：%v", c.name, body, err)
			}
		}
	}
}
