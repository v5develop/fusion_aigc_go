package core

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 用户的库（收藏 / 我的模板 / 节点）集成测试：真库跑 SQL（唯一键、上限、行业过滤、隔离），目录用内存夹具。
// 需要临时库里已执行 Go_V1.0.43：
//
//	PLAZA_TEST_DSN='root:123456@tcp(127.0.0.1:43306)/plaza_scratch_userlib_tmp?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27' \
//	  go test ./app/plaza/core -run UserLib -v
//
// 没设 PLAZA_TEST_DSN 时跳过；库名必须含 scratch 或 test（开头会清空这四张表）。

const userLibSchema = `{"schemaVersion":1,"items":[` +
	`{"key":"shop_name","type":"text","label":"店名","required":true,"maxLength":10},` +
	`{"key":"style","type":"select","label":"风格","options":[{"value":"a","label":"A"},{"value":"b","label":"B"}]}]}`

// userLibFixture 在 stdFixture 上给模板 101 挂入参 schema。
func userLibFixture() *fakeSource {
	f := stdFixture()
	f.tpls[0].SlotsJSON = json.RawMessage(userLibSchema)
	return f
}

func userLibService(t *testing.T) (*Service, *fakeSource) {
	t.Helper()
	dsn := os.Getenv("PLAZA_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PLAZA_TEST_DSN，跳过集成测试")
	}
	if !strings.Contains(dsn, "scratch") && !strings.Contains(dsn, "test") {
		t.Fatalf("PLAZA_TEST_DSN 必须指向临时库（库名含 scratch 或 test）：%s", dsn)
	}
	db, err := bundb.New(sqlx.NewMysql(dsn))
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range []string{"afv_plaza_favorite", "afv_plaza_my_template", "afv_plaza_user_lock", "afv_plaza_node_template", "afv_plaza_node"} {
		if _, err := db.ExecContext(context.Background(), "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	src := userLibFixture()
	cfg := Config{}
	cfg.Industry.Enabled = true
	s := NewWithSource(src, cfg, func() time.Time { return t0 })
	s.db = db
	return s, src
}

func favIDs(l *FavoriteList) []int64 {
	var out []int64
	for _, it := range l.Items {
		out = append(out, it.PlazaTemplateID)
	}
	return out
}

func TestUserLibFavoritesIndustryStrictAndIdempotent(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	for _, id := range []int64{101, 103, 104, 101} { // 101 重复收藏：幂等
		if st, err := s.AddFavorite(ctx, 1, id); err != nil || !st.Favorited {
			t.Fatalf("add %d: %v %+v", id, err, st)
		}
	}
	l, err := s.ListFavorites(ctx, 1, "foot_bath", "")
	if err != nil {
		t.Fatal(err)
	}
	// foot_bath 含其子级 bluev：101、103；104 属于 spa 不出现；收藏时间倒序（103 比 101 晚）
	if got := favIDs(l); len(got) != 2 || got[0] != 103 || got[1] != 101 || l.Total != 2 || l.Limit != MaxFavorites {
		t.Fatalf("foot_bath 收藏: %v %+v", got, l)
	}
	if l, _ = s.ListFavorites(ctx, 1, "spa", ""); len(l.Items) != 1 || l.Items[0].PlazaTemplateID != 104 || l.Items[0].FavoritedAt == "" {
		t.Fatalf("spa 收藏: %+v", l)
	}
	if l, _ = s.ListFavorites(ctx, 1, "foot_bath", "foot_bath_bluev"); len(l.Items) != 1 || l.Items[0].PlazaTemplateID != 103 {
		t.Fatalf("二级只含自己: %+v", l)
	}
	var n int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_favorite WHERE user_id = 1").Scan(ctx, &n); err != nil || n != 3 {
		t.Fatalf("重复收藏只落一行: n=%d err=%v", n, err)
	}
	// 缺行业 / 行业失效
	if _, err := s.ListFavorites(ctx, 1, "", ""); xerr.ReasonOf(err) != xerr.Reason(xerr.ParamMissing) {
		t.Fatalf("缺 industry: %v", err)
	}
	if _, err := s.ListFavorites(ctx, 1, "dead", ""); reasonOf(err) != ReasonIndustryNotFound {
		t.Fatalf("停用行业: %v", err)
	}
	// 不可见模板不能收藏（105 只挂在停用行业）、不存在的也不行
	for _, id := range []int64{105, 999} {
		if _, err := s.AddFavorite(ctx, 1, id); err == nil || reasonOf(err) == "" {
			t.Fatalf("收藏 %d 应被拒: %v", id, err)
		}
	}
	// 取消收藏幂等
	for i := 0; i < 2; i++ {
		if st, err := s.RemoveFavorite(ctx, 1, 101); err != nil || st.Favorited {
			t.Fatalf("remove: %v %+v", err, st)
		}
	}
	if l, _ = s.ListFavorites(ctx, 1, "foot_bath", ""); len(l.Items) != 1 {
		t.Fatalf("取消后: %+v", l)
	}
}

func TestUserLibFavoritesUserIsolation(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	if _, err := s.AddFavorite(ctx, 1, 101); err != nil {
		t.Fatal(err)
	}
	if l, err := s.ListFavorites(ctx, 2, "foot_bath", ""); err != nil || len(l.Items) != 0 {
		t.Fatalf("别人看不到: %v %+v", err, l)
	}
	if _, err := s.RemoveFavorite(ctx, 2, 101); err != nil { // 删别人的 = 没有效果
		t.Fatal(err)
	}
	if l, _ := s.ListFavorites(ctx, 1, "foot_bath", ""); len(l.Items) != 1 {
		t.Fatalf("自己的没被删: %+v", l)
	}
	if _, err := s.AddFavorite(ctx, 0, 101); reasonOf(err) != xerr.Reason(xerr.Unauthorized) {
		t.Fatalf("未登录: %v", err)
	}
}

func TestUserLibFavoritesOfflineHiddenButKept(t *testing.T) {
	s, src := userLibService(t)
	ctx := context.Background()
	if _, err := s.AddFavorite(ctx, 1, 102); err != nil {
		t.Fatal(err)
	}
	src.tpls[1].State = model.StateOffline
	s.Invalidate(ctx)
	if l, _ := s.ListFavorites(ctx, 1, "foot_bath", ""); len(l.Items) != 0 {
		t.Fatalf("下线后不显示: %+v", l)
	}
	var n int
	_ = s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_favorite WHERE user_id = 1 AND template_id = 102").Scan(ctx, &n)
	if n != 1 {
		t.Fatal("下线不删记录")
	}
	src.tpls[1].State = model.StatePublished // 重新上架：自然回来
	s.Invalidate(ctx)
	if l, _ := s.ListFavorites(ctx, 1, "foot_bath", ""); len(l.Items) != 1 {
		t.Fatalf("重新上架后回来: %+v", l)
	}
	src.tpls[1].State = model.StateOffline
	s.Invalidate(ctx)
	if _, err := s.RemoveFavorite(ctx, 1, 102); err != nil { // 下线的也能取消
		t.Fatal(err)
	}
	_ = s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_favorite WHERE user_id = 1").Scan(ctx, &n)
	if n != 0 {
		t.Fatal("下线的模板也能取消收藏")
	}
}

func TestUserLibFavoritesLimit(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_favorite (user_id, template_id) "+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?) SELECT 1, 100000 + i FROM n", MaxFavorites-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFavorite(ctx, 1, 101); err != nil { // 第 500 个
		t.Fatal(err)
	}
	if _, err := s.AddFavorite(ctx, 1, 102); reasonOf(err) != ReasonFavoriteLimit { // 第 501 个
		t.Fatalf("应满: %v", err)
	}
	if st, err := s.AddFavorite(ctx, 1, 101); err != nil || !st.Favorited { // 已收藏的再收藏仍成功
		t.Fatalf("已收藏幂等: %v", err)
	}
	if _, err := s.AddFavorite(ctx, 2, 102); err != nil { // 别人不受影响
		t.Fatal(err)
	}
	if _, err := s.RemoveFavorite(ctx, 1, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFavorite(ctx, 1, 102); err != nil { // 腾出位置后可收藏
		t.Fatal(err)
	}
}
