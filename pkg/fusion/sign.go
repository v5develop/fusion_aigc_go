package fusion

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// stringToSign 待签串：7 段换行拼接，**结尾没有换行**。
//
//	method \n requestURI \n canonicalQuery \n sha256hex(body) \n timestamp \n nonce \n appKey
//
// 少一段、多一个换行、query 没排序，对方都会算出不一样的签名并回 401。
func stringToSign(method, uri, canonicalQuery, bodyHash, timestamp, nonce, appKey string) string {
	return strings.Join([]string{method, uri, canonicalQuery, bodyHash, timestamp, nonce, appKey}, "\n")
}

func hmacSHA256(src, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(src))
	return hex.EncodeToString(mac.Sum(nil))
}

// sha256Hex body 的 sha256；没有 body 时是空串的 sha256（e3b0c442…）。
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// canonicalQuery 参数按 key 升序拼成 k=v&k=v，值做 URL 转义；没有参数时是空串。
func canonicalQuery(q map[string]string) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+url.QueryEscape(q[k]))
	}
	return strings.Join(parts, "&")
}

// genNonce 16 字节随机数的 hex（32 个字符）。
func genNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
