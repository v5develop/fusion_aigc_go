// Package mcptoken 画布 MCP 访问令牌（外部 MCP 客户端专用，Go_V1.0.42）的纯逻辑：格式、哈希、作用域、上限。
//
//   - 明文 = "mcp_" + 43 位 base64url（32 字节随机）；只在创建时返回一次。
//   - 库里只存 sha256(明文) 小写 hex（唯一键）+ 明文前 12 位（mcp_ + 8 位，列表里便于识别）。
//     令牌本身有 256 位熵，不需要加盐 / 慢哈希；校验按哈希等值查唯一键。
//   - 作用域：canvas_rw（画布读写）/ canvas_rw_generate（画布读写 + 生成，会冻结点数）。
//
// 网关（app/gateway/api/internal/handler/canvasagent）负责解析 Bearer、缓存校验结果、按作用域拦工具；
// canvas-rpc（logic/mcp_token.go）负责存储、上限与吊销。设计见 docs/design/canvas-agent-tools.md §13。
package mcptoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
)

// Prefix 明文前缀（与登录令牌——32 位 hex 会话令牌 / 三段点分 JWT——形状不重叠）。
const Prefix = "mcp_"

const (
	bodyLen = 43 // base64url(32 字节) 无填充
	// DisplayPrefixLen 列表展示 / 库里存的明文前缀长度。
	DisplayPrefixLen = len(Prefix) + 8
	// MaxActivePerUser 每人最多生效中（未吊销、未过期）的令牌数。
	MaxActivePerUser = 10
	// MaxNameLen 名称上限（字符）。
	MaxNameLen = 50
)

// 作用域。
const (
	ScopeRW         = "canvas_rw"
	ScopeRWGenerate = "canvas_rw_generate"
)

// 鉴权来源（AgentCallReq.auth_source）。
const (
	AuthSourceUser     = "user"      // 登录令牌
	AuthSourceMcpToken = "mcp_token" // MCP 访问令牌
	AuthSourceSvc      = "svc"       // 服务间 HMAC 入口
)

// AllowsTool 作用域是否允许该工具：生成类（会冻结点数）需要 canvas_rw_generate，其余 canvas_rw 即可。
func AllowsTool(scope string, generates bool) bool {
	if !ValidScope(scope) {
		return false
	}
	return !generates || AllowsGenerate(scope)
}

// TTLDays 允许的有效期（天）。
var TTLDays = []int32{7, 30, 90}

// ValidScope 作用域是否合法。
func ValidScope(s string) bool { return s == ScopeRW || s == ScopeRWGenerate }

// ValidTTL 有效期是否合法。
func ValidTTL(days int32) bool { return slices.Contains(TTLDays, days) }

// AllowsGenerate 作用域是否允许生成类工具（canvas_run_node）。
func AllowsGenerate(scope string) bool { return scope == ScopeRWGenerate }

// Generate 生成一枚新令牌：明文、哈希、展示前缀。
func Generate() (plain, hash, display string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", err
	}
	plain = Prefix + base64.RawURLEncoding.EncodeToString(b)
	return plain, Hash(plain), plain[:DisplayPrefixLen], nil
}

// Looks 形状像 MCP 令牌（前缀 + 长度 + 字符集）。只看形状，不查库。
func Looks(raw string) bool {
	if !strings.HasPrefix(raw, Prefix) || len(raw) != len(Prefix)+bodyLen {
		return false
	}
	for _, c := range raw[len(Prefix):] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// HasPrefix 带 MCP 令牌前缀（形状不对也算：用于「管理接口拒绝 MCP 令牌」「不当登录令牌解析」）。
func HasPrefix(raw string) bool { return strings.HasPrefix(raw, Prefix) }

// Hash sha256(明文) 小写 hex。
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Grant 一次请求经 MCP 令牌鉴权得到的授权（登录令牌请求没有 Grant = 不受作用域限制）。
type Grant struct {
	TokenID   int64
	UserID    int64
	Scope     string
	ExpiresAt int64 // unix 毫秒
}

type grantKey struct{}

// WithGrant 把授权放进 context。
func WithGrant(ctx context.Context, g Grant) context.Context {
	return context.WithValue(ctx, grantKey{}, g)
}

// GrantFrom 取授权；ok=false 表示本请求不是 MCP 令牌鉴权的。
func GrantFrom(ctx context.Context) (Grant, bool) {
	g, ok := ctx.Value(grantKey{}).(Grant)
	return g, ok
}
