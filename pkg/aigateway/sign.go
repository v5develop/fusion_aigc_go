package aigateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 签名协议版本与七个请求头（contract/hmac.md）。
const (
	SignVersion = "v1"

	HeaderVersion   = "X-Aig-Version"
	HeaderCaller    = "X-Aig-Caller"
	HeaderKeyID     = "X-Aig-Key-Id"
	HeaderPurpose   = "X-Aig-Purpose"
	HeaderTimestamp = "X-Aig-Timestamp"
	HeaderNonce     = "X-Aig-Nonce"
	HeaderSignature = "X-Aig-Signature"
)

// 路由 scope，签名里的 X-Aig-Purpose 必须等于目标路由的 x-aig-scope。
const (
	PurposeCalls   = "calls"
	PurposeCatalog = "catalog"
	PurposeLedger  = "ledger"
	PurposeAdmin   = "admin"
)

// emptyBodySHA256 空 body（GET 等）的 sha256。
const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

var (
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

// Signer 一把调用方密钥。CallerID 必须与网关登记的 key 归属一致（Go 后端为 fusion-go）。
type Signer struct {
	CallerID string
	KeyID    string
	Secret   string
}

// Sign 计算签名并把七个 X-Aig-* 头写入 req。
//
// body 必须是线上实际发送的字节（不要重新序列化后再签）；无 body 传 nil。
// 路径取 req.URL.Path，query 取 req.URL.RawQuery，二者都按 hmac.md 规范化后入签。
func (s Signer) Sign(req *http.Request, purpose string, body []byte, now time.Time, nonce string) error {
	if req == nil || req.URL == nil {
		return errors.New("aigateway: 签名请求为空")
	}
	if s.Secret == "" {
		return errors.New("aigateway: 未配置签名密钥 (check=hmac.secret_configured)")
	}
	if !tokenRe.MatchString(s.CallerID) || !tokenRe.MatchString(s.KeyID) || !tokenRe.MatchString(purpose) {
		return fmt.Errorf("aigateway: caller/key id/purpose 格式非法 (check=hmac.token_format)")
	}
	if !nonceRe.MatchString(nonce) {
		return fmt.Errorf("aigateway: nonce 格式非法 (check=hmac.nonce_format)")
	}
	path, err := NormalizePath(req.URL.Path)
	if err != nil {
		return err
	}
	ts := now.Unix()
	canonical := CanonicalString(s.CallerID, purpose, req.Method, path, CanonicalQuery(req.URL.RawQuery), ts, nonce, body)

	h := req.Header
	if h == nil {
		h = http.Header{}
		req.Header = h
	}
	h.Set(HeaderVersion, SignVersion)
	h.Set(HeaderCaller, s.CallerID)
	h.Set(HeaderKeyID, s.KeyID)
	h.Set(HeaderPurpose, purpose)
	h.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderSignature, Signature(s.Secret, canonical))
	return nil
}

// Signature lower-hex HMAC-SHA256(secret, canonical)。
func Signature(secret, canonical string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}

// CanonicalString 九行以 \n 连接、末尾无换行：
// v1 / caller / purpose / METHOD / path / canonical_query / timestamp / nonce / sha256(body)。
// path 与 canonicalQuery 须已规范化（见 NormalizePath、CanonicalQuery）。
func CanonicalString(callerID, purpose, method, path, canonicalQuery string, timestamp int64, nonce string, body []byte) string {
	return strings.Join([]string{
		SignVersion,
		callerID,
		purpose,
		strings.ToUpper(method),
		path,
		canonicalQuery,
		strconv.FormatInt(timestamp, 10),
		nonce,
		BodySHA256(body),
	}, "\n")
}

// BodySHA256 body 字节的小写 hex sha256；nil 与空切片都按空串计算。
func BodySHA256(body []byte) string {
	if len(body) == 0 {
		return emptyBodySHA256
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// NormalizePath 绝对路径，连续的 / 折叠为一个，去掉末尾 /（根路径除外）。冒号路由原样保留（/v1/calls:batchGet）。
func NormalizePath(path string) (string, error) {
	if path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return "", errors.New("aigateway: 路径必须是绝对路径且不含 query/fragment (check=hmac.path_format)")
	}
	var b strings.Builder
	b.Grow(len(path))
	for i := 0; i < len(path); i++ {
		if path[i] == '/' && i > 0 && path[i-1] == '/' {
			continue
		}
		b.WriteByte(path[i])
	}
	out := b.String()
	if len(out) > 1 && strings.HasSuffix(out, "/") {
		out = out[:len(out)-1]
	}
	return out, nil
}

// CanonicalQuery 由原始 query 串（a=1&b=x%20y）得到规范 query：
// 先逐对解码（+ 视为空格，与网关 parse_qsl、Java URLDecoder 一致），再按 RFC 3986 编码键和值，
// 按 (编码后的 key, 编码后的 value) 排序，以 & 连接。无 query 返回空串。
func CanonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	var pairs [][2]string
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{lenientUnescape(k), lenientUnescape(v)})
	}
	return canonicalPairs(pairs)
}

// CanonicalValues 由已解码的参数得到规范 query（允许一键多值）。
func CanonicalValues(values url.Values) string {
	var pairs [][2]string
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	return canonicalPairs(pairs)
}

func canonicalPairs(decoded [][2]string) string {
	encoded := make([][2]string, len(decoded))
	for i, p := range decoded {
		encoded[i] = [2]string{Encode(p[0]), Encode(p[1])}
	}
	sort.Slice(encoded, func(i, j int) bool {
		if encoded[i][0] != encoded[j][0] {
			return encoded[i][0] < encoded[j][0]
		}
		return encoded[i][1] < encoded[j][1]
	})
	var b strings.Builder
	for i, p := range encoded {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p[0])
		b.WriteByte('=')
		b.WriteString(p[1])
	}
	return b.String()
}

// Encode RFC 3986 percent-encoding：保留 A-Z a-z 0-9 - . _ ~，其余按 UTF-8 字节编码为大写 %XX（空格为 %20）。
func Encode(s string) string {
	const hexUpper = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&0x0f])
	}
	return b.String()
}

// lenientUnescape 解码 query 片段。
//
// 与 Python 侧不完全等价（已知差异，勿当作「一致」）：
//   - 合法输入：与 unquote_plus 一致（%XX 解码、+ 变空格）。
//   - 含非法转义（如 %zz、结尾的 %）：Go 这里整段放弃百分号解码，只把 + 换成空格，
//     例如 "a%zz%41+b" -> "a%zz%41 b"；Python unquote 会逐个处理，合法的 %41 仍解成 A、非法的 %zz 保留，
//     且 unquote 不转换 +（unquote_plus 才转换）。
//
// 当前安全的原因：签名用的 query 由 url.Values.Encode 生成（见 CanonicalValues 调用方），
// 只会产出合法转义，不会走到非法分支；此函数只在验签一侧解析「别人给的原始 query」时兜底。
// 若以后有人用手写 / 外部来源的 query 参与签名，两端对非法转义的规范化会分叉，须先对齐再改。
func lenientUnescape(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return strings.ReplaceAll(s, "+", " ")
}
