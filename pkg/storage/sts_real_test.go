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

// TestSTSRealCredentials 连真实账号试一次 AssumeRole（默认跳过）：
//
//	OSS_REAL=1 OSS_DSN='…' go test ./pkg/storage -run STSReal -v
func TestSTSRealCredentials(t *testing.T) {
	dsn := os.Getenv("OSS_DSN")
	if os.Getenv("OSS_REAL") != "1" || dsn == "" {
		t.Skip("设置 OSS_REAL=1 与 OSS_DSN 后才会连真实账号")
	}
	s := New(Conf{Mode: "oss", Prefix: "aigc-local-life"}, sqlx.NewMysql(dsn))
	cred, err := s.Credentials(context.Background(), "video", 123456, 30*time.Minute)
	if err != nil {
		t.Fatalf("AssumeRole 失败: %v", err)
	}
	t.Logf("bucket=%s host=%s dir=%s base=%s expire=%s", cred.Bucket, cred.Host, cred.Dir, cred.BaseURL, cred.Expiration)
	if cred.AccessKeyID == "" || cred.SecurityToken == "" {
		t.Fatalf("凭证不完整: %+v", cred)
	}
	if got := cred.Dir; got[:10] != "aigc-local-life/" {
		t.Fatalf("目录前缀不对: %s", got)
	}
}

// TestSTSRealUpload 用临时凭证真实上传：目录内可写、目录外被拒、公网可访问，最后清理。
func TestSTSRealUpload(t *testing.T) {
	dsn := os.Getenv("OSS_DSN")
	if os.Getenv("OSS_REAL") != "1" || dsn == "" {
		t.Skip("设置 OSS_REAL=1 与 OSS_DSN 后才会连真实账号")
	}
	conn := sqlx.NewMysql(dsn)
	s := New(Conf{Mode: "oss", Prefix: "aigc-local-life"}, conn).(*ossStore)
	ctx := context.Background()
	cred, err := s.Credentials(ctx, "video", 123456, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("sts direct upload " + time.Now().Format(time.RFC3339))

	key := cred.Dir + "selftest.txt"
	if code, msg := stsPut(cred, key, body); code != 200 {
		t.Fatalf("目录内应可写，得到 %d: %s", code, msg)
	}
	t.Logf("uploaded: %s/%s", cred.BaseURL, key)
	cfg, _ := s.config(ctx)
	defer deleteObject(t, s, cfg, key)

	if size, ct, err := s.Stat(ctx, cred.BaseURL+"/"+key); err != nil || size != int64(len(body)) {
		t.Fatalf("Stat: size=%d ct=%s err=%v", size, ct, err)
	}
	if code, _ := stsPut(cred, "aigc-local-life/other/999/evil.txt", body); code != 403 {
		t.Errorf("目录外应被拒，得到 %d", code)
	}
}

// stsPut 用临时凭证 PUT 一个对象（OSS V1 签名 + x-oss-security-token）。
func stsPut(c *Credential, key string, body []byte) (int, string) {
	date := time.Now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha1.New, []byte(c.AccessKeySecret))
	mac.Write([]byte("PUT\n\ntext/plain\n" + date + "\nx-oss-security-token:" + c.SecurityToken + "\n/" + c.Bucket + "/" + key))
	req, _ := http.NewRequest(http.MethodPut, "https://"+c.Host+"/"+key, bytes.NewReader(body))
	req.Header.Set("Date", date)
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-oss-security-token", c.SecurityToken)
	req.Header.Set("Authorization", "OSS "+c.AccessKeyID+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return resp.StatusCode, string(raw)
}
