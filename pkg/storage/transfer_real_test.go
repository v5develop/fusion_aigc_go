package storage

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// TestTransferReal 用现有任务里的私有桶产物真实转存一次（默认跳过）：
//
//	OSS_REAL=1 OSS_DSN='…' go test ./pkg/storage -run TransferReal -v
func TestTransferReal(t *testing.T) {
	dsn := os.Getenv("OSS_DSN")
	if os.Getenv("OSS_REAL") != "1" || dsn == "" {
		t.Skip("设置 OSS_REAL=1 与 OSS_DSN 后才会连真实账号")
	}
	conn := sqlx.NewMysql(dsn)
	var src string
	q := "SELECT transfer_source_url FROM afv_creation_task " +
		"WHERE transfer_source_url LIKE 'http%' AND transfer_source_url NOT LIKE '%[%' ORDER BY id DESC LIMIT 1"
	if err := conn.QueryRowCtx(context.Background(), &src, q); err != nil {
		t.Skipf("库里没有可用的原始产物链接: %v", err)
	}
	t.Logf("source: %s", src)

	s := New(Conf{Mode: "oss", Prefix: "aigc-life"}, conn).(*ossStore)
	ctx := context.Background()

	// 私有桶不对外可读，直接下载应当拿不到
	if resp, err := http.Get(src); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Logf("注意：该地址目前可直接访问（HTTP 200），仍会转存")
		} else {
			t.Logf("直接访问被拒（HTTP %d），符合私有桶预期", resp.StatusCode)
		}
	}

	dst, err := s.Transfer(ctx, src, "selftest", 0)
	if err != nil {
		t.Fatalf("转存失败: %v", err)
	}
	t.Logf("transferred: %s", dst)
	cfg, _ := s.config(ctx)
	defer deleteObject(t, s, cfg, objectKeyOf(dst))

	if !strings.HasPrefix(dst, cfg.publicBase()+"/aigc-life/selftest/") {
		t.Fatalf("转存后的地址不对: %s", dst)
	}
	size, ct, err := s.Stat(ctx, dst)
	if err != nil || size <= 0 {
		t.Fatalf("转存后不可访问: size=%d ct=%s err=%v", size, ct, err)
	}
	t.Logf("public copy: %d bytes, %s", size, ct)

	// 已经在公有桶的地址不应重复转存
	if again, err := s.Transfer(ctx, dst, "selftest", 0); err != nil || again != dst {
		t.Fatalf("公有桶地址应原样返回: %s %v", again, err)
	}
}
