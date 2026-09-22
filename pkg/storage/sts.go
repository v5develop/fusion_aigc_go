package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Credential 客户端直传 OSS 用的临时凭证（STS AssumeRole 签发）。
// 权限被策略限定为「只能往 Dir 前缀下写」，有效期 DurationSeconds。
type Credential struct {
	AccessKeyID     string
	AccessKeySecret string
	SecurityToken   string
	Expiration      string // ISO8601 UTC
	Bucket          string
	Host            string // <bucket>.<endpoint host>，客户端 PUT 的目标
	Dir             string // 允许写入的对象前缀
	BaseURL         string // 对外访问地址前缀（自定义域名优先）
}

// Uploader 支持客户端直传的存储。local 模式不支持（开发环境用网关代传）。
type Uploader interface {
	// Credentials 为某个用户签发一份限定前缀的上传凭证。
	Credentials(ctx context.Context, kind string, userID int64, ttl time.Duration) (*Credential, error)
	// Stat 读对象大小与类型（客户端传完后校验用）。
	Stat(ctx context.Context, url string) (size int64, contentType string, err error)
	// PresignPut 签一个带签名的 PUT 地址：客户端用它上传，PUT 时 Content-Type 必须与 contentType 一致。
	PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (*Presigned, error)
}

// Presigned 预签名上传地址。
type Presigned struct {
	PutURL    string    // 客户端 PUT 到这里
	URL       string    // 上传成功后的文件地址，提交给业务接口
	ExpiresAt time.Time // PutURL 过期时间
}

var ErrDirectUploadUnsupported = errors.New("storage: 当前存储模式不支持客户端直传")

func (s *localStore) Credentials(context.Context, string, int64, time.Duration) (*Credential, error) {
	return nil, ErrDirectUploadUnsupported
}

func (s *localStore) PresignPut(context.Context, string, string, time.Duration) (*Presigned, error) {
	return nil, ErrDirectUploadUnsupported
}

// PresignPut OSS V1 URL 签名：Signature = base64(hmac-sha1(SK, "PUT\n\n<Content-Type>\n<Expires>\n/<bucket>/<key>"))。
// 用服务端自己的密钥签，客户端拿不到任何密钥；签名只对这一个 key、这一个 Content-Type 有效。
func (s *ossStore) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (*Presigned, error) {
	c, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	expires := time.Now().Add(ttl).Truncate(time.Second)
	exp := fmt.Sprint(expires.Unix())
	mac := hmac.New(sha1.New, []byte(c.SecretKey))
	mac.Write([]byte("PUT\n\n" + contentType + "\n" + exp + "\n/" + c.Bucket + "/" + key))
	q := url.Values{
		"OSSAccessKeyId": {c.AccessKey},
		"Expires":        {exp},
		"Signature":      {base64.StdEncoding.EncodeToString(mac.Sum(nil))},
	}
	return &Presigned{
		PutURL:    "https://" + c.host() + "/" + key + "?" + q.Encode(),
		URL:       c.publicBase() + "/" + key,
		ExpiresAt: expires,
	}, nil
}

// Stat 本地模式直接读文件：go-zero 的文件服务只响应 GET，HEAD 拿不到信息。
func (s *localStore) Stat(_ context.Context, u string) (int64, string, error) {
	if !strings.HasPrefix(u, s.baseURL+"/") {
		return 0, "", errors.New("storage: 不是本存储的地址")
	}
	p := filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(u, s.baseURL+"/")))
	st, err := os.Stat(p)
	if err != nil {
		return 0, "", fmt.Errorf("storage: 读取文件信息失败: %w", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	return st.Size(), http.DetectContentType(head[:n]), nil
}

// Credentials 调 STS AssumeRole 换一份临时凭证，策略限定只能写 <prefix>/<kind>/<userID>/ 下的对象。
func (s *ossStore) Credentials(ctx context.Context, kind string, userID int64, ttl time.Duration) (*Credential, error) {
	c, err := s.config(ctx)
	if err != nil {
		return nil, err
	}
	if c.StsRoleArn == "" {
		return nil, fmt.Errorf("storage: afv_system_config 缺少 storage_public_sts_role_arn")
	}
	dir := fmt.Sprintf("%s/%s/%d/%s/", s.prefix, kind, userID, time.Now().Format("20060102"))
	policy, _ := json.Marshal(map[string]any{
		"Version": "1",
		"Statement": []map[string]any{{
			"Effect":   "Allow",
			"Action":   []string{"oss:PutObject"},
			"Resource": []string{fmt.Sprintf("acs:oss:*:*:%s/%s*", c.Bucket, dir)},
		}},
	})
	if ttl <= 0 {
		ttl = time.Hour
	}
	out, err := s.assumeRole(ctx, c, string(policy), int(ttl.Seconds()), fmt.Sprintf("aigclife-%d", userID))
	if err != nil {
		return nil, err
	}
	return &Credential{
		AccessKeyID: out.AccessKeyID, AccessKeySecret: out.AccessKeySecret, SecurityToken: out.SecurityToken,
		Expiration: out.Expiration, Bucket: c.Bucket, Host: c.host(), Dir: dir, BaseURL: c.publicBase(),
	}, nil
}

func (s *ossStore) Stat(_ context.Context, u string) (int64, string, error) {
	return statURL(u)
}

type stsCredential struct {
	AccessKeyID     string `json:"AccessKeyId"`
	AccessKeySecret string `json:"AccessKeySecret"`
	SecurityToken   string `json:"SecurityToken"`
	Expiration      string `json:"Expiration"`
}

// assumeRole 阿里云 STS 的 RPC 风格接口，自行签名（HMAC-SHA1），不引 SDK。
func (s *ossStore) assumeRole(ctx context.Context, c *ossConfig, policy string, durationSeconds int, session string) (*stsCredential, error) {
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	params := map[string]string{
		"Action":           "AssumeRole",
		"RoleArn":          c.StsRoleArn,
		"RoleSessionName":  session,
		"DurationSeconds":  fmt.Sprint(durationSeconds),
		"Policy":           policy,
		"Version":          "2015-04-01",
		"Format":           "JSON",
		"AccessKeyId":      c.AccessKey,
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureVersion": "1.0",
		"SignatureNonce":   hex.EncodeToString(nonce),
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	params["Signature"] = signRPC(http.MethodPost, params, c.SecretKey)

	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	endpoint := "https://sts." + c.stsRegion() + ".aliyuncs.com/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage: 申请上传凭证失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Credentials stsCredential `json:"Credentials"`
		Code        string        `json:"Code"`
		Message     string        `json:"Message"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("storage: 上传凭证响应解析失败(HTTP %d): %s", resp.StatusCode, truncate(string(raw), 300))
	}
	if resp.StatusCode != http.StatusOK || out.Credentials.AccessKeyID == "" {
		return nil, fmt.Errorf("storage: 申请上传凭证失败(HTTP %d) %s: %s", resp.StatusCode, out.Code, out.Message)
	}
	return &out.Credentials, nil
}

// signRPC 阿里云 RPC 风格签名：对规范化查询串做 HMAC-SHA1。
func signRPC(method string, params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(percentEncode(k) + "=" + percentEncode(params[k]))
	}
	toSign := method + "&" + percentEncode("/") + "&" + percentEncode(b.String())
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(toSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// percentEncode 阿里云要求的 URL 编码：空格为 %20，* 与 ~ 按其规则处理。
func percentEncode(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	return strings.ReplaceAll(e, "%7E", "~")
}

func (c ossConfig) stsRegion() string {
	if c.StsRegion != "" {
		return c.StsRegion
	}
	return c.Region
}

// statURL HEAD 一次，拿大小与类型。
func statURL(u string) (int64, string, error) {
	resp, err := http.Head(u)
	if err != nil {
		return 0, "", fmt.Errorf("storage: 读取文件信息失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("storage: 文件不存在或不可访问(HTTP %d)", resp.StatusCode)
	}
	return resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// 上传文件类型
const (
	KindImage = "image"
	KindVideo = "video"
)

// Owned 校验客户端直传上来的地址是否属于本存储签发的目录。
// 文件大小与类型由前端控制，后端不管（产品已确认）。
func Owned(s Store, rawURL string) error {
	if rawURL == "" || !s.Owns(rawURL) {
		return errors.New("请先通过上传接口上传")
	}
	return nil
}
