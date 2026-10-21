package media

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 资源身份（设计文档 §1、§4、§5）。
//
//   rid        = <所属实例 ID 6 位> "-" <类别> "-" <HMAC(密钥, "rid|"+类别+"|"+幂等键) 的 26 位编码>
//   object_key = <HMAC(密钥, "key|"+rid) 的 26 位编码>      对外可见，不含 rid、不含实例 ID
//   桶内路径   = v1/<class>/<object_key>，class ∈ user | work | deliver | asset
//
// rid 由幂等键派生：同一个幂等键的重试得到同一个 rid 与同一个对象键，天然不产生重复对象。

// 对象键与 rid 使用的编码：小写 base32，无填充。
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

const idLen = 26 // 26 个字符 = 130 bit，超过 128 bit 的安全下限

var (
	instanceIDPattern = regexp.MustCompile(`^[a-z0-9]{6}$`)
	ridPattern        = regexp.MustCompile(`^([a-z0-9]{6})-(user|work|deliver|asset)-([a-z2-7]{26})$`)
	objectKeyPattern2 = regexp.MustCompile(`^[a-z2-7]{26}$`)
)

// minIdentitySecretLen HMAC 密钥的最短长度（字节）。
const minIdentitySecretLen = 32

// identity 持有实例 ID 与派生密钥。所有派生都是纯函数，同输入同输出。
type identity struct {
	instanceID string
	secret     []byte
}

func newIdentity(instanceID, secret string) (*identity, error) {
	if !instanceIDPattern.MatchString(instanceID) {
		return nil, errors.New("media: InstanceID 必须是 6 位小写字母或数字（部署时生成并持久化）")
	}
	if len(secret) < minIdentitySecretLen {
		return nil, fmt.Errorf("media: IdentitySecret 至少 %d 字节", minIdentitySecretLen)
	}
	return &identity{instanceID: instanceID, secret: []byte(secret)}, nil
}

// mac HMAC-SHA256(密钥, label|value)。label 区分派生用途，避免不同用途的输出互相关联。
func (id *identity) mac(label, value string) []byte {
	h := hmac.New(sha256.New, id.secret)
	h.Write([]byte(label + "|" + value))
	return h.Sum(nil)
}

// RID 由类别与幂等键派生资源 ID。owner 为空时归属本实例。
func (id *identity) RID(owner, class, idemKey string) (string, error) {
	if owner == "" {
		owner = id.instanceID
	}
	if !instanceIDPattern.MatchString(owner) {
		return "", errors.New("media: owner 实例 ID 不合法")
	}
	if !validClass(class) {
		return "", errors.New("media: 类别不合法")
	}
	if idemKey == "" {
		return "", errors.New("media: 派生 rid 需要幂等键")
	}
	code := idEncoding.EncodeToString(id.mac("rid", class+"|"+idemKey))[:idLen]
	return owner + "-" + class + "-" + code, nil
}

// ParseRID 校验 rid 格式，返回所属实例 ID 与类别。
func ParseRID(rid string) (owner, class string, err error) {
	m := ridPattern.FindStringSubmatch(rid)
	if m == nil {
		return "", "", errors.New("media: rid 格式不合法")
	}
	return m[1], m[2], nil
}

// ObjectKey 由 rid 派生对外可见的对象键。不含 rid、不含实例 ID。
func (id *identity) ObjectKey(rid string) string {
	return idEncoding.EncodeToString(id.mac("key", rid))[:idLen]
}

// 资源类别（桶内前缀）。
const (
	classUser    = "user"
	classWork    = "work"
	classDeliver = "deliver"
	classAsset   = "asset"
)

// validClass 类别是否合法。
func validClass(c string) bool {
	return c == classUser || c == classWork || c == classDeliver || c == classAsset
}

// storePath 桶内路径：v1/<class>/<object_key>。
func storePath(class, objectKey string) string {
	return "v1/" + class + "/" + objectKey
}

// validObjectKey 对象键格式校验。
func validObjectKey(key string) bool {
	return objectKeyPattern2.MatchString(strings.TrimSpace(key))
}

// objectPathPattern 桶内路径的合法形式：v1/<类别>/<对象键>。
var objectPathPattern = regexp.MustCompile(`^v1/(user|work|deliver|asset)/([a-z2-7]{26})$`)

// legacyPathPattern 历史路径（只读）：sha256/<64 位十六进制>[.ext]。
var legacyPathPattern = regexp.MustCompile(`^sha256/([0-9a-f]{64})(\.[a-z0-9]{1,8})?$`)

// classOfPath 返回路径所属类别；历史路径返回 legacy。非法路径返回空串。
func classOfPath(path string) string {
	if m := objectPathPattern.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	if legacyPathPattern.MatchString(path) {
		return "legacy"
	}
	return ""
}

// classFor 用途到类别（设计文档 §2）。
func classFor(purpose string) string {
	switch purpose {
	case "user_input":
		return classUser
	case "asset_library":
		return classAsset
	case "third_party":
		return classDeliver
	default:
		return classWork
	}
}
