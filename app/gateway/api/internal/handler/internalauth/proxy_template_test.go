package internalauth_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"aigc-local-life/app/gateway/api/internal/handler/canvasagent"
	"aigc-local-life/app/gateway/api/internal/handler/replicateagent"
)

// 部署用的内网代理模板（deploy/server/nginx/canvas-internal.conf.tmpl）必须放行每个服务间入口：
// 漏放行的路径被代理回 404，Java 把它当成「能力未开启」（2026-11-20 t81 复刻入口即如此，画布正常）。
// 新增服务间入口时把它的前缀加进 prefixes，并同步改模板。
func TestInternalProxyTemplateAllowsEveryInternalEntry(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "server", "nginx", "canvas-internal.conf.tmpl"))
	if err != nil {
		t.Fatalf("读代理模板: %v", err)
	}
	var patterns []*regexp.Regexp
	var prefixes []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) < 3 || f[0] != "location" {
			continue
		}
		switch f[1] {
		case "~", "~*":
			patterns = append(patterns, regexp.MustCompile(f[2]))
		case "^~", "=":
			prefixes = append(prefixes, f[2])
		}
	}
	allowed := func(path string) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(path, p) {
				return true
			}
		}
		for _, re := range patterns {
			if re.MatchString(path) {
				return true
			}
		}
		return false
	}
	for _, prefix := range []string{canvasagent.InternalPrefix, replicateagent.InternalPrefix} {
		for _, path := range []string{prefix + "/agent/call", prefix + "/agent/tools"} {
			if !allowed(path) {
				t.Errorf("代理模板没有放行 %s：Java 调用会被代理回 404（=「能力未开启」）", path)
			}
		}
	}
	if allowed("/internal/v1/unknown/agent/call") || allowed("/api/v1/canvas/canvases") {
		t.Error("代理模板放行范围过宽：只应放行服务间入口")
	}
}
