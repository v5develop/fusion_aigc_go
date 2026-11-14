// Package storage 文件存储：用户上传的照片、转存的配音等。
//
//   - local：写本机目录，由网关 /files 提供访问。仅开发环境使用，AIGC 执行面访问不到。
//   - oss：写现有公有桶，连接配置从 afv_system_config 的 storage_public_* 读取（与现有系统同一个桶），
//     不写在配置文件里。
package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type Conf struct {
	Mode string `json:",default=local,options=local|oss"`
	// Prefix 对象路径前缀，所有文件都在它下面
	Prefix string `json:",default=aigc-local-life"`
	// LocalDir / LocalBaseUrl 仅 local 模式使用
	LocalDir     string `json:",default=data/files"`
	LocalBaseUrl string `json:",default=http://127.0.0.1:8888/files"`
}

type Store interface {
	// Put 写入对象，返回公网可访问地址。开发环境网关代传用；线上前端直传。
	Put(ctx context.Context, key, contentType string, data []byte) (string, error)
	// Owns 判断地址是否是本存储签发的（校验客户端传回的上传地址用）。
	Owns(url string) bool
	// Transfer 把执行面产物（私有桶、会过期）转存到公有桶，返回新地址。local 模式原样返回。
	Transfer(ctx context.Context, srcURL, kind string, userID int64) (string, error)
	// ObjectKey 从公网地址反解出桶内的对象 key。
	// 执行面跑本地模型（lite）时不下载 URL，要求直接给 OSS key，所以要这一步。
	// 地址不在本服务用的桶里时返回错误——那种情况得先 Transfer 过来。
	ObjectKey(ctx context.Context, rawURL string) (string, error)
	// Delete 删除桶内对象（幂等：对象不存在也返回 nil）。key 必须在本存储的 Prefix 之下，否则拒绝——
	// 这是存储层的最后一道边界；调用方（如 plaza 孤儿清理）还要自己再限定到具体业务目录。
	Delete(ctx context.Context, key string) error
	// Uploader 客户端直传：签发临时凭证、校验传上来的对象。
	Uploader
}

// New 按模式创建。oss 模式需要 conn 读取 afv_system_config。
func New(c Conf, conn sqlx.SqlConn) Store {
	if c.Mode == "oss" {
		return &ossStore{prefix: c.Prefix, conn: conn}
	}
	return &localStore{dir: c.LocalDir, baseURL: strings.TrimRight(c.LocalBaseUrl, "/"), prefix: c.Prefix}
}

// ObjectKey 生成对象路径：<prefix>/<kind>/<userID>/<yyyymmdd>/<随机>.<ext>
func ObjectKey(prefix, kind string, userID int64, ext string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s/%s/%d/%s/%s.%s", prefix, kind, userID, time.Now().Format("20060102"), hex.EncodeToString(b), ext)
}

type localStore struct {
	dir, baseURL, prefix string
}

func (s *localStore) Put(_ context.Context, key, _ string, data []byte) (string, error) {
	p := filepath.Join(s.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	return s.baseURL + "/" + key, nil
}

func (s *localStore) Owns(url string) bool {
	return strings.HasPrefix(url, s.baseURL+"/"+s.prefix+"/")
}

// checkDeleteKey 删除前的 key 边界：非空、相对路径、无 ..、无查询串、且在 prefix/ 之下。
func checkDeleteKey(prefix, key string) error {
	p := strings.Trim(prefix, "/")
	switch {
	case key == "" || p == "":
		return fmt.Errorf("storage: 拒绝删除 判据=key 或 prefix 为空")
	case strings.HasPrefix(key, "/") || strings.Contains(key, "..") || strings.ContainsAny(key, "?#\\") || strings.Contains(key, "//"):
		return fmt.Errorf("storage: 拒绝删除 判据=key 不规范 key=%q", key)
	case !strings.HasPrefix(key, p+"/") || len(key) == len(p)+1:
		return fmt.Errorf("storage: 拒绝删除 判据=key 不在 %s/ 之下 key=%q", p, key)
	}
	return nil
}

// Delete 删本机文件；文件不存在视为成功。
func (s *localStore) Delete(_ context.Context, key string) error {
	if err := checkDeleteKey(s.prefix, key); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, filepath.FromSlash(key))); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
