package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 对象路径（方案 B）：v1/<类别>/<yyyy>/<mm>/<对象键>[.<后缀>]
//   - 日期取首次写入时间（上海时区），后缀由内容的实际类型映射（不取客户端声明）。
//   - rid → 路径靠私有桶里的索引对象 idx/<派生键>.json，内容为 {"path": "..."}。索引不可变。
//   - 没有索引的 rid 即对象不存在（不做旧路径回退，早期测试环境的对象不保留）。
//
// 写入顺序：先写索引再写对象。重试时索引已存在则沿用原路径，保证同一个 rid 只有一个对象。

var shanghaiZone = time.FixedZone("CST", 8*3600)

// classIndex 索引对象的类别（只用于存储层的路径校验，不对外）。
const classIndex = "index"

// objectIndex 索引对象的内容。
type objectIndex struct {
	Path string `json:"path"`
}

// extByMime 媒体类型到规范后缀。未列出的类型不带后缀（路径仍然合法）。
var extByMime = map[string]string{
	"image/png":       "png",
	"image/jpeg":      "jpg",
	"image/webp":      "webp",
	"image/gif":       "gif",
	"image/svg+xml":   "svg",
	"video/mp4":       "mp4",
	"video/quicktime": "mov",
	"video/webm":      "webm",
	"audio/mpeg":      "mp3",
	"audio/mp4":       "m4a",
	"audio/wav":       "wav",
	"audio/x-wav":     "wav",
	"font/ttf":        "ttf",
	"font/otf":        "otf",
	"font/woff2":      "woff2",
}

// extForMime 由内容的媒体类型取规范后缀；未知类型返回空串。
func extForMime(mime string) string {
	return extByMime[strings.ToLower(strings.TrimSpace(mime))]
}

// datedPath 新路径：v1/<类别>/<yyyy>/<mm>/<对象键>[.<后缀>]。
func datedPath(class string, at time.Time, objectKey, ext string) string {
	t := at.In(shanghaiZone)
	p := fmt.Sprintf("v1/%s/%04d/%02d/%s", class, t.Year(), int(t.Month()), objectKey)
	if ext != "" {
		p += "." + ext
	}
	return p
}

// maxCachedPaths 路径缓存上限；超过即整体清空。
const maxCachedPaths = 50000

// pathCacheTTL 缓存的有效期。删除只清理存储上的索引，其它实例的缓存要靠过期收敛（否则重新入库后可能长时间返回旧路径）。
const pathCacheTTL = time.Minute

// pathCache 已解析的 rid → 路径（只缓存有索引的结果，带过期时间）。
type pathCache struct {
	mu sync.Mutex
	m  map[string]cachedPath
}

type cachedPath struct {
	path string
	at   time.Time
}

func newPathCache() *pathCache {
	return &pathCache{m: make(map[string]cachedPath)}
}

func (c *pathCache) get(rid string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[rid]
	if !ok || time.Since(e.at) > pathCacheTTL {
		delete(c.m, rid)
		return "", false
	}
	return e.path, true
}

func (c *pathCache) put(rid, path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= maxCachedPaths {
		c.m = make(map[string]cachedPath)
	}
	c.m[rid] = cachedPath{path: path, at: time.Now()}
}

// objectKeyOfPath 路径 → 对象键（路径最后一段，去掉后缀）。非对象键返回 false。
func objectKeyOfPath(path string) (string, bool) {
	base := path[strings.LastIndex(path, "/")+1:]
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base, validObjectKey(base)
}

// readIndex 读取 rid 的索引。found=false 表示没有索引（历史对象）。
func (s *Server) readIndex(ctx context.Context, rid, class string) (path string, found bool, err error) {
	dir, err := os.MkdirTemp("", "media-idx-")
	if err != nil {
		return "", false, internal(err)
	}
	defer os.RemoveAll(dir)
	local, err := s.objects.Fetch(ctx, storePrivate, s.id.IndexKey(rid), dir)
	switch {
	case err == nil:
		raw, rerr := os.ReadFile(local)
		if rerr != nil {
			return "", false, internal(rerr)
		}
		var idx objectIndex
		if jerr := json.Unmarshal(raw, &idx); jerr != nil || classOfPath(idx.Path) != class {
			return "", false, internal(fmt.Errorf("object index corrupt rid=%s", rid))
		}
		return idx.Path, true, nil
	case errors.Is(err, errObjectNotFound):
		return "", false, nil
	default:
		return "", false, unavailable("对象索引暂不可读，请稍后重试")
	}
}

// writeIndex 写入索引对象（私有桶，内容不可变）。
func (s *Server) writeIndex(ctx context.Context, rid, path string) error {
	dir, err := os.MkdirTemp("", "media-idx-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	raw, err := json.Marshal(objectIndex{Path: path})
	if err != nil {
		return err
	}
	local := filepath.Join(dir, "index.json")
	if err := os.WriteFile(local, raw, 0o600); err != nil {
		return err
	}
	_, err = s.objects.Put(ctx, storePrivate, s.id.IndexKey(rid), local, "application/json")
	return err
}

// ridNotFound 按 rid 找不到对象（没有索引）对外统一为 404 not_found。
func ridNotFound(err error) error {
	if errors.Is(err, errObjectNotFound) {
		return newAPIError(codeNotFound, "object.not_found")
	}
	return err
}

// objectPath 由 rid 解析存储与路径（读取侧与删除、复制、定位共用）。没有索引即对象不存在（errObjectNotFound）。
func (s *Server) objectPath(ctx context.Context, rid string) (store, path string, err error) {
	_, class, perr := ParseRID(rid)
	if perr != nil {
		return "", "", invalid("request.rid")
	}
	store = storeForClass(class, "")
	if p, ok := s.paths.get(rid); ok {
		return store, p, nil
	}
	p, found, err := s.readIndex(ctx, rid, class)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", errObjectNotFound
	}
	s.paths.put(rid, p)
	return store, p, nil
}

// placeObject 写入前决定对象路径：已有索引（同一幂等键的重试）沿用原路径；否则生成新路径并先写索引。
func (s *Server) placeObject(ctx context.Context, rid, class, store, key, mime string, now time.Time) (string, error) {
	if p, found, err := s.readIndex(ctx, rid, class); err != nil {
		return "", err
	} else if found {
		s.paths.put(rid, p)
		return p, nil
	}
	p := datedPath(class, now, key, extForMime(mime))
	if err := s.writeIndex(ctx, rid, p); err != nil {
		return "", unavailable("对象索引写入失败，请稍后重试")
	}
	s.paths.put(rid, p)
	return p, nil
}
