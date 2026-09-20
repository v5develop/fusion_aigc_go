package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// TestOSSRealUpload 连真实公有桶跑一遍：上传 → 公网访问 → 删除。
// 默认跳过，需要显式开启：
//
//	OSS_REAL=1 OSS_DSN='root:***@tcp(127.0.0.1:3306)/fusion_aigc' go test ./pkg/storage -run OSSReal -v
func TestOSSRealUpload(t *testing.T) {
	dsn := os.Getenv("OSS_DSN")
	if os.Getenv("OSS_REAL") != "1" || dsn == "" {
		t.Skip("设置 OSS_REAL=1 与 OSS_DSN 后才会连真实的桶")
	}
	conn := sqlx.NewMysql(dsn)
	s := New(Conf{Mode: "oss", Prefix: "aigc-life"}, conn).(*ossStore)
	ctx := context.Background()

	cfg, err := s.config(ctx)
	if err != nil {
		t.Fatalf("读取 afv_system_config 的 storage_public_*: %v", err)
	}
	t.Logf("bucket=%s host=%s base=%s", cfg.Bucket, cfg.host(), cfg.publicBase())

	key := ObjectKey("aigc-life", "selftest", 0, "txt")
	body := []byte("aigc-life storage self test " + time.Now().Format(time.RFC3339))
	url, err := s.Put(ctx, key, "text/plain", body)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	t.Logf("uploaded: %s", url)
	if !s.Owns(url) {
		t.Errorf("Owns 应认出自己签发的地址: %s", url)
	}
	defer deleteObject(t, s, cfg, key)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("公网访问失败: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("公网访问返回 %d: %s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("内容不一致: %s", got)
	}
}

// deleteObject 测试收尾：把刚上传的对象删掉，同样用 OSS V1 签名。
func deleteObject(t *testing.T, s *ossStore, c *ossConfig, key string) {
	t.Helper()
	date := time.Now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha1.New, []byte(c.SecretKey))
	mac.Write([]byte("DELETE\n\n\n" + date + "\n/" + c.Bucket + "/" + key))
	req, _ := http.NewRequest(http.MethodDelete, "https://"+c.host()+"/"+key, nil)
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "OSS "+c.AccessKey+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := s.hc.Do(req)
	if err != nil {
		t.Errorf("清理失败（对象仍在桶里 %s）: %v", key, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Errorf("清理返回 %d（对象仍在桶里 %s）: %s", resp.StatusCode, key, raw)
		return
	}
	t.Logf("cleaned: %s", key)
}

// TestOSSDeleteKey 手工清理用：删掉指定对象。
//
//	OSS_REAL=1 OSS_DSN='…' OSS_DELETE_KEY='aigc-life/image/…' go test ./pkg/storage -run OSSDeleteKey -v
func TestOSSDeleteKey(t *testing.T) {
	dsn, key := os.Getenv("OSS_DSN"), os.Getenv("OSS_DELETE_KEY")
	if os.Getenv("OSS_REAL") != "1" || dsn == "" || key == "" {
		t.Skip("需要 OSS_REAL=1、OSS_DSN 与 OSS_DELETE_KEY")
	}
	s := New(Conf{Mode: "oss", Prefix: "aigc-life"}, sqlx.NewMysql(dsn)).(*ossStore)
	cfg, err := s.config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deleteObject(t, s, cfg, key)
}
