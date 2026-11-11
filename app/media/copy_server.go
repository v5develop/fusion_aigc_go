package media

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"
)

// 服务端复制（resources/copy 的快路径）。
//
// 原来的复制是「把源字节读到本机（copy_read）→ 再上传到目标桶（put）」：出口带宽低的环境（实测 ~125 KB/s）
// 一张大图、一段视频要几十秒到几分钟，还要付两次公网/跨网流量。源与目标都在同一个对象存储（同地域）时，
// 让存储自己在服务端复制（OSS CopyObject；> 阈值用 UploadPartCopy 分片复制），本机只发几次控制请求。
//
// 复制结果必须与原路径逐字段一致（sha256、size、mime、时长、宽高、帧率、音轨、色彩），而这些值原来是读字节时
// 算出来的（hash + probe）。媒体服务没有资源表，所以落桶时把这份字节的事实（storedFacts）写进对象自己的元数据
// （OSS：x-oss-meta-media-facts）；复制时 HEAD 源对象读回。元数据跟着每次 PUT 整体替换，只描述这次写入的字节；
// 事实里带这份字节的 CRC64，读回时与 OSS 对当前字节给出的 x-oss-hash-crc64ecma 核对，不会张冠李戴（见 storedFacts）。
// 没有事实或事实不带 CRC64 的源（本功能 / CRC64 绑定上线前写入的对象、客户端直传的对象）回退原路径。
//
// rid 派生、索引、待删除标记的处理与原路径完全相同（同一个幂等键得到同一个 rid、同一个路径）：
// 服务端复制失败（权限 / 不支持）时回退原路径，原路径沿用刚写的索引，结果不变。

// serverCopier 可选的存储能力：在存储服务端复制对象，字节不经过本机。
type serverCopier interface {
	// ServerCopyEligible src → dst 能否服务端复制；不能时返回原因（日志用，如 disabled、cross_region），能返回 ""。
	ServerCopyEligible(srcStore, dstStore string) string
	// ObjectFacts HEAD 源对象：字节数、版本、内容类型与落桶时登记的事实（没有为 nil）。不存在返回 errObjectNotFound。
	ObjectFacts(ctx context.Context, store, path string) (objectHead, error)
	// ServerCopy 把 srcStore/srcPath 复制到 dstStore/dstPath（覆盖写，与 Put 同语义），保留内容类型与对象元数据。
	// mode 为 single 或 multipart。不能复制（权限不足、存储不支持、跨地域）返回 *serverCopyRefused，调用方回退下载 + 上传。
	ServerCopy(ctx context.Context, srcStore, srcPath, dstStore, dstPath string, src objectHead) (obj StoredObject, mode string, err error)
}

// objectHead 一次 HEAD 得到的源对象信息。
type objectHead struct {
	Size         int64
	Version      string            // 归一化的 ETag
	ETag         string            // 原样的 ETag：复制请求带 x-oss-copy-source-if-match，只复制 HEAD 到的这一版
	ContentType  string            // 源对象的 Content-Type
	StorageClass string            // x-oss-storage-class（低频 / 归档不做服务端复制）
	CRC64        string            // x-oss-hash-crc64ecma：OSS 对当前字节算的 CRC64（十进制）；存储不给（非 OSS 的 S3 兼容存储等）为空
	UserMeta     map[string]string // 全部 x-oss-meta-*（小写键）：分片复制不自动带元数据，要显式写到目标
	Facts        *storedFacts      // 验签通过的事实（由 Server.trustedFacts 填，存储层不填）
}

// serverCopyRefused 服务端复制做不了（不是暂时故障）：调用方回退下载 + 上传。
type serverCopyRefused struct {
	reason   string
	err      error
	remember bool // 这对桶整体做不了（权限、不支持）：一段时间内不再尝试；只与这一个对象有关的（409、412）不记
}

func (e *serverCopyRefused) Error() string {
	if e.err == nil {
		return "media: 服务端复制不可用: " + e.reason
	}
	return fmt.Sprintf("media: 服务端复制不可用: %s: %v", e.reason, e.err)
}

func (e *serverCopyRefused) Unwrap() error { return e.err }

func refusedCopy(reason string, err error) error {
	return &serverCopyRefused{reason: reason, err: err, remember: true}
}

func refusedOnce(reason string, err error) error { return &serverCopyRefused{reason: reason, err: err} }

// storedFacts 落桶时对这份字节已知的事实，写在对象元数据里。V 是格式版本，不认识的版本视为没有。
//
// CRC64 是这份字节的 CRC-64/ECMA（OSS 口径，十进制，与 x-oss-hash-crc64ecma 同格式），在签名覆盖范围内。
// 它是后加的可选字段（omitempty，V 不变）：之前写入的事实没有这个键，payload 字节没变，原签名照样验得过——
// 只是这样的事实不绑定字节，trustFacts 不拿它做服务端复制（回退原路径）。
type storedFacts struct {
	V      int       `json:"v"`
	Sha256 string    `json:"sha256"`
	Size   int64     `json:"size"`
	CRC64  string    `json:"crc64,omitempty"`
	Meta   MediaMeta `json:"meta"`
}

const storedFactsVersion = 1

// ossFactsHeader 事实所在的对象元数据头。值是「JSON 的 base64url . HMAC 的 base64url」（无填充）：只含 ASCII，不受 HTTP 头编码影响。
const ossFactsHeader = "x-oss-meta-media-facts"

// factsMACLabel 事实签名的派生用途（identity.mac 的 label），与 rid / 对象键的派生互不相关。
const factsMACLabel = "facts"

// 事实为什么要签名：对象元数据谁能写对象谁就能写。除本服务外，持有桶写权限的还有 Java（含签发给客户端的
// STS 直传凭证）、运维工具等；公有桶的对象还能被匿名 HEAD 看到元数据。不签名的话，任何能往桶里写对象的一方
// 都可以给一份字节配上伪造的 sha256 / 时长 / 宽高，服务端复制会原样当真。签名密钥用 IdentitySecret
// （只有本服务持有，rid 与对象键也由它派生），没有密钥的写入方伪造不了；验签失败一律按「没有事实」回退原路径
// （读字节、算 sha256、probe），结果仍然正确，只是慢。
//
// 事实与字节的绑定：签名覆盖事实里的 CRC64（落桶时与 sha256 同一遍对本地字节算出，字节上传则在读请求体时算出，
// 不额外整读；Put 后还与 OSS 响应的 x-oss-hash-crc64ecma 核对，不一致只限流打 ERROR、写入照常成功——
// 此时事实里的 CRC64 与对象当前字节不符，下面的核对必然不过，等同于不带 CRC64；进程启动时 CRC64 实现自检不过
// 则整体关闭绑定，事实不带 CRC64，见 store.go 的 crc64Binding）。复制前 HEAD 源对象，
// OSS 对当前字节给出 x-oss-hash-crc64ecma，与事实里的 CRC64 相等才把事实当真；复制请求带
// x-oss-copy-source-if-match（只复制 HEAD 到的那一版），复制完成后再核对目标的 CRC64 与源一致。
// 于是「把一份真签名的事实连同等长、同类型的另一份字节写到某路径上」（例如有写权限的一方把 A 的元数据原样
// 抄到 B 上）会因 CRC64 对不上而回退原路径（读字节、重新算 sha256、probe），不会把 A 的 sha256 / 宽高安到 B 上。
//
// CRC64 不是密码学摘要：能刻意构造与 A 同 CRC64、同长度、同类型的另一份字节的一方仍可绕过；但这样的一方
// 本来就能直接改写该路径上的字节，这不是服务端复制新引入的面。sha256 仍只在本机读字节时算，复制不重算。
//
// 缺 CRC64 时一律回退原路径，而不是退回「只核对字节数与内容类型」：旧事实（绑定上线前写入）与不给
// x-oss-hash-crc64ecma 的存储（非 OSS 的 S3 兼容实现）都无法证明事实描述的是当前字节；回退只是慢一次，
// 回退写出的新对象带 CRC64，之后的复制即可走服务端。

// sealFacts 编码并签名事实。
func sealFacts(id *identity, f storedFacts) string {
	raw, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + base64.RawURLEncoding.EncodeToString(id.mac(factsMACLabel, payload))
}

// openFacts 验签并解析元数据里的事实；空、签名不对、损坏、版本不认识、sha256 不是 64 位小写十六进制都返回 nil
// （按没有处理，回退原路径）。验签用原始 payload 字节，与 JSON 字段顺序 / 新增字段无关。
func openFacts(id *identity, v string) *storedFacts {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || payload == "" || sig == "" || id == nil {
		return nil
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, id.mac(factsMACLabel, payload)) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var f storedFacts
	if json.Unmarshal(raw, &f) != nil || f.V != storedFactsVersion || !isLowerHex64(f.Sha256) || f.Size < 0 {
		return nil
	}
	return &f
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// trustedFacts 源对象上的事实能否当真：有、验签通过、字节数与内容类型与这次 HEAD 一致、存储类型是标准型，
// 且事实里的 CRC64 与 HEAD 给出的当前字节 CRC64 相等。
// 不能时返回回退原因（日志用）。
func (s *Server) trustedFacts(h objectHead) (*storedFacts, string) { return trustFacts(s.id, h) }

func trustFacts(id *identity, h objectHead) (*storedFacts, string) {
	raw := h.UserMeta[ossFactsHeader]
	if raw == "" {
		return nil, "source_facts_missing"
	}
	f := openFacts(id, raw)
	if f == nil {
		// 签名不对（别的写入方写的、密钥换过）或格式损坏
		return nil, "source_facts_untrusted"
	}
	// 事实必须描述当前这份字节：字节数或内容类型对不上（被不带事实的写入覆盖过、被别的写入方改过）一律不用
	if h.Size < 0 || f.Size != h.Size || f.Meta.Mime != h.ContentType {
		return nil, "source_facts_stale"
	}
	// 非标准存储（低频 / 归档）的源：复制出的对象存储类型可能与「下载 + 上传」（取目标桶默认）不同，回退原路径
	if h.StorageClass != "" && !strings.EqualFold(h.StorageClass, "Standard") {
		return nil, "source_storage_class"
	}
	// 事实必须绑定当前字节：事实里的 CRC64 与 OSS 对当前字节给出的 CRC64 相等。任一侧没有都无法证明，回退原路径
	if f.CRC64 == "" {
		return nil, "source_facts_no_crc64" // 绑定上线前写入的事实
	}
	if h.CRC64 == "" {
		return nil, "source_crc64_missing" // 存储不给 x-oss-hash-crc64ecma
	}
	if f.CRC64 != h.CRC64 {
		return nil, "source_crc64_mismatch" // 字节被换过（例如别处的事实被原样抄过来）
	}
	return f, ""
}

type storeFactsKey struct{}

type storeFactsVal struct {
	path   string
	sum    string
	size   int64
	sealed string
}

// withStoreFacts 登记本地文件 path 的事实（只在 probe 过的落桶里调用），存储的 Put 据此写对象元数据。
// 与 withKnownDigest 一样只对这一个路径生效：同一请求里写的索引、其它产物不带。
// crc 为空（不知道这份字节的 CRC64）时照样写事实，只是之后不能据此服务端复制（见 trustFacts）。
func (s *Server) withStoreFacts(ctx context.Context, path, sum, crc string, size int64, meta MediaMeta) context.Context {
	return withSealedFacts(ctx, path, sum, size,
		sealFacts(s.id, storedFacts{V: storedFactsVersion, Sha256: sum, Size: size, CRC64: crc, Meta: meta}))
}

func withSealedFacts(ctx context.Context, path, sum string, size int64, sealed string) context.Context {
	if sealed == "" {
		return ctx
	}
	return context.WithValue(ctx, storeFactsKey{}, storeFactsVal{path: path, sum: sum, size: size, sealed: sealed})
}

// factsHeaders Put 要附带的元数据头：路径一致、sha256 与字节数与本次写入一致才给，否则 nil（照旧写入）。
func factsHeaders(ctx context.Context, localPath, sum string, size int64) map[string]string {
	v, ok := ctx.Value(storeFactsKey{}).(storeFactsVal)
	if !ok || v.path != localPath || v.sum != sum || v.size != size || v.sealed == "" {
		return nil
	}
	return map[string]string{ossFactsHeader: v.sealed}
}

// copyServerSide 尝试服务端复制。handled=false 表示没有做（不符合条件或被拒），调用方走原来的下载 + 上传；
// handled=true 时返回结果或错误（暂时故障等，按原错误语义返回给调用方重试）。
func (s *Server) copyServerSide(ctx context.Context, srcRid, purpose, srcStore, srcPath string) (MediaResult, bool, error) {
	dstStore := storeFor(purpose)
	copier, ok := s.objects.(serverCopier)
	if !ok {
		logCopyFallback(ctx, srcRid, srcStore, dstStore, "store_unsupported", nil)
		return MediaResult{}, false, nil
	}
	if reason := copier.ServerCopyEligible(srcStore, dstStore); reason != "" {
		logCopyFallback(ctx, srcRid, srcStore, dstStore, reason, nil)
		return MediaResult{}, false, nil
	}
	headStart := time.Now()
	src, err := copier.ObjectFacts(ctx, srcStore, srcPath)
	logStage(ctx, "copy_src_facts", headStart) // HEAD 源对象取落桶时登记的事实
	if err != nil {
		// HEAD 失败（含刚被删除）交给原路径：它有完整的错误语义（404 / 410 / transient）
		logCopyFallback(ctx, srcRid, srcStore, dstStore, "source_head_failed", err)
		return MediaResult{}, false, nil
	}
	facts, reason := s.trustedFacts(src)
	if facts == nil {
		logCopyFallback(ctx, srcRid, srcStore, dstStore, reason, nil)
		return MediaResult{}, false, nil
	}
	src.Facts = facts

	// 以下与 storeMeta 相同：rid 由幂等键派生（没有时由内容摘要派生），已有索引沿用原路径
	class := classFor(purpose)
	source := idemKeyOf(ctx)
	if source == "" {
		source = facts.Sha256
	}
	rid, err := s.id.RID(s.owners.ownerFor(ctx, source), class, source)
	if err != nil {
		return MediaResult{}, true, err
	}
	placeStart := time.Now()
	objPath, reused, err := s.placeObjectReused(ctx, rid, class, dstStore, s.id.ObjectKey(rid), facts.Meta.Mime, time.Now())
	logStage(ctx, "place_object", placeStart)
	if err != nil {
		return MediaResult{}, true, err
	}
	copyStart := time.Now()
	var mode string
	obj, err := s.writeAndClearPending(ctx, dstStore, objPath, reused, func() (StoredObject, error) {
		o, m, err := copier.ServerCopy(ctx, srcStore, srcPath, dstStore, objPath, src)
		mode = m
		return o, err
	})
	logStage(ctx, "copy_server", copyStart) // 存储服务端复制（字节不经本机）
	if err != nil {
		var refused *serverCopyRefused
		if errors.As(err, &refused) {
			// 索引已写好：回退路径算出同一个 rid，沿用同一个路径
			logCopyFallback(ctx, srcRid, srcStore, dstStore, refused.reason, refused.err)
			return MediaResult{}, false, nil
		}
		return MediaResult{}, true, err
	}
	// 目标 rid 在本机可能有旧的写穿透条目（同键重写）：字节没经过本机，不登记新条目，旧条目作废
	s.objCache.forget(rid)
	obj.RID, obj.Class = rid, class
	if obj.Sha256 == "" {
		obj.Sha256 = facts.Sha256
	}
	if obj.Size <= 0 {
		obj.Size = src.Size
	}
	if obj.Mime == "" {
		obj.Mime = facts.Meta.Mime
	}
	urlStart := time.Now()
	u, err := s.objects.URL(ctx, obj)
	logStage(ctx, "url", urlStart)
	if err != nil {
		return MediaResult{}, true, err
	}
	log.Printf("media copy trace_id=%s check=media.copy_server_side src_rid=%s rid=%s src_store=%s dst_store=%s size=%d mode=%s dur_ms=%d",
		TraceIDFromContext(ctx), srcRid, rid, srcStore, dstStore, obj.Size, mode, time.Since(copyStart).Milliseconds())
	meta := facts.Meta
	return MediaResult{
		Object: ObjectRef{
			RID: rid, Class: class,
			Store: obj.Store, Sha256: obj.Sha256, Size: obj.Size, Mime: obj.Mime,
			DurationS: meta.DurationS, Width: meta.Width, Height: meta.Height, Fps: meta.Fps, HasAudio: meta.HasAudio,
		},
		Url: URLRef{Kind: u.Kind, Value: u.Value, ExpiresAt: u.ExpiresAt},
	}, true, nil
}

// logCopyFallback 服务端复制没做、回退下载 + 上传时打一行，带判据与原因（不含对象键与地址）。
func logCopyFallback(ctx context.Context, srcRid, srcStore, dstStore, reason string, err error) {
	detail := ""
	if err != nil {
		detail = " err=" + redactErr(err)
	}
	log.Printf("media copy trace_id=%s check=media.copy_fallback reason=%s src_rid=%s src_store=%s dst_store=%s%s",
		TraceIDFromContext(ctx), reason, srcRid, srcStore, dstStore, detail)
}

// redactErr 错误文本去掉请求地址（*url.Error 带完整 URL，含桶域名与对象键），只留操作与原因。
func redactErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return strings.Replace(err.Error(), ue.Error(), ue.Op+": "+ue.Err.Error(), 1)
	}
	return err.Error()
}
