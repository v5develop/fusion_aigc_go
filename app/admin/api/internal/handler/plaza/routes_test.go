package plaza

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/pkg/result"

	"github.com/zeromicro/go-zero/rest"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// 路由表里静态段与参数段混用（/templates/trash 与 /templates/:id、/templates/batch/:action 与 /templates/:id/publish、/industries/sort 与 /industries/:id）
// 必须各自命中自己的 handler：Plaza 服务为 nil 时 handler 回「功能暂未开放」(7003)，404 / 405 就说明路由撞了。
func TestRoutesDoNotCollide(t *testing.T) {
	port := freePort(t)
	var c rest.RestConf
	c.Host, c.Port, c.Name = "127.0.0.1", port, "plaza-route-test"
	c.Log.Mode = "console"
	server := rest.MustNewServer(c)
	defer server.Stop()
	result.Setup()
	passthrough := func(next http.HandlerFunc) http.HandlerFunc { return next }
	Register(server, &svc.ServiceContext{AdminAuth: passthrough})
	go server.Start()
	base := fmt.Sprintf("http://127.0.0.1:%d/api/v1/plaza/admin", port)
	for i := 0; i < 50; i++ {
		if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cases := []struct{ method, path string }{
		{"GET", "/industries"}, {"POST", "/industries"}, {"PUT", "/industries/sort"}, {"PUT", "/industries/3"}, {"DELETE", "/industries/3"},
		{"GET", "/industries/3/templates"}, {"PUT", "/industries/3/templates/order"},
		{"GET", "/templates"}, {"POST", "/templates"}, {"GET", "/templates/trash"}, {"GET", "/templates/9"}, {"PUT", "/templates/9"}, {"PUT", "/templates/9/slots"}, {"PUT", "/templates/9/industries"}, {"PUT", "/templates/9/attrs"}, {"GET", "/capabilities"}, {"GET", "/templates/unassigned-summary"}, {"DELETE", "/templates/9"},
		{"POST", "/templates/batch/publish"}, {"POST", "/templates/batch/delete"}, {"POST", "/templates/9/publish"}, {"POST", "/templates/9/reprocess-media"}, {"DELETE", "/channels/1"}, {"POST", "/templates/9/offline"},
		{"POST", "/templates/9/duplicate"}, {"POST", "/templates/9/restore"}, {"DELETE", "/templates/9/purge"}, {"GET", "/templates/9/audit"},
		{"GET", "/audit"}, {"GET", "/preview/templates"}, {"GET", "/channels"}, {"POST", "/channels"}, {"PUT", "/channels/1"},
		{"GET", "/samples"}, {"POST", "/samples"}, {"PUT", "/samples/1"}, {"DELETE", "/samples/1"},
		{"GET", "/industry-suggestions"}, {"POST", "/industry-suggestions/abc/adopt"}, {"POST", "/industry-suggestions/abc/ignore"},
		{"GET", "/search-misses"}, {"POST", "/search-misses/status"},
		{"GET", "/nodes"}, {"POST", "/nodes"}, {"GET", "/nodes/5"}, {"PUT", "/nodes/5"}, {"PUT", "/nodes/5/enabled"}, {"DELETE", "/nodes/5"},
		{"GET", "/nodes/5/templates"}, {"POST", "/nodes/5/templates"}, {"PUT", "/nodes/5/templates/order"}, {"DELETE", "/nodes/5/templates/9"},
		{"POST", "/uploads/presign"}, {"POST", "/uploads/confirm"}, {"GET", "/skills"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var env struct {
			Code int `json:"code"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(b, &env) != nil || env.Code != 7003 {
			t.Errorf("%s %s 没命中自己的 handler: %d %s", c.method, c.path, resp.StatusCode, b)
		}
	}
}
