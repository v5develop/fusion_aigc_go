package notify

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func TestSendValidates(t *testing.T) {
	var nilSvc *Service
	if _, err := nilSvc.Send(context.Background(), Message{UserID: 1, Title: "t", DedupeKey: "k"}); err == nil {
		t.Fatal("没装配库应报错")
	}
	if New(nil) != nil {
		t.Fatal("conn 为 nil 返回 nil")
	}
	s := &Service{}
	for _, m := range []Message{{Title: "t", DedupeKey: "k"}, {UserID: 1, DedupeKey: "k"}, {UserID: 1, Title: "t"},
		{UserID: 1, Title: "t", DedupeKey: strings.Repeat("k", 129)}} {
		if out, err := s.Send(context.Background(), m); err != ErrInvalid || out != Skipped {
			t.Fatalf("应拒收：%+v %v", m, err)
		}
	}
}

// 集成（NOTIFY_TEST_DSN，临时库里有 afv_notification（含 Java 补的 link/biz_type/biz_id/dedupe_key + uk_dedupe_key））。
func TestIntegrationSendIdempotentConcurrent(t *testing.T) {
	dsn := os.Getenv("NOTIFY_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NOTIFY_TEST_DSN，跳过")
	}
	if !strings.Contains(dsn, "scratch") {
		t.Fatalf("必须是临时库：%s", dsn)
	}
	ctx := context.Background()
	conn := sqlx.NewMysql(dsn)
	clean := func() { _, _ = conn.ExecCtx(ctx, "DELETE FROM afv_notification WHERE target_user_id = 990099") }
	clean()
	t.Cleanup(clean)
	s := New(conn)
	m := Message{UserID: 990099, Kind: "replicate.done", Title: strings.Repeat("标", 200), Body: "3 段全部成功，点击查看",
		Link: "/video/replicate?job=1", BizType: "replicate_job", BizID: "1", DedupeKey: "replicate:1:replicate.done"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[Outcome]int{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := s.Send(ctx, m)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			got[out]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got[Sent] != 1 || got[Duplicate] != 9 {
		t.Fatalf("同一去重键只写一条：%v", got)
	}
	var row struct {
		Title  string `db:"title"`
		Type   string `db:"notification_type"`
		Scope  string `db:"target_scope"`
		Status int    `db:"status"`
		Link   string `db:"link"`
		Pub    int    `db:"pub"`
	}
	if err := conn.QueryRowCtx(ctx, &row, "SELECT title, notification_type, target_scope, status, link, publish_time <= NOW() AS pub FROM afv_notification WHERE target_user_id = 990099"); err != nil {
		t.Fatal(err)
	}
	if len([]rune(row.Title)) != 128 || row.Type != "SYSTEM" || row.Scope != "USER" || row.Status != 1 || row.Link != m.Link || row.Pub != 1 {
		t.Fatalf("Java 用户端能列出的定向通知：%+v", row)
	}
}
