package media

import (
	"strings"
	"testing"
)

const testSecret = "test-identity-secret-0123456789abcdef"

func testIdentity(t *testing.T) *identity {
	t.Helper()
	id, err := newIdentity("tst001", testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 同一个幂等键，rid 与对象键必须完全相同（重试不产生新对象）。
func TestRIDIsDeterministicByIdempotencyKey(t *testing.T) {
	id := testIdentity(t)
	a, _ := id.RID("", "work", "job-42")
	b, _ := id.RID("", "work", "job-42")
	if a != b || id.ObjectKey(a) != id.ObjectKey(b) {
		t.Fatalf("同一幂等键得到不同身份: %s vs %s", a, b)
	}
	c, _ := id.RID("", "work", "job-43")
	if a == c {
		t.Fatal("不同幂等键必须得到不同 rid")
	}
}

// rid 的格式：所属实例 6 位 + 26 位编码；所属实例可解析。
func TestRIDFormatAndOwner(t *testing.T) {
	id := testIdentity(t)
	rid, err := id.RID("", "work", "k1")
	if err != nil {
		t.Fatal(err)
	}
	owner, class, err := ParseRID(rid)
	if err != nil || owner != "tst001" || class != "work" {
		t.Fatalf("ParseRID(%s) = %q %q, %v", rid, owner, class, err)
	}
	other, _ := id.RID("zzz999", "work", "k1")
	if owner2, _, _ := ParseRID(other); owner2 != "zzz999" {
		t.Fatalf("指定 owner 时必须归属它，got %s", other)
	}
}

// 对象键不泄露 rid、实例 ID 或业务信息，且格式固定。
func TestObjectKeyDoesNotLeakIdentity(t *testing.T) {
	id := testIdentity(t)
	rid, _ := id.RID("", "work", "k2")
	key := id.ObjectKey(rid)
	if !validObjectKey(key) {
		t.Fatalf("对象键格式不合法: %s", key)
	}
	if strings.Contains(key, "tst001") || strings.Contains(key, strings.Split(rid, "-")[1]) {
		t.Fatalf("对象键泄露了 rid 或实例 ID: %s", key)
	}
}

// 不同密钥派生出的键互不相同：泄露的 rid 无法在别的部署上算出键。
func TestDifferentSecretsDeriveDifferentKeys(t *testing.T) {
	a := testIdentity(t)
	b, _ := newIdentity("tst001", "another-identity-secret-0123456789abcdef")
	ra, _ := a.RID("", "work", "k3")
	rb, _ := b.RID("", "work", "k3")
	if a.ObjectKey(ra) == b.ObjectKey(rb) {
		t.Fatal("不同密钥必须得到不同对象键")
	}
}

// 实例 ID 与密钥的校验：不合法则拒绝构造。
func TestNewIdentityValidation(t *testing.T) {
	if _, err := newIdentity("ABC", testSecret); err == nil {
		t.Fatal("大写或长度不对的实例 ID 必须拒绝")
	}
	if _, err := newIdentity("tst001", "short"); err == nil {
		t.Fatal("过短的密钥必须拒绝")
	}
}

// 用途类别：只接受四类，路径格式固定。
func TestStorePathClasses(t *testing.T) {
	for _, c := range []string{"user", "work", "deliver", "asset"} {
		if !validClass(c) {
			t.Fatalf("%s 应该合法", c)
		}
	}
	if validClass("video") || validClass("") {
		t.Fatal("非法类别必须拒绝")
	}
	if got := storePath("asset", "abcdefghijklmnopqrstuvwxyz"); got != "v1/asset/abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("storePath = %s", got)
	}
}
