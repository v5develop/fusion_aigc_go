package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zeromicro/go-zero/rest/pathvar"
	"github.com/zeromicro/go-zero/rest/router"
)

// CONTRACT §AA：素材识别 / 想怎么改走固定限频（不读运行参数），两个动作互不影响，也不影响其它动作。
func TestClassifyAndWishRateLimitFixedAndIndependent(t *testing.T) {
	l := newRateLimiter(nil, nil)
	ctx := context.Background()
	p := RateParams{} // 配置全 0（不限）时仍有固定上限
	for i := 0; i < classifyPerMinute; i++ {
		if _, err := l.take(ctx, ActionClassify, 9, p); err != nil {
			t.Fatalf("识别第 %d 次应放行：%v", i+1, err)
		}
	}
	if _, err := l.take(ctx, ActionClassify, 9, p); err == nil {
		t.Fatal("识别超过每分钟上限应限频")
	}
	for i := 0; i < wishPerMinute; i++ {
		if _, err := l.take(ctx, ActionWish, 9, p); err != nil {
			t.Fatalf("想怎么改第 %d 次应放行（与识别互不占额度）：%v", i+1, err)
		}
	}
	_, err := l.take(ctx, ActionWish, 9, p)
	if rl, ok := err.(*RateLimitError); !ok || rl.Check != "RATE_MINUTE" {
		t.Fatalf("想怎么改超过每分钟上限应限频：%v", err)
	}
	if _, err := l.take(ctx, ActionFeedback, 9, p); err != nil {
		t.Fatalf("不影响其它动作：%v", err)
	}
	// 日上限文案是各自的（用户能分清是哪个功能用完了）
	if dayText[ActionClassify] == "" || dayText[ActionWish] == "" || dayText[ActionClassify] == dayText[ActionWish] {
		t.Fatal("两个动作应各有每日用完的文案")
	}
}

// 路径含冒号的 refs:classify 必须真能被 go-zero 路由命中（不被当成路径参数），且 wish 的 :no 取得到。
func TestAARoutesMatchRouter(t *testing.T) {
	rt := router.NewRouter()
	var got []string
	mk := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = append(got, name+":"+pathvar.Vars(r)["no"])
		})
	}
	for _, rt0 := range []struct{ method, path, name string }{
		{http.MethodPost, prefix + "/refs:classify", "classify"},
		{http.MethodPost, prefix + "/analyses/:no/wish", "wish"},
		{http.MethodGet, prefix + "/analyses/:no", "get"},
	} {
		if err := rt.Handle(rt0.method, rt0.path, mk(rt0.name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, prefix + "/refs:classify"},
		{http.MethodPost, prefix + "/analyses/VRA1/wish"},
		{http.MethodGet, prefix + "/analyses/VRA1"},
	} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s 应命中路由：%d", c.method, c.path, w.Code)
		}
	}
	want := []string{"classify:", "wish:VRA1", "get:VRA1"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("路由命中 got=%v want=%v", got, want)
	}
}
