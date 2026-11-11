package media

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OSS 服务端复制（copy_server.go 的 serverCopier 实现）。
//
//   - ≤ copyMultipartThreshold（默认 1 GiB，OSS CopyObject 的上限）：一次 CopyObject
//     （PUT 目标 + x-oss-copy-source，x-oss-metadata-directive=COPY：内容类型与 x-oss-meta-* 原样带过去）。
//   - 更大：InitiateMultipartUpload（显式写内容类型与 x-oss-meta-*，分片复制不带元数据）→ 并发 UploadPartCopy
//     （按字节区间）→ CompleteMultipartUpload；任何一步失败都 Abort，不留半截分片。
//
// 请求一律用目标桶的密钥签名：源桶与目标桶同一个账号/密钥时天然可读；密钥不同时需要目标密钥对源桶有读权限，
// 没有时 OSS 返回 403，调用方回退下载 + 上传，并在 copyDeniedTTL 内不再对这对桶尝试（省掉一次注定失败的往返）。
// ACL：复制与 Put 一样不带 x-oss-object-acl，目标对象继承目标桶的读写权限（公有桶公开、私有桶私有）。
// 校验：只复制事实里 CRC64 与 HEAD 的 x-oss-hash-crc64ecma 相等的源（见 copy_server.go 的 trustFacts）；复制完成后
// 目标的 CRC64（复制响应头，没有则 HEAD 目标）必须等于源，否则按「这次做不了」回退原路径覆盖目标。

const (
	defaultCopyMultipartThreshold = int64(1) << 30   // 1 GiB：OSS CopyObject 单次复制的上限
	defaultCopyPartSize           = int64(256) << 20 // 256 MiB：5 GiB 视频 20 片
	copyPartParallel              = 4
	copyDeniedTTL                 = 10 * time.Minute
)

// ossCopyState 服务端复制的开关、分片参数与「最近被拒」记录。
type ossCopyState struct {
	disabled  bool
	threshold int64 // 超过它走分片复制
	partSize  int64
	mu        sync.Mutex
	denied    map[string]time.Time // "src>dst" → 到期时间
}

func newOSSCopyState(disabled bool) *ossCopyState {
	return &ossCopyState{disabled: disabled, threshold: defaultCopyMultipartThreshold, partSize: defaultCopyPartSize,
		denied: map[string]time.Time{}}
}

func copyPairKey(src, dst string) string { return src + ">" + dst }

// ossRegionHost 阿里云 OSS 地域 endpoint：oss-<地域>[-internal].aliyuncs.com，旧式 VPC 前缀 vpc100-oss-<地域>.aliyuncs.com。
var ossRegionHost = regexp.MustCompile(`^(?:vpc\d+-)?oss-([a-z0-9-]+?)(?:-internal)?\.aliyuncs\.com$`)

// endpointHost endpoint 的主机名：容忍有无协议头、尾部斜杠、端口，小写。
func endpointHost(endpoint string) string {
	h := strings.TrimSpace(endpoint)
	if u, err := url.Parse(h); err == nil && u.Host != "" {
		h = u.Host
	} else if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	h = strings.ToLower(h)
	if hn, _, err := net.SplitHostPort(h); err == nil {
		h = hn
	}
	return strings.TrimSuffix(h, ".")
}

// regionKey endpoint 所在地域的比较键；判断不了地域时返回 ""（不做服务端复制）。
//   - 阿里云地域 endpoint：取地域（内网 -internal 与公网同地域）。
//   - 传输加速 / 全局 endpoint（oss-accelerate[-overseas]）：看不出桶在哪个地域，判断不了。
//   - 其它主机（自建 / 兼容存储）：只有主机名完全相同才算同一处（同一服务内复制）。
func regionKey(endpoint string) string {
	h := endpointHost(endpoint)
	if h == "" {
		return ""
	}
	if m := ossRegionHost.FindStringSubmatch(h); m != nil {
		region := m[1]
		if strings.HasPrefix(region, "accelerate") {
			return ""
		}
		return "oss:" + region
	}
	if strings.HasSuffix(h, ".aliyuncs.com") {
		return "" // aliyuncs 下认不出的形态（新式 / 全局域名）：不猜
	}
	return "host:" + h
}

// ServerCopyEligible 关闭、跨地域、最近被拒时不做服务端复制。
func (o *OSSStore) ServerCopyEligible(srcStore, dstStore string) string {
	if o.copier == nil || o.copier.disabled {
		return "disabled"
	}
	sb, err := o.bucket(srcStore)
	if err != nil {
		return "invalid_store"
	}
	db, err := o.bucket(dstStore)
	if err != nil {
		return "invalid_store"
	}
	sr, dr := regionKey(sb.Endpoint), regionKey(db.Endpoint)
	if sr == "" || dr == "" {
		return "region_unknown"
	}
	if sr != dr {
		return "cross_region"
	}
	o.copier.mu.Lock()
	defer o.copier.mu.Unlock()
	if until, ok := o.copier.denied[copyPairKey(srcStore, dstStore)]; ok {
		if o.now().Before(until) {
			return "denied_recently"
		}
		delete(o.copier.denied, copyPairKey(srcStore, dstStore))
	}
	return ""
}

func (o *OSSStore) markCopyDenied(srcStore, dstStore string) {
	if o.copier == nil {
		return
	}
	o.copier.mu.Lock()
	o.copier.denied[copyPairKey(srcStore, dstStore)] = o.now().Add(copyDeniedTTL)
	o.copier.mu.Unlock()
}

// ObjectFacts 签名 HEAD 源对象，读出字节数、ETag、内容类型、全部 x-oss-meta-* 与落桶事实。
func (o *OSSStore) ObjectFacts(ctx context.Context, store, path string) (objectHead, error) {
	if classOfPath(path) == "" {
		return objectHead{}, errInvalidObjectKey
	}
	b, err := o.bucket(store)
	if err != nil {
		return objectHead{}, err
	}
	resp, err := o.send(ctx, http.MethodHead, b, path, "", "", nil, nil, -1)
	if err != nil {
		return objectHead{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return objectHead{}, errObjectNotFound
	default:
		return objectHead{}, fmt.Errorf("media: OSS 查询返回 %d", resp.StatusCode)
	}
	size := resp.ContentLength
	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}
	h := objectHead{
		Size:         size,
		Version:      normalizeETag(resp.Header.Get("ETag")),
		ETag:         resp.Header.Get("ETag"),
		ContentType:  resp.Header.Get("Content-Type"),
		StorageClass: resp.Header.Get("x-oss-storage-class"),
		CRC64:        ossCRC64Of(resp.Header),
		UserMeta:     map[string]string{},
	}
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-oss-meta-") && len(vs) > 0 {
			h.UserMeta[lk] = vs[0]
		}
	}
	// 事实的验签与核对（字节数、内容类型、存储类型、CRC64）在 Server.trustedFacts：签名密钥不在存储层
	return h, nil
}

// ServerCopy 见 serverCopier。
func (o *OSSStore) ServerCopy(ctx context.Context, srcStore, srcPath, dstStore, dstPath string, src objectHead) (StoredObject, string, error) {
	if classOfPath(srcPath) == "" || classOfPath(dstPath) == "" {
		return StoredObject{}, "", errInvalidObjectKey
	}
	sb, err := o.bucket(srcStore)
	if err != nil {
		return StoredObject{}, "", err
	}
	db, err := o.bucket(dstStore)
	if err != nil {
		return StoredObject{}, "", err
	}
	if src.Facts == nil || src.ETag == "" {
		// 没有验过的事实就不复制：结果的 sha256 / 元数据无从给出
		return StoredObject{}, "", refusedOnce("source_facts_missing", nil)
	}
	if src.CRC64 == "" || src.Facts.CRC64 != src.CRC64 {
		// 存储层再守一道：事实没绑定到 HEAD 到的字节（trustFacts 已拦，这里防调用方绕过）就不复制
		return StoredObject{}, "", refusedOnce("source_crc64_unverified", nil)
	}
	cs := copySrc{path: "/" + sb.Bucket + "/" + escapeObjectKey(srcPath), ifMatch: src.ETag}
	mode := "single"
	var etag, dstCRC string
	if src.Size > o.copier.threshold {
		mode = "multipart"
		etag, dstCRC, err = o.multipartCopy(ctx, db, dstPath, cs, src)
	} else {
		etag, dstCRC, err = o.singleCopy(ctx, db, dstPath, cs)
	}
	if err == nil {
		err = o.verifyCopiedCRC(ctx, dstStore, dstPath, src.CRC64, dstCRC)
	}
	if err != nil {
		var refused *serverCopyRefused
		if errors.As(err, &refused) && refused.remember {
			o.markCopyDenied(srcStore, dstStore)
		}
		return StoredObject{}, mode, err
	}
	obj := StoredObject{Store: dstStore, Key: dstPath, Size: src.Size, Mime: src.ContentType, Version: normalizeETag(etag)}
	if src.Facts != nil {
		obj.Sha256 = src.Facts.Sha256
		if src.Facts.Meta.Mime != "" {
			obj.Mime = src.Facts.Meta.Mime
		}
	}
	return obj, mode, nil
}

// verifyCopiedCRC 复制结果的 CRC64 必须等于源（事实绑定的那一份字节）。响应里带了就直接比，没带再 HEAD 目标一次。
// 不一致或目标给不出 CRC64：按「这次做不了」回退原路径（下载 + 上传会覆盖目标，结果仍正确），不记这对桶；
// HEAD 本身失败（网络 / 5xx）按暂时故障返回，调用方重试（同键同路径，覆盖写）。
func (o *OSSStore) verifyCopiedCRC(ctx context.Context, dstStore, dstPath, want, got string) error {
	if got == "" {
		h, err := o.ObjectFacts(ctx, dstStore, dstPath)
		if err != nil {
			return err
		}
		got = h.CRC64
	}
	if got == "" {
		return refusedOnce("dest_crc64_missing", nil)
	}
	if got != want {
		return refusedOnce("dest_crc64_mismatch", nil)
	}
	return nil
}

// escapeObjectKey x-oss-copy-source 里的对象键按段 URL 编码（我方的键只含安全字符，编码后不变）。
func escapeObjectKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ossError OSS 错误响应体（只取 Code）。
type ossError struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
}

// copySrc 复制源：x-oss-copy-source 与 HEAD 时的 ETag（x-oss-copy-source-if-match）。
// 带上 if-match 保证复制的正是验过事实的那一版字节：HEAD 之后源被改写时 OSS 返回 412，回退原路径重新读字节。
type copySrc struct {
	path    string
	ifMatch string
}

func (c copySrc) headers() map[string]string {
	return map[string]string{"x-oss-copy-source": c.path, "x-oss-copy-source-if-match": c.ifMatch}
}

// classifyCopyFailure 复制请求的失败归类：权限 / 不支持 / 跨地域等「做不了」→ *serverCopyRefused（回退，并记下这对桶）；
// 只与这个对象有关的 409 / 412（源在 HEAD 之后变了）→ 回退但不记；源不存在 → errObjectNotFound；
// 其余（5xx、限流）→ 普通错误（暂时故障，按原语义返回）。错误里只有状态码与 Code，不含键。
func classifyCopyFailure(step string, status int, body []byte) error {
	var e ossError
	_ = xml.Unmarshal(body, &e)
	desc := fmt.Sprintf("%s 返回 %d %s", step, status, e.Code)
	switch {
	case status == http.StatusForbidden:
		return refusedCopy("permission", errors.New(desc))
	case status == http.StatusNotFound && e.Code == "NoSuchKey":
		return errObjectNotFound
	case status == http.StatusPreconditionFailed:
		return refusedOnce("source_changed", errors.New(desc))
	case status == http.StatusConflict:
		return refusedOnce("unsupported", errors.New(desc))
	case status == http.StatusNotFound, status == http.StatusBadRequest, status == http.StatusMethodNotAllowed,
		status == http.StatusNotImplemented:
		return refusedCopy("unsupported", errors.New(desc))
	default:
		return fmt.Errorf("media: OSS %s", desc)
	}
}

type ossCopyResult struct {
	ETag string `xml:"ETag"`
}

// readCopyResponse 读 200 响应里的 ETag 与 x-oss-hash-crc64ecma（没有为空）。
// 注意 200 的响应体也可能是 <Error>（S3 兼容实现的长复制会这样），按失败处理。
func readCopyResponse(step string, resp *http.Response) (string, string, error) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", "", classifyCopyFailure(step, resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte("<Error>")) {
		return "", "", classifyCopyFailure(step, http.StatusInternalServerError, body)
	}
	crc := ossCRC64Of(resp.Header)
	var r ossCopyResult
	if err := xml.Unmarshal(body, &r); err != nil || r.ETag == "" {
		if et := resp.Header.Get("ETag"); et != "" {
			return et, crc, nil
		}
		return "", "", fmt.Errorf("media: OSS %s 响应无法解析", step)
	}
	return r.ETag, crc, nil
}

// singleCopy 一次 CopyObject：元数据（内容类型、x-oss-meta-*）原样复制。
func (o *OSSStore) singleCopy(ctx context.Context, db OSSBucket, dstPath string, cs copySrc) (string, string, error) {
	h := cs.headers()
	h["x-oss-metadata-directive"] = "COPY"
	resp, err := o.send(ctx, http.MethodPut, db, dstPath, "", "", h, nil, 0)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	return readCopyResponse("CopyObject", resp)
}

type ossInitiateResult struct {
	UploadID string `xml:"UploadId"`
}

// multipartCopy 分片复制：初始化（写内容类型与元数据）→ 并发按区间 UploadPartCopy → 完成；失败 Abort。
// 返回完成响应里的 ETag 与 CRC64（OSS 对合成后的整个对象算，没有为空）。
func (o *OSSStore) multipartCopy(ctx context.Context, db OSSBucket, dstPath string, cs copySrc, src objectHead) (string, string, error) {
	meta := map[string]string{}
	for k, v := range src.UserMeta {
		meta[k] = v
	}
	resp, err := o.send(ctx, http.MethodPost, db, dstPath, "uploads", src.ContentType, meta, nil, 0)
	if err != nil {
		return "", "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", classifyCopyFailure("InitiateMultipartUpload", resp.StatusCode, body)
	}
	var init ossInitiateResult
	if err := xml.Unmarshal(body, &init); err != nil || init.UploadID == "" {
		return "", "", errors.New("media: OSS InitiateMultipartUpload 响应无法解析")
	}
	uploadID := init.UploadID
	etag, crc, err := o.copyParts(ctx, db, dstPath, cs, uploadID, src.Size)
	if err != nil {
		o.abortMultipart(ctx, db, dstPath, uploadID)
		return "", "", err
	}
	return etag, crc, nil
}

type copiedPart struct {
	n    int
	etag string
}

func (o *OSSStore) copyParts(ctx context.Context, db OSSBucket, dstPath string, cs copySrc, uploadID string, size int64) (string, string, error) {
	partSize := o.copier.partSize
	count := int((size + partSize - 1) / partSize)
	parts := make([]copiedPart, count)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, copyPartParallel)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for i := 0; i < count; i++ {
		start := int64(i) * partSize
		end := min(start+partSize, size) - 1
		wg.Add(1)
		go func(n int, start, end int64) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			etag, err := o.copyPart(ctx, db, dstPath, cs, uploadID, n, start, end)
			if err != nil {
				once.Do(func() { firstErr = err; cancel() })
				return
			}
			parts[n-1] = copiedPart{n: n, etag: etag}
		}(i+1, start, end)
	}
	wg.Wait()
	if firstErr != nil {
		return "", "", firstErr
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	for i, p := range parts {
		if p.n != i+1 || p.etag == "" {
			return "", "", fmt.Errorf("media: OSS UploadPartCopy 第 %d 片缺失", i+1)
		}
	}
	return o.completeMultipart(ctx, db, dstPath, uploadID, parts)
}

func (o *OSSStore) copyPart(ctx context.Context, db OSSBucket, dstPath string, cs copySrc, uploadID string, n int, start, end int64) (string, error) {
	sub := "partNumber=" + strconv.Itoa(n) + "&uploadId=" + uploadID
	h := cs.headers()
	h["x-oss-copy-source-range"] = fmt.Sprintf("bytes=%d-%d", start, end)
	resp, err := o.send(ctx, http.MethodPut, db, dstPath, sub, "", h, nil, 0)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	etag, _, err := readCopyResponse("UploadPartCopy", resp)
	return etag, err
}

func (o *OSSStore) completeMultipart(ctx context.Context, db OSSBucket, dstPath, uploadID string, parts []copiedPart) (string, string, error) {
	sort.Slice(parts, func(i, j int) bool { return parts[i].n < parts[j].n })
	var b strings.Builder
	b.WriteString("<CompleteMultipartUpload>")
	for _, p := range parts {
		b.WriteString("<Part><PartNumber>" + strconv.Itoa(p.n) + "</PartNumber><ETag>")
		_ = xml.EscapeText(&b, []byte(p.etag))
		b.WriteString("</ETag></Part>")
	}
	b.WriteString("</CompleteMultipartUpload>")
	body := []byte(b.String())
	resp, err := o.send(ctx, http.MethodPost, db, dstPath, "uploadId="+uploadID, "", nil, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	return readCopyResponse("CompleteMultipartUpload", resp)
}

// abortMultipart 放弃分片上传（尽力而为，用独立的短超时：请求上下文可能已取消）。
func (o *OSSStore) abortMultipart(ctx context.Context, db OSSBucket, dstPath, uploadID string) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	resp, err := o.send(actx, http.MethodDelete, db, dstPath, "uploadId="+uploadID, "", nil, nil, -1)
	if err == nil {
		resp.Body.Close()
	}
}

// send 发起一次签名请求（OSS v1）：canonical resource 带子资源 subres（已按名字排序，原始值），
// headers 为小写 x-oss-* 头，进入 CanonicalizedOSSHeaders。headers 为空、subres 为空时签名与 do 完全相同。
func (o *OSSStore) send(ctx context.Context, method string, b OSSBucket, key, subres, contentType string,
	headers map[string]string, body io.Reader, size int64) (*http.Response, error) {
	u := "https://" + host(b) + "/" + key
	resource := "/" + b.Bucket + "/" + key
	if subres != "" {
		u += "?" + subres
		resource += "?" + subres
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)
	var ossHeaders strings.Builder
	for _, n := range names {
		req.Header.Set(n, headers[n])
		ossHeaders.WriteString(n + ":" + headers[n] + "\n")
	}
	date := o.now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	toSign := method + "\n\n" + contentType + "\n" + date + "\n" + ossHeaders.String() + resource
	req.Header.Set("Authorization", "OSS "+b.AccessKey+":"+sign(b.SecretKey, toSign))
	resp, err := o.hc.Do(req)
	if err != nil {
		// *url.Error 的文本带完整地址（桶域名 + 对象键）：只留方法与原因，不进日志 / 错误链
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("media: OSS 请求失败: %s: %w", method, ue.Err)
		}
		return nil, fmt.Errorf("media: OSS 请求失败: %w", err)
	}
	return resp, nil
}
