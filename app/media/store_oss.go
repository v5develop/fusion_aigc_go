package media

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 存储名（ObjectRef.store，契约 §5）。存储名就是用途类别，每个类别对应一个桶配置。
// asset_library 与 third_party 是两个公有桶；两者的配置可以填同一个桶，之后分开时只改配置。
const (
	storePrivate    = "private"
	storeAsset      = "asset_library"
	storeThirdParty = "third_party"
)

// storeFor 用途到存储的映射（契约 §5）。
func storeFor(purpose string) string {
	switch purpose {
	case "asset_library":
		return storeAsset
	case "third_party":
		return storeThirdParty
	default:
		return storePrivate
	}
}

// OSSBucket 一个 OSS 桶的连接信息。密钥只从环境变量注入，不写进配置文件。
type OSSBucket struct {
	Endpoint     string `json:",optional"` // 例如 https://oss-cn-shanghai.aliyuncs.com
	Bucket       string `json:",optional"`
	AccessKey    string `json:",optional"`
	SecretKey    string `json:",optional"`
	CustomDomain string `json:",optional"` // 公有桶的对外域名，可选
}

// OSSConfig 对象存储配置。Enabled 为 false 时使用本地实现（只用于开发）。
type OSSConfig struct {
	Enabled      bool      `json:",optional"`
	Private      OSSBucket `json:",optional"`
	AssetLibrary OSSBucket `json:",optional"`
	ThirdParty   OSSBucket `json:",optional"`
	// PresignMinutes 私有桶签名地址的有效期（分钟），上限 1440（24 小时）
	PresignMinutes int `json:",default=60"`
	// DisableServerSideCopy 关闭服务端复制（resources/copy 走 OSS CopyObject / UploadPartCopy，不经本机）。
	// 默认 false 即开启；设 true 时一律走「下载 + 上传」（见 copy_server.go）。
	DisableServerSideCopy bool `json:",optional"`
}

// OSSStore 阿里云 OSS 实现（OSS v1 签名，与 pkg/storage 的 oss.go 同一套签名算法）。
type OSSStore struct {
	private, asset, thirdParty OSSBucket
	presign                    time.Duration
	hc                         *http.Client
	now                        func() time.Time
	copier                     *ossCopyState // 服务端复制（store_oss_copy.go）
}

// NewOSSStore 校验配置并返回存储。两个桶的连接信息必须完整。
func NewOSSStore(cfg OSSConfig) (*OSSStore, error) {
	if err := checkBucket("private", cfg.Private); err != nil {
		return nil, err
	}
	if err := checkBucket("asset_library", cfg.AssetLibrary); err != nil {
		return nil, err
	}
	if err := checkBucket("third_party", cfg.ThirdParty); err != nil {
		return nil, err
	}
	if cfg.PresignMinutes <= 0 || cfg.PresignMinutes > 1440 {
		return nil, errors.New("media: OSS PresignMinutes 须在 (0, 1440] 之间")
	}
	return &OSSStore{
		private:    cfg.Private,
		asset:      cfg.AssetLibrary,
		thirdParty: cfg.ThirdParty,
		presign:    time.Duration(cfg.PresignMinutes) * time.Minute,
		hc:         &http.Client{Timeout: 10 * time.Minute},
		now:        time.Now,
		copier:     newOSSCopyState(cfg.DisableServerSideCopy),
	}, nil
}

func checkBucket(name string, b OSSBucket) error {
	if b.Endpoint == "" || b.Bucket == "" || b.AccessKey == "" || b.SecretKey == "" {
		return fmt.Errorf("media: OSS %s 桶配置不完整（Endpoint/Bucket/AccessKey/SecretKey 必填）", name)
	}
	return nil
}

func (o *OSSStore) bucket(store string) (OSSBucket, error) {
	switch store {
	case storePrivate:
		return o.private, nil
	case storeAsset:
		return o.asset, nil
	case storeThirdParty:
		return o.thirdParty, nil
	default:
		return OSSBucket{}, errInvalidObjectKey
	}
}

// host 桶的访问域名：<bucket>.<endpoint host>。
func host(b OSSBucket) string {
	h := b.Endpoint
	if u, err := url.Parse(b.Endpoint); err == nil && u.Host != "" {
		h = u.Host
	}
	return b.Bucket + "." + h
}

// Put 把本地文件写到 store 的 path 下。覆盖写，同路径同内容结果一致。
func (o *OSSStore) Put(ctx context.Context, store, path, localPath, mime string) (StoredObject, error) {
	// 调用方已算过摘要（storeWithMeta / 字节上传）时复用，不再整读一遍文件
	d, err := digestFileKnown(ctx, localPath)
	if err != nil {
		return StoredObject{}, fmt.Errorf("media: 读取源文件: %w", err)
	}
	sum, size := d.sum, d.size
	if classOfPath(path) == "" || !validStore(store) {
		return StoredObject{}, errInvalidObjectKey
	}
	f, err := os.Open(localPath)
	if err != nil {
		return StoredObject{}, fmt.Errorf("media: 打开源文件: %w", err)
	}
	defer f.Close()
	// 落桶时已知的事实（sha256 + probe 元数据）写进对象元数据：之后服务端复制不读字节也能给出结果（copy_server.go）
	etag, crc, err := o.putStreamWith(ctx, store, path, mime, factsHeaders(ctx, localPath, sum, size), f, size)
	if err != nil {
		return StoredObject{}, err
	}
	// OSS 对收到的字节给出 CRC64，与本地算的（事实里签进去的那个）核对。不一致绝不让写入失败（CRC64 绑定只是
	// 服务端复制的加速条件，不能挡用户写入）：限流打 ERROR（check=media.crc64_mismatch），对象保留，照常返回。
	// 对象上的事实带的是本地 CRC64，与 OSS 对当前字节给出的 CRC64 不等，trustFacts 必然判 source_crc64_mismatch，
	// 等同于「事实不带 CRC64」：之后对它的复制一律回退原路径（读真实字节、重算 sha256、probe），不会按这份事实
	// 做服务端复制。只有日后该路径被写成 CRC64 恰好等于事实里那个值的字节（即本地这份字节）时事实才会再被当真，
	// 那时事实描述的正是当前字节。任一侧没有 CRC64 时不核对。
	if d.crc64 != "" && crc != "" && d.crc64 != crc {
		logCRC64Mismatch(store, path, d.crc64, crc)
	}
	return StoredObject{Store: store, Key: path, Sha256: sum, Size: size, Mime: mime, Version: normalizeETag(etag)}, nil
}

// Fetch 把对象下载到 dir 下的临时文件并返回路径（只读，调用方不得修改或删除）。
func (o *OSSStore) Fetch(ctx context.Context, store, key, dir string) (string, error) {
	if classOfPath(key) == "" {
		return "", errInvalidObjectKey
	}
	b, err := o.bucket(store)
	if err != nil {
		return "", err
	}
	resp, err := o.get(ctx, store, b, key)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errObjectNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("media: OSS 读取返回 %d", resp.StatusCode)
	}
	path := filepath.Join(dir, filepath.Base(key))
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return "", err
	}
	return path, out.Close()
}

// URL 公有桶返回公有地址（优先自定义域名）；私有桶返回签名地址，有效期见 PresignMinutes。
func (o *OSSStore) URL(_ context.Context, obj StoredObject) (StoredURL, error) {
	b, err := o.bucket(obj.Store)
	if err != nil {
		return StoredURL{}, err
	}
	if obj.Store != storePrivate {
		base := "https://" + host(b)
		if b.CustomDomain != "" {
			base = strings.TrimRight(b.CustomDomain, "/")
			if !strings.HasPrefix(base, "http") {
				base = "https://" + base
			}
		}
		return StoredURL{Kind: "public", Value: base + "/" + obj.Key}, nil
	}
	expires := o.now().Add(o.presign).Unix()
	sig := sign(b.SecretKey, "GET\n\n\n"+strconv.FormatInt(expires, 10)+"\n/"+b.Bucket+"/"+obj.Key)
	q := url.Values{}
	q.Set("OSSAccessKeyId", b.AccessKey)
	q.Set("Expires", strconv.FormatInt(expires, 10))
	q.Set("Signature", sig)
	return StoredURL{
		Kind:      "signed",
		Value:     "https://" + host(b) + "/" + obj.Key + "?" + q.Encode(),
		ExpiresAt: time.Unix(expires, 0).Format(time.RFC3339),
	}, nil
}

// Delete 删除对象。只允许 work、deliver、asset 类别；user 与历史路径拒绝。
// ListIndexes 按 prefix=idx/ 列举私有桶的索引对象，LastModified 取自列举结果。
func (o *OSSStore) ListIndexes(ctx context.Context, marker string, limit int) ([]IndexEntry, string, error) {
	b, err := o.bucket(storePrivate)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("prefix", "idx/")
	q.Set("max-keys", strconv.Itoa(limit))
	if marker != "" {
		q.Set("marker", marker)
	}
	resp, err := o.signedReq(ctx, http.MethodGet, b, "", q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("media: OSS 列举索引返回 %d: %s", resp.StatusCode, raw)
	}
	var lr ossListResult
	if err := xml.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, "", fmt.Errorf("media: OSS 列举索引解析失败: %w", err)
	}
	out := make([]IndexEntry, 0, len(lr.Contents))
	for _, c := range lr.Contents {
		key, ok := objectKeyOfIndexName(strings.TrimPrefix(c.Key, "idx/"))
		if !ok {
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, c.LastModified)
		out = append(out, IndexEntry{ObjectKey: key, Modified: at})
	}
	next := ""
	if lr.IsTruncated && len(lr.Contents) > 0 {
		next = lr.Contents[len(lr.Contents)-1].Key
	}
	return out, next, nil
}

// DeleteIndex 删除索引对象（私有桶）。不存在视为成功。
func (o *OSSStore) DeleteIndex(ctx context.Context, objectKey string) error {
	b, err := o.bucket(storePrivate)
	if err != nil {
		return err
	}
	resp, err := o.do(ctx, http.MethodDelete, storePrivate, b, indexKeyOf(objectKey), "", nil, -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("media: OSS 删除索引返回 %d", resp.StatusCode)
	}
}

func (o *OSSStore) Delete(ctx context.Context, store, path string) (bool, error) {
	if !deletable(path) {
		return false, errUserInputProtected
	}
	return o.remove(ctx, store, path)
}

// DeleteRetentionExpired 删除保留期到期的用户输入；只收 user 类别。
func (o *OSSStore) DeleteRetentionExpired(ctx context.Context, store, path string) (bool, error) {
	if classOfPath(path) != classUser {
		return false, errUserInputProtected
	}
	return o.remove(ctx, store, path)
}

// remove 物理删除 OSS 对象；调用方已做类别校验。
func (o *OSSStore) remove(ctx context.Context, store, path string) (bool, error) {
	b, err := o.bucket(store)
	if err != nil {
		return false, err
	}
	resp, err := o.do(ctx, http.MethodDelete, store, b, path, "", nil, -1)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("media: OSS 删除返回 %d", resp.StatusCode)
	}
}

func (o *OSSStore) putBytes(ctx context.Context, store, key, contentType string, data []byte) error {
	_, _, err := o.putStreamWith(ctx, store, key, contentType, nil, bytes.NewReader(data), int64(len(data)))
	return err
}

// putStream 写入对象，返回响应里的 ETag（对象缓存用作版本；没有则为空）。
func (o *OSSStore) putStream(ctx context.Context, store, key, contentType string, body io.Reader, size int64) (string, error) {
	etag, _, err := o.putStreamWith(ctx, store, key, contentType, nil, body, size)
	return etag, err
}

// putStreamWith 同 putStream，另外带 x-oss-* 头（小写键，进入签名），并返回响应里的 x-oss-hash-crc64ecma（没有为空）。
// headers 为空时与 putStream 的请求完全相同。
func (o *OSSStore) putStreamWith(ctx context.Context, store, key, contentType string, headers map[string]string, body io.Reader, size int64) (string, string, error) {
	b, err := o.bucket(store)
	if err != nil {
		return "", "", err
	}
	resp, err := o.send(ctx, http.MethodPut, b, key, "", contentType, headers, body, size)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", "", fmt.Errorf("media: OSS 写入返回 %d: %s", resp.StatusCode, raw)
	}
	return resp.Header.Get("ETag"), ossCRC64Of(resp.Header), nil
}

// ossCRC64Header OSS 对对象字节算的 CRC64（PutObject / CopyObject / CompleteMultipartUpload 响应与 HEAD 都带）。
const ossCRC64Header = "x-oss-hash-crc64ecma"

// ossCRC64Of 读 x-oss-hash-crc64ecma，按无符号十进制归一（去空白、去前导零）；没有或不是数字返回空（按「不知道」处理）。
func ossCRC64Of(h http.Header) string {
	v := strings.TrimSpace(h.Get(ossCRC64Header))
	if v == "" {
		return ""
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return ""
	}
	return formatCRC64(n)
}

func (o *OSSStore) exists(ctx context.Context, store, key string) (bool, error) {
	b, err := o.bucket(store)
	if err != nil {
		return false, err
	}
	resp, err := o.do(ctx, http.MethodHead, store, b, key, "", nil, -1)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("media: OSS 查询返回 %d", resp.StatusCode)
	}
}

func (o *OSSStore) get(ctx context.Context, store string, b OSSBucket, key string) (*http.Response, error) {
	return o.do(ctx, http.MethodGet, store, b, key, "", nil, -1)
}

// do 发起一次 OSS 请求。私有桶带 OSS v1 签名头；公有桶的读取不签名。
func (o *OSSStore) do(ctx context.Context, method, store string, b OSSBucket, key, contentType string,
	body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "https://"+host(b)+"/"+key, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if store == storePrivate || method != http.MethodGet {
		date := o.now().UTC().Format(http.TimeFormat)
		req.Header.Set("Date", date)
		toSign := method + "\n\n" + contentType + "\n" + date + "\n/" + b.Bucket + "/" + key
		req.Header.Set("Authorization", "OSS "+b.AccessKey+":"+sign(b.SecretKey, toSign))
	}
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("media: OSS 请求失败: %w", err)
	}
	return resp, nil
}

func sign(secret, toSign string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(toSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// validStore 契约允许的存储名。
func validStore(store string) bool {
	return store == storePrivate || store == storeAsset || store == storeThirdParty
}

// Head 查询对象字节数。对象不存在返回 errObjectNotFound。
func (o *OSSStore) Head(ctx context.Context, store, path string) (int64, error) {
	b, err := o.bucket(store)
	if err != nil {
		return 0, err
	}
	resp, err := o.do(ctx, http.MethodHead, store, b, path, "", nil, -1)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, errObjectNotFound
	default:
		return 0, fmt.Errorf("media: OSS 查询返回 %d", resp.StatusCode)
	}
}

// Version HEAD 对象，返回字节数与 ETag（对象缓存的版本）。不存在返回 errObjectNotFound。
// ETag 随内容变化（同内容覆盖写得到同一 ETag，字节也相同，命中仍然正确）。
func (o *OSSStore) Version(ctx context.Context, store, path string) (int64, string, error) {
	b, err := o.bucket(store)
	if err != nil {
		return 0, "", err
	}
	resp, err := o.do(ctx, http.MethodHead, store, b, path, "", nil, -1)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, normalizeETag(resp.Header.Get("ETag")), nil
	case http.StatusNotFound:
		return 0, "", errObjectNotFound
	default:
		return 0, "", fmt.Errorf("media: OSS 查询返回 %d", resp.StatusCode)
	}
}

// SignedURL 签发有效期为 ttl 的 GET 签名地址（OSS v1 查询签名）。
func (o *OSSStore) SignedURL(ctx context.Context, store, path string, ttl time.Duration) (StoredURL, error) {
	return o.signedGetURL(ctx, store, path, "", ttl)
}

// SignedProcessURL 签发带图片处理样式的 GET 签名地址：地址带 x-oss-process=<style>，签名的 canonical resource
// 同时包含该子资源（OSS 只签子资源，x-oss-process 是子资源，因此改样式会改签名）。style 为空等同于 SignedURL。
// style 必须是 validProcessStyle 允许的字符（字母数字与 _ . / - ,），不做 URL 编码：这些字符在查询串里原样合法，
// 签名用的值与地址里的值完全一致。
func (o *OSSStore) SignedProcessURL(ctx context.Context, store, path, style string, ttl time.Duration) (StoredURL, error) {
	if style != "" && !validProcessStyle(style) {
		return StoredURL{}, invalid("request.process_style")
	}
	return o.signedGetURL(ctx, store, path, style, ttl)
}

// signedGetURL 构造 GET 查询签名。canonical resource 为 "/<bucket>/<key>"，有样式时追加 "?x-oss-process=<style>"
// （原始值，不编码）；查询串的参数名按字典序排列，签名参数不进 canonical resource。
func (o *OSSStore) signedGetURL(_ context.Context, store, path, style string, ttl time.Duration) (StoredURL, error) {
	b, err := o.bucket(store)
	if err != nil {
		return StoredURL{}, err
	}
	expires := o.now().Add(ttl).Unix()
	exp := strconv.FormatInt(expires, 10)
	resource := "/" + b.Bucket + "/" + path
	if style != "" {
		resource += "?x-oss-process=" + style
	}
	sig := sign(b.SecretKey, "GET\n\n\n"+exp+"\n"+resource)
	q := url.Values{}
	q.Set("OSSAccessKeyId", b.AccessKey)
	q.Set("Expires", exp)
	q.Set("Signature", sig)
	query := q.Encode()
	if style != "" {
		// Encode 按键排序后 x-oss-process 排在最后，与 canonical resource 的子资源一致；style 不含需转义的字符。
		query += "&x-oss-process=" + style
	}
	return StoredURL{
		Kind:      "signed",
		Value:     "https://" + host(b) + "/" + path + "?" + query,
		ExpiresAt: time.Unix(expires, 0).Format(time.RFC3339),
	}, nil
}

// PublicURL 公有桶返回公有地址（优先自定义域名）；私有桶返回 Kind=none。
func (o *OSSStore) PublicURL(_ context.Context, store, path string) (StoredURL, error) {
	b, err := o.bucket(store)
	if err != nil {
		return StoredURL{}, err
	}
	if store == storePrivate {
		return StoredURL{Kind: "none"}, nil
	}
	base := "https://" + host(b)
	if b.CustomDomain != "" {
		base = strings.TrimRight(b.CustomDomain, "/")
		if !strings.HasPrefix(base, "http") {
			base = "https://" + base
		}
	}
	return StoredURL{Kind: "public", Value: base + "/" + path}, nil
}

// SignedPutURL 签发有效期为 ttl 的 PUT 签名地址（OSS v1 查询签名）。客户端直传时不得设置 Content-Type，
// 否则签名不匹配。
func (o *OSSStore) SignedPutURL(_ context.Context, store, path string, ttl time.Duration) (StoredURL, error) {
	b, err := o.bucket(store)
	if err != nil {
		return StoredURL{}, err
	}
	expires := o.now().Add(ttl).Unix()
	sig := sign(b.SecretKey, "PUT\n\n\n"+strconv.FormatInt(expires, 10)+"\n/"+b.Bucket+"/"+path)
	q := url.Values{}
	q.Set("OSSAccessKeyId", b.AccessKey)
	q.Set("Expires", strconv.FormatInt(expires, 10))
	q.Set("Signature", sig)
	return StoredURL{
		Kind:      "signed",
		Value:     "https://" + host(b) + "/" + path + "?" + q.Encode(),
		ExpiresAt: time.Unix(expires, 0).Format(time.RFC3339),
	}, nil
}

// ossDeleteAfterHeader 待删除标记的到期时间（对象元数据，RFC3339）。
const ossDeleteAfterHeader = "x-oss-meta-delete-after"

// ossDeleteReasonHeader 待删除标记的原因（对象元数据）。保留期删除写 retention；通用删除不写。
const ossDeleteReasonHeader = "x-oss-meta-delete-reason"

// signedReq 发起一次总是签名的请求，用于待删除标记与列举。与 do 不同：不看桶的读权限，写与读都签。
// headers 的键必须是小写的 x-oss-* 头，会一并进入签名（OSS 的 CanonicalizedOSSHeaders）。
// key 为空表示桶根（列举）。查询参数不进签名（OSS 只签子资源，prefix/marker 不是子资源）。
func (o *OSSStore) signedReq(ctx context.Context, method string, b OSSBucket, key, rawQuery string, headers map[string]string) (*http.Response, error) {
	u := "https://" + host(b) + "/" + key
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	date := o.now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
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
	toSign := method + "\n\n\n" + date + "\n" + ossHeaders.String() + "/" + b.Bucket + "/" + key
	req.Header.Set("Authorization", "OSS "+b.AccessKey+":"+sign(b.SecretKey, toSign))
	resp, err := o.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("media: OSS 请求失败: %w", err)
	}
	return resp, nil
}

// pendingKey 待删除标记在桶里的键：pending-delete/<path>，与数据对象同桶。
func pendingKey(path string) (string, error) {
	if classOfPath(path) == "" {
		return "", errInvalidObjectKey
	}
	return pendingPrefix + path, nil
}

// CreatePending 用 x-oss-forbid-overwrite 创建标记：已存在时 OSS 返回 409，读回已有的到期时间，不覆盖。
func (o *OSSStore) CreatePending(ctx context.Context, store, path string, deleteAfter time.Time, reason string) (time.Time, bool, error) {
	b, err := o.bucket(store)
	if err != nil {
		return time.Time{}, false, err
	}
	key, err := pendingKey(path)
	if err != nil {
		return time.Time{}, false, err
	}
	at := deleteAfter.UTC().Truncate(time.Second)
	headers := map[string]string{
		"x-oss-forbid-overwrite": "true",
		ossDeleteAfterHeader:     at.Format(time.RFC3339),
	}
	if reason != pendingReasonManual {
		headers[ossDeleteReasonHeader] = reason
	}
	resp, err := o.signedReq(ctx, http.MethodPut, b, key, "", headers)
	if err != nil {
		return time.Time{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return at, true, nil
	case http.StatusConflict:
		existing, ok, err := o.PendingDeadline(ctx, store, path)
		if err != nil {
			return time.Time{}, false, err
		}
		if !ok {
			return time.Time{}, false, errors.New("media: 待删除标记冲突但读取不到")
		}
		return existing, false, nil
	default:
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return time.Time{}, false, fmt.Errorf("media: OSS 登记待删除返回 %d: %s", resp.StatusCode, raw)
	}
}

// PendingDeadline 读取标记的到期时间（HEAD 元数据）。
func (o *OSSStore) PendingDeadline(ctx context.Context, store, path string) (time.Time, bool, error) {
	m, ok, err := o.PendingMarker(ctx, store, path)
	return m.DeleteAfter, ok, err
}

// PendingMarker 读取标记的到期时间与原因（HEAD 元数据 x-oss-meta-delete-after / x-oss-meta-delete-reason）。
func (o *OSSStore) PendingMarker(ctx context.Context, store, path string) (PendingMark, bool, error) {
	b, err := o.bucket(store)
	if err != nil {
		return PendingMark{}, false, err
	}
	key, err := pendingKey(path)
	if err != nil {
		return PendingMark{}, false, err
	}
	resp, err := o.signedReq(ctx, http.MethodHead, b, key, "", nil)
	if err != nil {
		return PendingMark{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		at, err := time.Parse(time.RFC3339, resp.Header.Get(ossDeleteAfterHeader))
		if err != nil {
			return PendingMark{}, false, fmt.Errorf("%w: %v", errPendingCorrupt, err)
		}
		return PendingMark{Store: store, Path: path, Key: key, DeleteAfter: at,
			Reason: resp.Header.Get(ossDeleteReasonHeader)}, true, nil
	case http.StatusNotFound:
		return PendingMark{}, false, nil
	default:
		return PendingMark{}, false, fmt.Errorf("media: OSS 查询待删除标记返回 %d", resp.StatusCode)
	}
}

// ClearPending 删除标记对象；不存在也成功。
func (o *OSSStore) ClearPending(ctx context.Context, store, path string) error {
	b, err := o.bucket(store)
	if err != nil {
		return err
	}
	key, err := pendingKey(path)
	if err != nil {
		return err
	}
	resp, err := o.signedReq(ctx, http.MethodDelete, b, key, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("media: OSS 撤销待删除标记返回 %d", resp.StatusCode)
	}
}

// ossListResult ListObjects（v1）的响应体，只取用到的字段。
type ossListResult struct {
	IsTruncated bool `xml:"IsTruncated"`
	Contents    []struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
}

// ListPending 按 prefix=pending-delete/ 列举标记；到期时间逐条 HEAD 取（列举不返回元数据）。
// 列举与 HEAD 之间被撤销的标记直接跳过。
func (o *OSSStore) ListPending(ctx context.Context, store, class, marker string, limit int) ([]PendingMark, string, error) {
	b, err := o.bucket(store)
	if err != nil {
		return nil, "", err
	}
	q := url.Values{}
	q.Set("prefix", pendingListPrefix(class))
	q.Set("max-keys", strconv.Itoa(limit))
	if marker != "" {
		q.Set("marker", marker)
	}
	resp, err := o.signedReq(ctx, http.MethodGet, b, "", q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("media: OSS 列举待删除标记返回 %d: %s", resp.StatusCode, raw)
	}
	var lr ossListResult
	if err := xml.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, "", fmt.Errorf("media: OSS 列举结果解析失败: %w", err)
	}
	marks := make([]PendingMark, 0, len(lr.Contents))
	for _, c := range lr.Contents {
		path := strings.TrimPrefix(c.Key, pendingPrefix)
		if classOfPath(path) == "" {
			continue
		}
		m, ok, err := o.PendingMarker(ctx, store, path)
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
		m.Key = c.Key
		marks = append(marks, m)
	}
	next := ""
	if lr.IsTruncated && len(lr.Contents) > 0 {
		next = lr.Contents[len(lr.Contents)-1].Key
	}
	return marks, next, nil
}
