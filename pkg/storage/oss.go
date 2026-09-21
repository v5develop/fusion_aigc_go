package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ossConfig 现有公有桶配置（afv_system_config.storage_public_*）。
type ossConfig struct {
	Endpoint     string // https://oss-cn-hangzhou.aliyuncs.com
	Bucket       string
	AccessKey    string
	SecretKey    string
	CustomDomain string
	Region       string
	// StsRoleArn / StsRegion 客户端直传用：AssumeRole 换临时凭证
	StsRoleArn string
	StsRegion  string
}

// host 桶的访问域名：<bucket>.<endpoint host>
func (c ossConfig) host() string {
	u, err := url.Parse(c.Endpoint)
	h := c.Endpoint
	if err == nil && u.Host != "" {
		h = u.Host
	}
	return c.Bucket + "." + h
}

// publicBase 对外地址前缀：优先自定义域名。
func (c ossConfig) publicBase() string {
	if c.CustomDomain != "" {
		d := strings.TrimRight(c.CustomDomain, "/")
		if !strings.HasPrefix(d, "http") {
			d = "https://" + d
		}
		return d
	}
	return "https://" + c.host()
}

type ossStore struct {
	prefix string
	conn   sqlx.SqlConn
	hc     http.Client

	mu         sync.Mutex
	cfg        *ossConfig
	loaded     time.Time
	priv       *ossConfig // 私有桶（转存产物时读）
	privLoaded time.Time
}

// config 读库并缓存 5 分钟，运维改了配置不用重启。
func (s *ossStore) config(ctx context.Context) (*ossConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg != nil && time.Since(s.loaded) < 5*time.Minute {
		return s.cfg, nil
	}
	c, err := loadOSSConfig(ctx, s.conn, "storage_public_")
	if err != nil {
		return nil, err
	}
	s.cfg, s.loaded = c, time.Now()
	return c, nil
}

// loadOSSConfig 从 afv_system_config 读一组桶配置（前缀 storage_public_ / storage_private_）。
func loadOSSConfig(ctx context.Context, conn sqlx.SqlConn, prefix string) (*ossConfig, error) {
	var rows []struct {
		Key   string `db:"config_key"`
		Value string `db:"config_value"`
	}
	q := "SELECT config_key, COALESCE(config_value, '') AS config_value FROM afv_system_config " +
		"WHERE config_key LIKE ? AND deleted = 0"
	if err := conn.QueryRowsCtx(ctx, &rows, q, strings.ReplaceAll(prefix, "_", "\\_")+"%"); err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, r := range rows {
		kv[strings.TrimPrefix(r.Key, prefix)] = strings.TrimSpace(r.Value)
	}
	c := &ossConfig{
		Endpoint: kv["endpoint"], Bucket: kv["bucket"], AccessKey: kv["access_key"], SecretKey: kv["secret_key"],
		CustomDomain: kv["custom_domain"], Region: kv["region"],
		StsRoleArn: kv["sts_role_arn"], StsRegion: kv["sts_region"],
	}
	if c.Endpoint == "" || c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
		return nil, fmt.Errorf("storage: afv_system_config 缺少 %s* 配置", prefix)
	}
	return c, nil
}

// Put 用 OSS V1 签名直接 PUT 对象。
func (s *ossStore) Put(ctx context.Context, key, contentType string, data []byte) (string, error) {
	c, err := s.config(ctx)
	if err != nil {
		return "", err
	}
	date := time.Now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha1.New, []byte(c.SecretKey))
	mac.Write([]byte("PUT\n\n" + contentType + "\n" + date + "\n/" + c.Bucket + "/" + key))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+c.host()+"/"+key, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Date", date)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "OSS "+c.AccessKey+":"+sign)
	req.ContentLength = int64(len(data))
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("storage: 上传失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("storage: 上传返回 %d: %s", resp.StatusCode, raw)
	}
	return c.publicBase() + "/" + key, nil
}

func (s *ossStore) Owns(u string) bool {
	c, err := s.config(context.Background())
	if err != nil {
		return false
	}
	return strings.HasPrefix(u, c.publicBase()+"/"+s.prefix+"/")
}
