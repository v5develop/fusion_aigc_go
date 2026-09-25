package unionauth

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
)

// 联盟的加密方式：AES-CBC、IV 就是 key、PKCS7、外层 base64。
// 这里自己加密一遍再解，确保我们的解法和它对得上。
func encryptLikeUnion(plain, key string) string {
	block, _ := aes.NewCipher([]byte(key))
	size := block.BlockSize()
	pad := size - len(plain)%size
	data := []byte(plain)
	for i := 0; i < pad; i++ {
		data = append(data, byte(pad))
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, []byte(key)[:size]).CryptBlocks(out, data)
	return base64.StdEncoding.EncodeToString(out)
}

func TestDecryptPhone(t *testing.T) {
	const key = "1234567890ABCDEF"
	ct := encryptLikeUnion("13012347881", key)

	got, err := decryptAesCBC(ct, key)
	if err != nil || got != "13012347881" {
		t.Fatalf("解出来 %q, err = %v", got, err)
	}
	if m := maskPhone(got); m != "130****7881" {
		t.Fatalf("脱敏成 %q", m)
	}
}

// 密钥不对时必须报错，不能返回一段乱码当手机号
func TestDecryptWrongKey(t *testing.T) {
	ct := encryptLikeUnion("13012347881", "1234567890ABCDEF")
	if got, err := decryptAesCBC(ct, "FEDCBA0987654321"); err == nil {
		t.Fatalf("密钥不对却解出了 %q", got)
	}
	// 走到 phone() 里也应该是空串而不是乱码
	d := &direct{conf: DirectConf{PhoneAesKey: "FEDCBA0987654321"}}
	if p := d.phone(ct); p != "" {
		t.Fatalf("密钥不对应该返回空，实际 %q", p)
	}
	// 没配密钥也一样
	if p := (&direct{}).phone(ct); p != "" {
		t.Fatalf("没配密钥应该返回空，实际 %q", p)
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"13012347881": "130****7881",
		"":            "",
		"123":         "123", // 位数不对就原样返回，别瞎切
	}
	for in, want := range cases {
		if got := maskPhone(in); got != want {
			t.Errorf("maskPhone(%q) = %q，应该是 %q", in, got, want)
		}
	}
}

// key 格式必须和联盟一致，否则永远读不到
func TestTempTokenKey(t *testing.T) {
	if k := tempTokenKey("cf3301a6e2047b387514288af9d046c2"); k != "temp:token:cf3301a6e2047b387514288af9d046c2" {
		t.Fatalf("key = %q", k)
	}
}

// 没配 Redis 地址就不启用这条路（回落到 HTTP 接口）
func TestDirectDisabled(t *testing.T) {
	if newDirect(DirectConf{}, "dsn") != nil {
		t.Error("没配 Redis 不该启用")
	}
	if newDirect(DirectConf{RedisAddr: " "}, "dsn") != nil {
		t.Error("空白地址不该启用")
	}
	if newDirect(DirectConf{RedisAddr: "127.0.0.1:6379"}, "") != nil {
		t.Error("没有库可用时不该启用")
	}
	d := newDirect(DirectConf{RedisAddr: "127.0.0.1:6379"}, "user:pass@tcp(127.0.0.1:3306)/x")
	if d == nil || d.table != "unionec_app.uec_app_users" {
		t.Fatalf("默认表名不对: %+v", d)
	}
	c := New(Conf{Direct: DirectConf{RedisAddr: "127.0.0.1:6379"}}, "user:pass@tcp(127.0.0.1:3306)/x")
	if !c.Enabled() || !c.Direct() {
		t.Error("配了 Redis 就该启用并走自己验这条路")
	}
	// 只配 HTTP 地址时不走 direct
	if c2 := New(Conf{BaseURL: "http://x"}, ""); !c2.Enabled() || c2.Direct() {
		t.Error("只配地址时应该走 HTTP")
	}
}
