package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc64"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 落桶接口（契约 /v1/mediasvc）。所有写操作的输出都只经过这里。
//
// 路径（path）是桶内路径：v1/<类别>/<yyyy>/<mm>/<对象键>[.<ext>]。路径由调用方通过索引解析（placeObject / objectPath），存储层只负责按路径读写。
// 实现：OSSStore（store_oss.go，生产）与 LocalStore（本地开发）。

var (
	errObjectNotFound     = errors.New("media: object not found")
	errInvalidObjectKey   = errors.New("media: invalid object key")
	errUserInputProtected = errors.New("media: object cannot be deleted (user input)")
)

// localStoreName 本地实现的默认存储名（只用于开发）。
const localStoreName = storePrivate

// StoredObject 落桶后的对象。
type StoredObject struct {
	Store  string
	Key    string // 桶内路径 v1/<类别>/<yyyy>/<mm>/<对象键>[.<ext>]
	Sha256 string
	Size   int64
	Mime   string
	RID    string
	Class  string
	// Version 写入后对象的版本标识（OSS 为 PUT 响应的 ETag，本地实现为大小+修改时间），与 objectVersioner 的结果可比。
	// 为空表示不知道（不做写穿透缓存）。
	Version string
}

// IndexEntry 索引对象的列举结果：对象键与最后修改时间。
type IndexEntry struct {
	ObjectKey string
	Modified  time.Time
}

// StoredURL 对象的访问地址。Kind: public | signed | none（没有可访问地址，如本地实现）。
type StoredURL struct {
	Kind      string
	Value     string
	ExpiresAt string
}

// ObjectStore 落桶接口。
type ObjectStore interface {
	// Put 把本地文件写到 store 的 path 下。同路径同内容重复写入是覆盖写，结果一致。Put 不改动 localPath。
	Put(ctx context.Context, store, path, localPath, mime string) (StoredObject, error)
	// Fetch 把 path 对应的对象下载到 dir 下，返回本地路径（只读，调用方不得修改或删除）。不存在返回 errObjectNotFound。
	Fetch(ctx context.Context, store, path, dir string) (string, error)
	// URL 返回对象的默认访问地址（私有存储签名、公有存储公开）。
	URL(ctx context.Context, obj StoredObject) (StoredURL, error)
	// ListIndexes 列举私有桶里的索引对象（按对象键与修改时间）。marker 为空从头开始；next 为空表示已到末尾。
	ListIndexes(ctx context.Context, marker string, limit int) ([]IndexEntry, string, error)
	// DeleteIndex 删除对象键对应的索引（私有桶）。索引只随对象一起物理删除，不经过用户可删除的路径校验；不存在视为成功。
	DeleteIndex(ctx context.Context, objectKey string) error
	// Head 返回对象的字节数。不存在返回 errObjectNotFound。
	Head(ctx context.Context, store, path string) (int64, error)
	// SignedURL 签发有效期为 ttl 的 GET 签名地址。
	SignedURL(ctx context.Context, store, path string, ttl time.Duration) (StoredURL, error)
	// SignedProcessURL 签发带图片处理样式（OSS named style，x-oss-process）的 GET 签名地址；style 为空等同 SignedURL。
	// 样式会进入签名。本地实现忽略样式，返回与 SignedURL 相同的结果。
	SignedProcessURL(ctx context.Context, store, path, style string, ttl time.Duration) (StoredURL, error)
	// SignedPutURL 签发有效期为 ttl 的 PUT 签名地址，供客户端直传。本地实现返回 Kind=none。
	SignedPutURL(ctx context.Context, store, path string, ttl time.Duration) (StoredURL, error)
	// PublicURL 返回公有地址。私有存储或本地实现返回 Kind=none。
	PublicURL(ctx context.Context, store, path string) (StoredURL, error)
	// Delete 物理删除对象（只由清理任务在宽限期过后调用）。不存在返回 false, nil；用户输入与历史路径返回 errUserInputProtected。
	Delete(ctx context.Context, store, path string) (bool, error)
	// DeleteRetentionExpired 物理删除保留期到期的用户输入（只由清理任务在宽限期过后、且标记原因为 retention 时调用）。
	// 只收 user 类别的日期路径，其它类别与历史路径返回 errUserInputProtected。不存在返回 false, nil。
	DeleteRetentionExpired(ctx context.Context, store, path string) (bool, error)
	// CreatePending 为 path 登记待删除标记（软删除，设计文档 §8.5）。标记是同桶的独立状态对象 pending-delete/<path>。
	// reason 记在标记上：保留期删除写 pendingReasonRetention，通用删除写 pendingReasonManual（空，不落原因）。
	// 已有标记时不覆盖（原因也不改）：返回已有的到期时间与 false；新建成功返回 deleteAfter 与 true。
	CreatePending(ctx context.Context, store, path string, deleteAfter time.Time, reason string) (time.Time, bool, error)
	// PendingDeadline 返回 path 的待删除到期时间；没有标记返回 false。
	PendingDeadline(ctx context.Context, store, path string) (time.Time, bool, error)
	// PendingMarker 返回 path 的待删除标记（到期时间与原因）；没有标记返回 false。清理物理删除前的复核用它。
	PendingMarker(ctx context.Context, store, path string) (PendingMark, bool, error)
	// ClearPending 撤销待删除标记。标记不存在也返回成功。
	ClearPending(ctx context.Context, store, path string) error
	// ListPending 按键的字典序列出 store 里的待删除标记：返回 marker 之后的至多 limit 条。
	// class 非空时只列该类别的标记（键前缀 pending-delete/v1/<class>/），为空列全部。
	// next 非空表示后面还有，下次以 next 作为 marker 继续；为空表示已列完。
	ListPending(ctx context.Context, store, class, marker string, limit int) (marks []PendingMark, next string, err error)
}

// pendingPrefix 待删除标记的键前缀。与数据对象的 v1/ 前缀不重叠。
const pendingPrefix = "pending-delete/"

// errPendingCorrupt 待删除标记的时间戳格式损坏。列举时单条跳过（记日志），不影响同页其它标记。
var errPendingCorrupt = errors.New("media: 待删除标记损坏")

// 待删除标记的原因。只有 retention 能让清理任务物理删除 user 类别的对象；其它原因（含空）一律不删用户输入。
const (
	pendingReasonManual    = ""
	pendingReasonRetention = "retention"
)

// PendingMark 一条待删除标记。Key 是标记在桶里的完整键，用作列举的 marker。Reason 为空表示通用删除（manual）。
type PendingMark struct {
	Store       string
	Path        string
	Key         string
	DeleteAfter time.Time
	Reason      string
}

// LocalStore 写到本地目录的实现，仅用于开发。目录结构：<dir>/<store>/<path>。
type LocalStore struct {
	dir string
}

// NewLocalStore dir 不存在时创建。
func NewLocalStore(dir string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{dir: dir}, nil
}

func (l *LocalStore) full(store, path string) (string, error) {
	if validStore(store) == false || classOfPath(path) == "" {
		return "", errInvalidObjectKey
	}
	return filepath.Join(l.dir, store, filepath.FromSlash(path)), nil
}

// Put 写入 <dir>/<store>/<path>。同内容已存在且大小一致时复用，不重写。
func (l *LocalStore) Put(ctx context.Context, store, path, localPath, mime string) (StoredObject, error) {
	target, err := l.full(store, path)
	if err != nil {
		return StoredObject{}, err
	}
	sum, size, err := hashFileKnown(ctx, localPath)
	if err != nil {
		return StoredObject{}, fmt.Errorf("media: 读取源文件: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return StoredObject{}, err
	}
	version := ""
	if st, err := os.Stat(target); err != nil || st.Size() != size {
		if err := copyAtomic(localPath, target); err != nil {
			return StoredObject{}, fmt.Errorf("media: 写入对象: %w", err)
		}
		// 只有真正写入时才给版本：复用已有文件时其内容未经比对，不能把本次的字节当成它的内容去写穿透
		if st, err := os.Stat(target); err == nil {
			version = localVersion(st)
		}
	}
	return StoredObject{Store: store, Key: path, Sha256: sum, Size: size, Mime: mime, Version: version}, nil
}

// Version 本地实现的版本：大小 + 修改时间（纳秒）。copyAtomic 每次都是新文件，改写后版本必然变化。
func (l *LocalStore) Version(_ context.Context, store, path string) (int64, string, error) {
	target, err := l.full(store, path)
	if err != nil {
		return 0, "", err
	}
	st, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, "", errObjectNotFound
		}
		return 0, "", err
	}
	return st.Size(), localVersion(st), nil
}

func localVersion(st fs.FileInfo) string {
	return fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixNano())
}

// Fetch 把对象复制到 dir 下返回路径（本地实现直接返回对象文件路径，只读）。
func (l *LocalStore) Fetch(_ context.Context, store, path, _ string) (string, error) {
	target, err := l.full(store, path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errObjectNotFound
		}
		return "", err
	}
	return target, nil
}

// URL 本地实现没有可访问地址。
func (l *LocalStore) URL(_ context.Context, _ StoredObject) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}

// Head 返回对象字节数。
func (l *LocalStore) Head(_ context.Context, store, path string) (int64, error) {
	target, err := l.full(store, path)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, errObjectNotFound
		}
		return 0, err
	}
	return st.Size(), nil
}

// SignedURL 本地实现没有可访问地址。
func (l *LocalStore) SignedURL(_ context.Context, _, _ string, _ time.Duration) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}

// errProcessUnsupported 本地存储不能做带处理参数的签名地址。
var errProcessUnsupported = errors.New("media: 本地存储不支持带图片处理参数的签名地址")

// SignedProcessURL 本地实现没有处理能力：要求带处理样式（免费档水印等）时返回错误，调用方按签不出处理
// （输出占位），绝不退化成干净地址；样式为空等同 SignedURL。
func (l *LocalStore) SignedProcessURL(ctx context.Context, store, path, style string, ttl time.Duration) (StoredURL, error) {
	if style != "" {
		return StoredURL{}, errProcessUnsupported
	}
	return l.SignedURL(ctx, store, path, ttl)
}

// SignedPutURL 本地实现没有直传地址。
// ListIndexes 本地实现：一次返回全部索引（开发用，不分页）。
func (l *LocalStore) ListIndexes(_ context.Context, _ string, _ int) ([]IndexEntry, string, error) {
	root := filepath.Join(l.dir, storePrivate, "idx")
	var out []IndexEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		key, ok := objectKeyOfIndexName(d.Name())
		if ok {
			out = append(out, IndexEntry{ObjectKey: key, Modified: info.ModTime()})
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	return out, "", err
}

func (l *LocalStore) DeleteIndex(_ context.Context, objectKey string) error {
	target, err := l.full(storePrivate, indexKeyOf(objectKey))
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *LocalStore) SignedPutURL(_ context.Context, _, _ string, _ time.Duration) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}

// PublicURL 本地实现没有公有地址。
func (l *LocalStore) PublicURL(_ context.Context, _, _ string) (StoredURL, error) {
	return StoredURL{Kind: "none"}, nil
}

// Delete 删除对象。用户输入（user/ 前缀）与历史路径（只读）拒绝删除。
func (l *LocalStore) Delete(_ context.Context, store, path string) (bool, error) {
	if !deletable(path) {
		return false, errUserInputProtected
	}
	return l.remove(store, path)
}

// DeleteRetentionExpired 删除保留期到期的用户输入；只收 user 类别。
func (l *LocalStore) DeleteRetentionExpired(_ context.Context, store, path string) (bool, error) {
	if classOfPath(path) != classUser {
		return false, errUserInputProtected
	}
	return l.remove(store, path)
}

// remove 物理删除本地对象；调用方已做类别校验。
func (l *LocalStore) remove(store, path string) (bool, error) {
	target, err := l.full(store, path)
	if err != nil {
		return false, err
	}
	err = os.Remove(target)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// deletable 只有 work、deliver、asset 类别可以删；user 与历史路径不可删（设计文档 §2）。
func deletable(path string) bool {
	switch classOfPath(path) {
	case classWork, classDeliver, classAsset:
		return true
	default:
		return false
	}
}

// copyAtomic 先写临时文件再 rename，中途失败不会留下半截对象。
func copyAtomic(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(to), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后是无害的空操作
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, to)
}

// knownDigest 某个本地文件已经算好的 sha256、CRC64 与字节数（字节上传在读请求体时算出，落桶时算出），
// 经 context 传给 storeWithMeta 与存储的 Put，同一个文件在一次写入里只整读一次。
// crc64 是 OSS 口径的 CRC-64/ECMA（十进制字符串，与 x-oss-hash-crc64ecma 同格式），没算过为空。
type knownDigest struct {
	path  string
	sum   string
	crc64 string
	size  int64
}

type knownDigestKey struct{}

// withKnownDigest 登记 path 的摘要（不带 CRC64）。只对这一个路径生效：别的文件（索引、其它产物）照常自己算。
func withKnownDigest(ctx context.Context, path, sum string, size int64) context.Context {
	return withKnownDigestCRC(ctx, path, sum, "", size)
}

// withKnownDigestCRC 同 withKnownDigest，另外登记 CRC64（空表示不知道：落桶写出的事实不带 CRC64，之后不走服务端复制）。
func withKnownDigestCRC(ctx context.Context, path, sum, crc string, size int64) context.Context {
	return context.WithValue(ctx, knownDigestKey{}, knownDigest{path: path, sum: sum, crc64: crc, size: size})
}

// fileDigest 一个本地文件的 sha256、CRC64（可能为空，见 knownDigest）与字节数。
type fileDigest struct {
	sum   string
	crc64 string
	size  int64
}

// hashFileKnown 优先用 context 里登记的摘要：路径一致且文件当前大小与登记的一致才用；否则整读计算（与 hashFile 相同）。
func hashFileKnown(ctx context.Context, path string) (string, int64, error) {
	d, err := digestFileKnown(ctx, path)
	return d.sum, d.size, err
}

// digestFileKnown 同 hashFileKnown，另外给出 CRC64：登记的摘要带不带 CRC64 原样给出；整读时与 sha256 同一遍算出。
func digestFileKnown(ctx context.Context, path string) (fileDigest, error) {
	if k, ok := ctx.Value(knownDigestKey{}).(knownDigest); ok && k.path == path && k.sum != "" {
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() && st.Size() == k.size {
			return fileDigest{sum: k.sum, crc64: k.crc64, size: k.size}, nil
		}
	}
	return digestFile(path)
}

func hashFile(path string) (string, int64, error) {
	d, err := digestFile(path)
	return d.sum, d.size, err
}

// digestFile 整读一遍文件，同时算 sha256 与 CRC64（CRC64 很便宜，省得服务端复制前再读一遍）。
func digestFile(path string) (fileDigest, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileDigest{}, err
	}
	defer f.Close()
	h := sha256.New()
	c := newCRC64()
	n, err := io.Copy(io.MultiWriter(h, c), f)
	if err != nil {
		return fileDigest{}, err
	}
	return fileDigest{sum: hex.EncodeToString(h.Sum(nil)), crc64: localCRC64(c), size: n}, nil
}

// ossCRC64Table OSS 的 CRC64：CRC-64/ECMA-182 多项式（0x42F0E1EBA9EA3693）的反射形式，初值与结果异或都是全 1，
// 即 CRC-64/XZ（"123456789" 的校验值 0x995DC9BBDF1939FA）；Go 的 hash/crc64 ECMA 表内部做了首尾取反，正是这一口径。
// 阿里云官方 Go SDK（aliyun-oss-go-sdk oss/utils.go crcTable()、alibabacloud-oss-go-sdk-v2 oss/utils_crc.go）
// 校验 x-oss-hash-crc64ecma 用的也是 crc64.New(crc64.MakeTable(crc64.ECMA))，其测试向量见 ossCRC64Vectors。
var ossCRC64Table = crc64.MakeTable(crc64.ECMA)

func newCRC64() hash.Hash64 { return crc64.New(ossCRC64Table) }

// formatCRC64 与 x-oss-hash-crc64ecma 相同的格式：无符号十进制。
func formatCRC64(v uint64) string { return strconv.FormatUint(v, 10) }

// ossCRC64Vectors CRC-64/XZ（= OSS crc64ecma）的已知向量：前两条取自阿里云 OSS Go SDK 的 crc_test.go
// （TestCRCCombine），"123456789" 也是 CRC 目录里 CRC-64/XZ 的 check 值。
var ossCRC64Vectors = []struct {
	in   string
	want uint64
}{
	{"123456789", 0x995DC9BBDF1939FA},
	{"This is a test of the emergency broadcast system.", 0x27DB187FC15BBC72},
	{"", 0},
}

// crc64Binding 事实与字节的 CRC64 绑定是否启用。进程启动时对实现做自检（selfTestCRC64），不过则关闭：
// 关闭后本地不再给出 CRC64（localCRC64 返回空），写出的事实不带 CRC64，服务端复制一律回退原路径（慢但正确），
// 写入也不再与 OSS 的 CRC64 核对。绑定只是复制的加速条件，任何情况下都不能让它挡住用户写入。
var crc64Binding = initCRC64Binding()

func initCRC64Binding() bool {
	if err := selfTestCRC64(newCRC64); err != nil {
		log.Printf("media ERROR check=media.crc64_selftest_failed: %v；关闭 CRC64 绑定，服务端复制回退原路径", err)
		return false
	}
	return true
}

// selfTestCRC64 用已知向量核对 CRC64 实现（分块写入与一次写入都要对）。
func selfTestCRC64(newHash func() hash.Hash64) error {
	for _, v := range ossCRC64Vectors {
		h := newHash()
		_, _ = io.WriteString(h, v.in)
		if got := h.Sum64(); got != v.want {
			return fmt.Errorf("CRC64(%q) = %#x，期望 %#x", v.in, got, v.want)
		}
		h = newHash()
		for i := 0; i < len(v.in); i++ {
			_, _ = h.Write([]byte{v.in[i]})
		}
		if got := h.Sum64(); got != v.want {
			return fmt.Errorf("CRC64(%q) 分块 = %#x，期望 %#x", v.in, got, v.want)
		}
	}
	return nil
}

// localCRC64 本地算出的 CRC64（十进制）；绑定关闭时返回空（按「不知道」处理，见 crc64Binding）。
func localCRC64(h hash.Hash64) string {
	if !crc64Binding {
		return ""
	}
	return formatCRC64(h.Sum64())
}

// crc64MismatchLog 本地与 OSS 的 CRC64 不一致时的 ERROR 日志限流：进程内第一次立即打，之后每 crc64MismatchEvery
// 最多打一次（带期间累计次数），不会刷屏。
var crc64MismatchLog struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

const crc64MismatchEvery = 10 * time.Minute

// logCRC64Mismatch 记一次 CRC64 不一致（限流）。返回这次是否真的打了日志（测试用）。
func logCRC64Mismatch(store, path, local, remote string) bool {
	l := &crc64MismatchLog
	l.mu.Lock()
	now := time.Now()
	if !l.last.IsZero() && now.Sub(l.last) < crc64MismatchEvery {
		l.suppressed++
		l.mu.Unlock()
		return false
	}
	suppressed := l.suppressed
	l.last, l.suppressed = now, 0
	l.mu.Unlock()
	log.Printf("media ERROR check=media.crc64_mismatch store=%s path=%s local_crc64=%s oss_crc64=%s suppressed_since_last=%d："+
		"写入照常成功、对象保留；对象上事实里的 CRC64 与 OSS 不符，服务端复制会回退原路径", store, path, local, remote, suppressed)
	return true
}

// extOf 只为常见类型加扩展名，其余不加。扩展名只影响本地文件名，不进入对象键。
func extOf(mime string) string {
	switch mime {
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "audio/mpeg":
		return ".mp3"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	}
	return ""
}

// pendingFile 本地待删除标记的文件：<dir>/<store>/pending-delete/<path>。
// 内容第一行是到期时间（RFC3339）；有原因时第二行是 reason=<原因>。只有一行的旧标记视为没有原因（manual）。
func (l *LocalStore) pendingFile(store, path string) (string, error) {
	if !validStore(store) || classOfPath(path) == "" {
		return "", errInvalidObjectKey
	}
	return filepath.Join(l.dir, store, filepath.FromSlash(pendingPrefix+path)), nil
}

// CreatePending 新建标记；文件已存在（O_EXCL）时读回已有的到期时间，不覆盖。
func (l *LocalStore) CreatePending(ctx context.Context, store, path string, deleteAfter time.Time, reason string) (time.Time, bool, error) {
	target, err := l.pendingFile(store, path)
	if err != nil {
		return time.Time{}, false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return time.Time{}, false, err
	}
	at := deleteAfter.UTC().Truncate(time.Second)
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		existing, ok, err := l.PendingDeadline(ctx, store, path)
		if err != nil {
			return time.Time{}, false, err
		}
		if !ok {
			return time.Time{}, false, errors.New("media: 待删除标记冲突但读取不到")
		}
		return existing, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	content := at.Format(time.RFC3339)
	if reason != pendingReasonManual {
		content += "\n" + localReasonPrefix + reason
	}
	_, werr := f.WriteString(content)
	cerr := f.Close()
	if werr != nil {
		return time.Time{}, false, werr
	}
	return at, true, cerr
}

// localReasonPrefix 本地标记文件里原因行的前缀。
const localReasonPrefix = "reason="

// PendingDeadline 读取本地待删除标记的到期时间。
func (l *LocalStore) PendingDeadline(ctx context.Context, store, path string) (time.Time, bool, error) {
	m, ok, err := l.PendingMarker(ctx, store, path)
	return m.DeleteAfter, ok, err
}

// PendingMarker 读取本地待删除标记：第一行到期时间，可选的第二行 reason=<原因>。
func (l *LocalStore) PendingMarker(_ context.Context, store, path string) (PendingMark, bool, error) {
	target, err := l.pendingFile(store, path)
	if err != nil {
		return PendingMark{}, false, err
	}
	raw, err := os.ReadFile(target)
	if errors.Is(err, fs.ErrNotExist) {
		return PendingMark{}, false, nil
	}
	if err != nil {
		return PendingMark{}, false, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(lines[0]))
	if err != nil {
		return PendingMark{}, false, fmt.Errorf("%w: %v", errPendingCorrupt, err)
	}
	reason := pendingReasonManual
	if len(lines) > 1 {
		line := strings.TrimSpace(lines[1])
		if !strings.HasPrefix(line, localReasonPrefix) {
			return PendingMark{}, false, fmt.Errorf("%w: 原因行格式错误", errPendingCorrupt)
		}
		reason = strings.TrimPrefix(line, localReasonPrefix)
	}
	return PendingMark{Store: store, Path: path, Key: pendingPrefix + path, DeleteAfter: at, Reason: reason}, true, nil
}

// ClearPending 删除本地待删除标记；不存在也成功。
func (l *LocalStore) ClearPending(_ context.Context, store, path string) error {
	target, err := l.pendingFile(store, path)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// pendingListPrefix 列举待删除标记的键前缀：class 为空时是全部标记，否则只取该类别的日期路径。
func pendingListPrefix(class string) string {
	if class == "" {
		return pendingPrefix
	}
	return pendingPrefix + "v1/" + class + "/"
}

// ListPending 本地列举：遍历 <dir>/<store>/pending-delete，按键排序后分页。
func (l *LocalStore) ListPending(ctx context.Context, store, class, marker string, limit int) ([]PendingMark, string, error) {
	if !validStore(store) {
		return nil, "", errInvalidObjectKey
	}
	root := filepath.Join(l.dir, store, "pending-delete")
	var keys []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		keys = append(keys, pendingPrefix+filepath.ToSlash(rel))
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	sort.Strings(keys)
	prefix := pendingListPrefix(class)
	var marks []PendingMark
	for _, k := range keys {
		if k <= marker || !strings.HasPrefix(k, prefix) {
			continue
		}
		if len(marks) == limit {
			return marks, marks[len(marks)-1].Key, nil
		}
		path := strings.TrimPrefix(k, pendingPrefix)
		m, ok, err := l.PendingMarker(ctx, store, path)
		if errors.Is(err, errPendingCorrupt) {
			log.Printf("media: 跳过损坏的待删除标记 store=%s path=%s: %v", store, path, err)
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if !ok {
			continue
		}
		m.Key = k
		marks = append(marks, m)
	}
	return marks, "", nil
}
