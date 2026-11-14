package core

import (
	"context"
	"sync"
	"testing"
)

// 10 并发、只剩 2 个名额：不超限、没有任何非业务错误（死锁 / 500）。需要 PLAZA_TEST_DSN（见 favorites_integration_test.go）。

func TestUserLibFavoritesConcurrentLimit(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_favorite (user_id, template_id) "+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?) SELECT 1, 100000 + i FROM n", MaxFavorites-2); err != nil {
		t.Fatal(err)
	}
	ids := []int64{101, 102, 103, 104}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.AddFavorite(ctx, 1, ids[i%len(ids)])
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil && reasonOf(err) != ReasonFavoriteLimit {
			t.Fatalf("并发 %d 出现非业务错误: %v", i, err)
		}
	}
	var n int
	if err := s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_favorite WHERE user_id = 1").Scan(ctx, &n); err != nil || n > MaxFavorites {
		t.Fatalf("不能超限: n=%d err=%v", n, err)
	}
	if n != MaxFavorites {
		t.Fatalf("两个名额应被占满: n=%d", n)
	}
}

func TestUserLibMyTemplatesConcurrentLimit(t *testing.T) {
	s, _ := userLibService(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_my_template (user_id, name, template_id, industry_id, variants, text) "+
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?) SELECT 1, 'x', 102, 1, 1, '' FROM n", MaxMyTemplates-2); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 10)
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.CreateMyTemplate(ctx, 1, MyTemplateInput{Name: sPtr("并发"), TemplateID: 102})
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case reasonOf(err) != ReasonMyTemplateLimit:
			t.Fatalf("并发 %d 出现非业务错误: %v", i, err)
		}
	}
	var n int
	_ = s.db.NewRaw("SELECT COUNT(*) FROM afv_plaza_my_template WHERE user_id = 1").Scan(ctx, &n)
	if ok != 2 || n != MaxMyTemplates {
		t.Fatalf("应恰好成功 2 个: ok=%d n=%d", ok, n)
	}
}

func TestLockConflictClassification(t *testing.T) {
	if isLockConflict(nil) || isLockConflict(errBusyRetry()) {
		t.Fatal("非 MySQL 错误不算锁冲突")
	}
	if reasonOf(errBusyRetry()) != reasonTooFrequent {
		t.Fatal("最终失败应是可重试的 TOO_FREQUENT")
	}
}
