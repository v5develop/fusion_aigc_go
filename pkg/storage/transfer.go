package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// Transfer 把 AIGC 执行面返回的产物转存到公有桶，返回新的公网地址。
//
// 执行面的产物落在现有系统的私有桶（afv_system_config.storage_private_*），
// 既不能长期保留也不对外可读——现有系统同样要转存（见 afv_creation_task.is_transferred）。
// 私有桶的对象用签名 GET 下载，其余地址直接下载；已经在我们自己目录（{prefix}/）下的原样返回。
// 公有桶里的其他目录也要复制：比如 74 返回的参考图在 aigc_temp/，那是执行面的临时目录，会被清理。
func (s *ossStore) Transfer(ctx context.Context, srcURL, kind string, userID int64) (string, error) {
	pub, err := s.config(ctx)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(srcURL, pub.publicBase()+"/"+s.prefix+"/") {
		return srcURL, nil // 已经在我们自己的目录里
	}
	key := ObjectKey(s.prefix, kind, userID, ext(srcURL))

	// 公有桶里的临时目录：同桶服务端复制
	if strings.HasPrefix(srcURL, pub.publicBase()+"/") {
		if url, err := s.copyObject(ctx, pub, pub.Bucket, objectKeyOf(stripQuery(srcURL)), key); err == nil {
			return url, nil
		} else {
			logx.WithContext(ctx).Errorf("storage: 同桶复制失败，改为下载上传: %v", err)
		}
	}

	// 同地域优先用 OSS 服务端复制（CopyObject），不经过我们，快很多；
	// 跨地域 OSS 不支持复制，只能下载再上传（当前私有桶在上海、公有桶在杭州，走的是这条）。
	if priv, perr := s.privateConfig(ctx); perr == nil && priv.matches(srcURL) && regionOf(priv) == regionOf(pub) {
		if url, err := s.copyObject(ctx, pub, priv.Bucket, objectKeyOf(srcURL), key); err == nil {
			return url, nil
		} else {
			logx.WithContext(ctx).Errorf("storage: 服务端复制失败，改为下载上传: %v", err)
		}
	}
	data, contentType, err := s.fetch(ctx, srcURL)
	if err != nil {
		return "", err
	}
	return s.Put(ctx, key, contentType, data)
}

// copyObject OSS 服务端复制，要求源桶与目标桶同地域。
func (s *ossStore) copyObject(ctx context.Context, dst *ossConfig, srcBucket, srcKey, dstKey string) (string, error) {
	source := "/" + srcBucket + "/" + srcKey
	date := time.Now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha1.New, []byte(dst.SecretKey))
	mac.Write([]byte("PUT\n\n\n" + date + "\nx-oss-copy-source:" + source + "\n/" + dst.Bucket + "/" + dstKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+dst.host()+"/"+dstKey, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Date", date)
	req.Header.Set("x-oss-copy-source", source)
	req.Header.Set("Authorization", "OSS "+dst.AccessKey+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	resp, err := s.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("复制返回 %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return dst.publicBase() + "/" + dstKey, nil
}

// regionOf 桶所在地域：优先配置里的 region，否则从 endpoint 域名里取（oss-cn-hangzhou.aliyuncs.com → cn-hangzhou）。
func regionOf(c *ossConfig) string {
	if c.Region != "" {
		return c.Region
	}
	h := c.host()
	if i := strings.Index(h, "oss-"); i >= 0 {
		if j := strings.Index(h[i+4:], "."); j > 0 {
			return h[i+4 : i+4+j]
		}
	}
	return ""
}

func (s *localStore) Transfer(_ context.Context, srcURL, _ string, _ int64) (string, error) {
	return srcURL, nil // 开发环境不转存
}

// fetch 下载产物。
//
// 执行面返回的私有桶地址本身就是带 Expires/Signature 的临时链接，而且可能已经过期；
// 命中私有桶时丢掉原签名参数，用我们自己的密钥重新签（两种签名不能同时用，OSS 会报 400）。
func (s *ossStore) fetch(ctx context.Context, srcURL string) ([]byte, string, error) {
	get := srcURL
	priv, perr := s.privateConfig(ctx)
	signed := perr == nil && priv.matches(srcURL)
	if signed {
		get = stripQuery(srcURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, get, nil)
	if err != nil {
		return nil, "", err
	}
	if signed {
		signGet(req, priv, objectKeyOf(srcURL))
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("storage: 下载产物失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("storage: 下载产物返回 %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 500<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

// privateConfig 私有桶配置（afv_system_config.storage_private_*），缓存 5 分钟。
func (s *ossStore) privateConfig(ctx context.Context) (*ossConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv != nil && time.Since(s.privLoaded) < 5*time.Minute {
		return s.priv, nil
	}
	c, err := loadOSSConfig(ctx, s.conn, "storage_private_")
	if err != nil {
		return nil, err
	}
	s.priv, s.privLoaded = c, time.Now()
	return c, nil
}

// ObjectKey 反解对象 key。自定义域名和 <bucket>.<endpoint> 两种写法都认。
func (s *ossStore) ObjectKey(ctx context.Context, rawURL string) (string, error) {
	c, err := s.config(ctx)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(stripQuery(rawURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("storage: 地址不合法: %q", rawURL)
	}
	base, err := url.Parse(c.publicBase())
	if err != nil {
		return "", err
	}
	if u.Host != c.host() && u.Host != base.Host {
		return "", fmt.Errorf("storage: %q 不在本服务的桶里", u.Host)
	}
	key := objectKeyOf(u.String())
	if key == "" {
		return "", fmt.Errorf("storage: 地址里没有对象路径: %q", rawURL)
	}
	return key, nil
}

// ObjectKey local 模式没有桶，按 baseURL 截出相对路径，够 mock 用。
func (s *localStore) ObjectKey(_ context.Context, rawURL string) (string, error) {
	if !s.Owns(rawURL) {
		return "", fmt.Errorf("storage: %q 不是本地存储签发的", rawURL)
	}
	return strings.TrimPrefix(stripQuery(rawURL), s.baseURL+"/"), nil
}

// matches 判断地址是否属于这个桶。
func (c *ossConfig) matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Host == c.host()
}

// objectKeyOf 从地址里取出对象 key。
func objectKeyOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// signGet 给私有桶的 GET 请求加 V1 签名。
func signGet(req *http.Request, c *ossConfig, key string) {
	date := time.Now().UTC().Format(http.TimeFormat)
	mac := hmac.New(sha1.New, []byte(c.SecretKey))
	mac.Write([]byte("GET\n\n\n" + date + "\n/" + c.Bucket + "/" + key))
	req.Header.Set("Date", date)
	req.Header.Set("Authorization", "OSS "+c.AccessKey+":"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// stripQuery 去掉地址上的查询参数（原有的临时签名）。
func stripQuery(rawURL string) string {
	if i := strings.IndexByte(rawURL, '?'); i > 0 {
		return rawURL[:i]
	}
	return rawURL
}

// ext 取地址里的扩展名，没有就按 bin。
func ext(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "bin"
	}
	if e := strings.TrimPrefix(path.Ext(u.Path), "."); e != "" {
		return e
	}
	return "bin"
}
