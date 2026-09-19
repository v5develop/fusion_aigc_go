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
	Prefix string `json:",default=aigc-life"`
	// LocalDir / LocalBaseUrl 仅 local 模式使用
	LocalDir     string `json:",default=data/files"`
	LocalBaseUrl string `json:",default=http://127.0.0.1:8888/files"`
}

type Store interface {
	// Put 写入对象，返回公网可访问地址。
	Put(ctx context.Context, key, contentType string, data []byte) (string, error)
	// Owns 判断地址是否是本存储签发的（校验客户端传回的上传地址用）。
	Owns(url string) bool
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
